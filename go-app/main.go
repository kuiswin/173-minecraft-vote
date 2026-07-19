package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"cloud.google.com/go/bigtable"
	"cloud.google.com/go/pubsub"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/api/option"
)

// Config variables
var (
	projectID      = "local-project"
	instanceID     = "minecraft-instance"
	tableName      = "votes_timeseries"
	columnFamily   = "stats"
	topicID        = "minecraft-votes"
	subscriptionID = "minecraft-votes-sub"
	port           = "80"
)

// Structs
type VotePayload struct {
	Username  string    `json:"username"`
	Item      string    `json:"item"`
	Timestamp time.Time `json:"timestamp"`
}

type VoteRecord struct {
	Username  string `json:"username"`
	Item      string `json:"item"`
	Timestamp string `json:"timestamp"`
	RowKey    string `json:"row_key"`
}

// Metrics
var (
	votesCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "minecraft_votes_total",
			Help: "Total number of Minecraft votes processed and written to Bigtable.",
		},
		[]string{"item"},
	)
)

func init() {
	prometheus.MustRegister(votesCounter)
}

func main() {
	// Override configs from env if set
	if envProj := os.Getenv("GCP_PROJECT_ID"); envProj != "" {
		projectID = envProj
	}
	if envInst := os.Getenv("BIGTABLE_INSTANCE_ID"); envInst != "" {
		instanceID = envInst
	}

	log.Printf("Starting Minecraft Vote service. Project: %s, Instance: %s", projectID, instanceID)

	ctx := context.Background()

	// 1. Initialize Pub/Sub Topic and Subscription
	pubsubClient, err := pubsub.NewClient(ctx, projectID, option.WithoutAuthentication())
	if err != nil {
		log.Fatalf("Failed to create Pub/Sub client: %v", err)
	}
	defer pubsubClient.Close()

	topic := pubsubClient.Topic(topicID)
	exists, err := topic.Exists(ctx)
	if err != nil {
		log.Fatalf("Failed to check if topic exists: %v", err)
	}
	if !exists {
		log.Printf("Creating Pub/Sub topic: %s", topicID)
		topic, err = pubsubClient.CreateTopic(ctx, topicID)
		if err != nil {
			log.Fatalf("Failed to create topic: %v", err)
		}
	}

	sub := pubsubClient.Subscription(subscriptionID)
	subExists, err := sub.Exists(ctx)
	if err != nil {
		log.Fatalf("Failed to check if subscription exists: %v", err)
	}
	if !subExists {
		log.Printf("Creating Pub/Sub subscription: %s", subscriptionID)
		_, err = pubsubClient.CreateSubscription(ctx, subscriptionID, pubsub.SubscriptionConfig{
			Topic: topic,
		})
		if err != nil {
			log.Fatalf("Failed to create subscription: %v", err)
		}
	}

	// 2. Initialize Bigtable Table and Column Family
	adminClient, err := bigtable.NewAdminClient(ctx, projectID, instanceID, option.WithoutAuthentication())
	if err != nil {
		log.Fatalf("Failed to create Bigtable Admin client: %v", err)
	}
	defer adminClient.Close()

	tables, err := adminClient.Tables(ctx)
	if err != nil {
		log.Fatalf("Failed to list Bigtable tables: %v", err)
	}

	tableCreated := false
	for _, t := range tables {
		if t == tableName {
			tableCreated = true
			break
		}
	}

	if !tableCreated {
		log.Printf("Creating Bigtable table: %s", tableName)
		err = adminClient.CreateTable(ctx, tableName)
		if err != nil {
			log.Fatalf("Failed to create Bigtable table: %v", err)
		}
		log.Printf("Creating Column Family: %s", columnFamily)
		err = adminClient.CreateColumnFamily(ctx, tableName, columnFamily)
		if err != nil {
			log.Fatalf("Failed to create Column Family: %v", err)
		}
	}

	// Initialize Bigtable Client once and share it
	btClient, err := bigtable.NewClient(ctx, projectID, instanceID, option.WithoutAuthentication())
	if err != nil {
		log.Fatalf("Failed to create Bigtable Client: %v", err)
	}
	defer btClient.Close()

	workerCtx, cancelWorker := context.WithCancel(context.Background())
	defer cancelWorker()

	// Listen for termination signals for Graceful Shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigChan
		log.Printf("Received termination signal %v. Shutting down gracefully...", sig)
		cancelWorker()
		topic.Stop()
	}()

	// 3. Start Background Subscriber Worker
	go func() {
		log.Printf("Worker: Starting subscriber for %s", subscriptionID)
		tbl := btClient.Open(tableName)

		err = sub.Receive(workerCtx, func(ctx context.Context, msg *pubsub.Message) {
			log.Printf("Worker: Received vote message: %s", string(msg.Data))

			var vote VotePayload
			if err := json.Unmarshal(msg.Data, &vote); err != nil {
				log.Printf("Worker: Error unmarshalling vote payload: %v", err)
				msg.Ack()
				return
			}

			// Bigtable row key: <shard_id>#vote#<inverted_timestamp>#<username>
			// Prepend a random shard ID (00-49) to distribute writes and avoid hot spotting
			invertedTime := math.MaxInt64 - vote.Timestamp.UnixNano()
			shardID := rand.Intn(50)
			rowKey := fmt.Sprintf("%02d#vote#%019d#%s", shardID, invertedTime, vote.Username)

			// Create mutation
			mut := bigtable.NewMutation()
			mut.Set(columnFamily, "item", bigtable.Now(), []byte(vote.Item))
			mut.Set(columnFamily, "username", bigtable.Now(), []byte(vote.Username))
			mut.Set(columnFamily, "timestamp", bigtable.Now(), []byte(vote.Timestamp.Format(time.RFC3339)))

			// Write to Bigtable
			if err := tbl.Apply(ctx, rowKey, mut); err != nil {
				log.Printf("Worker: Error applying mutation to Bigtable: %v", err)
				msg.Nack()
				return
			}

			log.Printf("Worker: Successfully saved vote key %s to Bigtable.", rowKey)
			votesCounter.WithLabelValues(vote.Item).Inc()
			msg.Ack()
		})
		if err != nil && err != context.Canceled {
			log.Fatalf("Worker: Receive error: %v", err)
		}
	}()

	// 4. Start HTTP Server
	http.Handle("/metrics", promhttp.Handler())

	// Serve Static Files
	fs := http.FileServer(http.Dir("./public"))
	http.Handle("/", fs)

	// API to Vote (Publish message)
	http.HandleFunc("/api/vote", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}

		var req struct {
			Username string `json:"username"`
			Item     string `json:"item"`
		}

		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}

		if req.Username == "" || req.Item == "" {
			http.Error(w, "Username and Item are required", http.StatusBadRequest)
			return
		}

		// Validate items
		validItems := map[string]bool{"Diamond": true, "Emerald": true, "Netherite": true, "Gold": true}
		if !validItems[req.Item] {
			http.Error(w, "Invalid vote item", http.StatusBadRequest)
			return
		}

		payload := VotePayload{
			Username:  req.Username,
			Item:      req.Item,
			Timestamp: time.Now(),
		}

		payloadBytes, err := json.Marshal(payload)
		if err != nil {
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}

		// Publish to Pub/Sub
		result := topic.Publish(ctx, &pubsub.Message{
			Data: payloadBytes,
		})

		_, err = result.Get(ctx)
		if err != nil {
			log.Printf("API: Failed to publish message: %v", err)
			http.Error(w, "Failed to register vote in queue", http.StatusInternalServerError)
			return
		}

		log.Printf("API: Published vote from %s for %s", req.Username, req.Item)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte(`{"status":"queued"}`))
	})

	// API to Get recent votes from Bigtable
	http.HandleFunc("/api/votes", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}

		tbl := btClient.Open(tableName)

		var records []VoteRecord

		// Since keys are sharded/salted (e.g. 02#vote#...), read rows and filter/sort in memory. Limit to 50 rows on server side.
		err = tbl.ReadRows(ctx, bigtable.InfiniteRange(""), func(row bigtable.Row) bool {
			if !strings.Contains(row.Key(), "#vote#") {
				return true
			}
			record := VoteRecord{
				RowKey: row.Key(),
			}

			// Extract data from column family
			for _, items := range row[columnFamily] {
				switch items.Column {
				case "stats:item":
					record.Item = string(items.Value)
				case "stats:username":
					record.Username = string(items.Value)
				case "stats:timestamp":
					record.Timestamp = string(items.Value)
				}
			}

			records = append(records, record)
			return true
		}, bigtable.LimitRows(50))

		// Sort records descending by timestamp (newest first)
		sort.Slice(records, func(i, j int) bool {
			return records[i].Timestamp > records[j].Timestamp
		})

		// Limit to latest 50 records
		if len(records) > 50 {
			records = records[:50]
		}

		if err != nil {
			log.Printf("API: Failed to read from Bigtable: %v", err)
			http.Error(w, "Failed to read data", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(records)
	})

	log.Printf("Web server starting on port %s...", port)
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		log.Fatalf("Failed to start HTTP server: %v", err)
	}
}

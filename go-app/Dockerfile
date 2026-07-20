FROM golang:1.25.0-alpine AS builder
WORKDIR /app
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o main .
RUN GOBIN=/app/bin go install cloud.google.com/go/cbt@latest

FROM alpine:latest
WORKDIR /app
COPY --from=builder /app/main .
COPY --from=builder /app/public ./public
COPY --from=builder /app/bin/cbt /usr/bin/cbt
RUN echo "project = local-project" > /root/.cbtrc && \
    echo "instance = minecraft-instance" >> /root/.cbtrc
EXPOSE 80
CMD ["./main"]

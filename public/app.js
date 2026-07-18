document.addEventListener('DOMContentLoaded', () => {
    const usernameInput = document.getElementById('username');
    const voteButtons = document.querySelectorAll('.vote-btn');
    const refreshButton = document.getElementById('refresh-btn');
    const logsBody = document.getElementById('logs-body');
    const totalCountEl = document.getElementById('total-count');
    const notification = document.getElementById('notification');

    // Vote button action
    voteButtons.forEach(button => {
        button.addEventListener('click', async () => {
            const username = usernameInput.value.trim() || 'Anonymous';
            const item = button.getAttribute('data-item');

            if (!item) return;

            // Simple button pop animation
            button.style.transform = 'scale(0.95)';
            setTimeout(() => button.style.transform = '', 100);

            try {
                const response = await fetch('/api/vote', {
                    method: 'POST',
                    headers: {
                        'Content-Type': 'application/json',
                    },
                    body: JSON.stringify({ username, item }),
                });

                if (response.ok) {
                    showNotification(`Submitted choice for ${item}! Processing via Pub/Sub queue...`, '#34d399');
                    // Fetch logs after a brief delay for Pub/Sub and Bigtable processing latency
                    setTimeout(fetchVotesAndStats, 1000);
                } else {
                    const text = await response.text();
                    showNotification(`Failed to submit: ${text}`, '#f87171');
                }
            } catch (err) {
                console.error(err);
                showNotification('Network error submitting answer', '#f87171');
            }
        });
    });

    // Refresh action
    refreshButton.addEventListener('click', () => {
        refreshButton.style.transform = 'rotate(180deg)';
        setTimeout(() => refreshButton.style.transform = '', 300);
        fetchVotesAndStats();
    });

    // Helper: Show custom toast notification
    function showNotification(message, borderHex) {
        notification.textContent = message;
        notification.style.borderColor = borderHex || '#38bdf8';
        notification.classList.add('show');
        setTimeout(() => {
            notification.classList.remove('show');
        }, 3500);
    }

    // Helper: Fetch and render data
    async function fetchVotesAndStats() {
        try {
            const response = await fetch('/api/votes');
            if (!response.ok) throw new Error('Failed to fetch logs');

            const data = await response.json();
            renderLogsAndStats(data || []);
        } catch (err) {
            console.error(err);
            logsBody.innerHTML = `<tr><td colspan="4" class="no-data" style="color: #f87171;">⚠️ データの読み込みに失敗しました</td></tr>`;
        }
    }

    // Helper: Render elements
    function renderLogsAndStats(records) {
        // 1. Render logs table
        if (records.length === 0) {
            logsBody.innerHTML = `<tr><td colspan="4" class="no-data">Bigtable にまだデータが書き込まれていません。</td></tr>`;
            totalCountEl.textContent = '0';
            updateBars({ 'Diamond': 0, 'Emerald': 0, 'Netherite': 0, 'Gold': 0 }, 0);
            return;
        }

        let html = '';
        const counts = { 'Diamond': 0, 'Emerald': 0, 'Netherite': 0, 'Gold': 0 };

        records.forEach(row => {
            counts[row.item] = (counts[row.item] || 0) + 1;

            // Format Timestamp
            let dateStr = row.timestamp;
            try {
                const date = new Date(row.timestamp);
                dateStr = date.toLocaleString();
            } catch (e) {}

            html += `
                <tr>
                    <td><code>${row.row_key}</code></td>
                    <td><strong>${escapeHtml(row.username)}</strong></td>
                    <td><span class="badge-item badge-${row.item.toLowerCase()}">${row.item}</span></td>
                    <td>${dateStr}</td>
                </tr>
            `;
        });

        logsBody.innerHTML = html;

        // 2. Render counts and update UI bars
        const totalVotes = records.length;
        totalCountEl.textContent = totalVotes;

        updateBars(counts, totalVotes);
    }

    function updateBars(counts, total) {
        const items = ['Diamond', 'Emerald', 'Netherite', 'Gold'];
        items.forEach(item => {
            const count = counts[item] || 0;
            const percentage = total > 0 ? (count / total) * 100 : 0;

            // Update counts text
            document.getElementById(`count-${item}`).textContent = `${count}件`;

            // Update progress bars
            const bar = document.getElementById(`bar-${item}`);
            bar.style.width = `${percentage}%`;
        });
    }

    function escapeHtml(str) {
        return str.replace(/&/g, '&amp;')
                  .replace(/</g, '&lt;')
                  .replace(/>/g, '&gt;')
                  .replace(/"/g, '&quot;')
                  .replace(/'/g, '&#039;');
    }

    // Initial load
    fetchVotesAndStats();

    // Poll logs every 5 seconds
    setInterval(fetchVotesAndStats, 5000);
});

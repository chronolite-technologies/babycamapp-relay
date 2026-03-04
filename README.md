# BabyCam Signaling Relay

Minimal HTTP relay service for cross-network WebRTC reconnect signaling. Acts as an ephemeral mailbox for encrypted SDP exchange when local discovery (NSD/mDNS) fails — e.g., when one device is on WiFi and the other on cellular.

## How It Works

Two previously paired devices derive the same room ID from their shared secret. They exchange AES-256-GCM encrypted SDP blobs through this relay, then establish a direct WebRTC connection.

```
Parent                          Relay                           Baby
   │  PUT /offer (encrypted)      │                               │
   │─────────────────────────────▶│                               │
   │                              │    GET /offer (polling)       │
   │                              │◀──────────────────────────────│
   │                              │         200 + body            │
   │                              │──────────────────────────────▶│
   │                              │    PUT /answer (encrypted)    │
   │                              │◀──────────────────────────────│
   │  GET /answer (polling)       │                               │
   │─────────────────────────────▶│                               │
   │           200 + body         │                               │
   │◀─────────────────────────────│                               │
   │                                                              │
   └──────── WebRTC connects directly (STUN/TURN) ───────────────┘
```

The relay server is a "blind mailbox" — it only stores opaque encrypted blobs and cannot read or modify the SDP data.

## API

| Method | Endpoint | Description | Response |
|--------|----------|-------------|----------|
| `PUT` | `/v1/signal/{roomId}/offer` | Store encrypted SDP offer | `201` |
| `GET` | `/v1/signal/{roomId}/offer` | Retrieve SDP offer | `200` + body / `204` |
| `PUT` | `/v1/signal/{roomId}/answer` | Store encrypted SDP answer | `201` |
| `GET` | `/v1/signal/{roomId}/answer` | Retrieve SDP answer | `200` + body / `204` |
| `GET` | `/health` | Health check | `200 "ok"` |

**Error responses:** `400` (invalid input), `405` (wrong method), `413` (body > 16 KB), `429` (rate limited)

**Constraints:**
- `roomId`: exactly 32 lowercase hex characters
- `slot`: `"offer"` or `"answer"` only
- Max payload: 16 KB
- Room TTL: 120 seconds (auto-deleted)
- Rate limit: 10 requests/minute/IP

## Build & Run

```bash
# Run tests
go test -v ./...

# Build
go build -o babycam-relay .

# Run (defaults to port 8080)
./babycam-relay

# Cross-compile for Linux (deployment)
GOOS=linux GOARCH=amd64 go build -o babycam-relay .
```

## Configuration

All configuration via environment variables:

| Variable | Default | Description |
|----------|---------|-------------|
| `RELAY_PORT` | `8080` | Listen port |
| `RELAY_TTL` | `120` | Room TTL in seconds |
| `RELAY_MAX_BODY` | `16384` | Max payload in bytes |
| `RELAY_RATE_LIMIT` | `10` | Requests per minute per IP |

## Deployment

The service listens on plain HTTP — TLS termination is handled by the ingress proxy.

A reference systemd unit file is provided in `systemd/babycam-relay.service`.

```bash
# Copy binary
scp babycam-relay user@server:/opt/babycam-relay/

# Install and start service
sudo cp systemd/babycam-relay.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now babycam-relay
```

## Security

- **No authentication needed** — room IDs are 128-bit hashes derived from shared secrets (brute-force resistant)
- **End-to-end encrypted** — server only sees AES-256-GCM ciphertext
- **Ephemeral** — all data auto-deleted after 120s
- **Rate limited** — 10 req/min/IP, `X-Forwarded-For` trusted only from loopback
- **No logging of room contents** — only metadata (room ID prefix, size, status code)
- **Zero external dependencies** — Go standard library only

## Tech Stack

- Go 1.22+
- No external dependencies
- `net/http` for HTTP server
- `sync.RWMutex` for concurrent access
- `net/http/httptest` for testing

## License

[MIT](LICENSE)

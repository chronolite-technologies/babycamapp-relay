package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// --- Configuration (from environment variables) ---

type Config struct {
	Port      string
	TTL       time.Duration
	MaxBody   int64
	RateLimit float64 // requests per minute per IP
	MaxRooms  int     // max concurrent rooms
}

func loadConfig() Config {
	return Config{
		Port:      envOrDefault("RELAY_PORT", "8080"),
		TTL:       time.Duration(envIntOrDefault("RELAY_TTL", 120)) * time.Second,
		MaxBody:   int64(envIntOrDefault("RELAY_MAX_BODY", 16384)),
		RateLimit: float64(envIntOrDefault("RELAY_RATE_LIMIT", 10)),
		MaxRooms:  envIntOrDefault("RELAY_MAX_ROOMS", 128),
	}
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envIntOrDefault(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

// --- Data Structures ---

// Slot holds an encrypted SDP blob with its creation timestamp.
type Slot struct {
	Data      []byte
	CreatedAt time.Time
}

// Room holds offer and answer slots for a signaling session.
type Room struct {
	Offer  *Slot
	Answer *Slot
}

// clientRate tracks token bucket state for a single IP.
type clientRate struct {
	tokens    float64
	lastCheck time.Time
}

// RateLimiter implements a per-IP token bucket rate limiter.
type RateLimiter struct {
	mu        sync.Mutex
	clients   map[string]*clientRate
	ratePerSec float64 // tokens added per second
	burst     float64 // max tokens (bucket size)
}

// NewRateLimiter creates a rate limiter with the given requests-per-minute limit.
func NewRateLimiter(requestsPerMinute float64) *RateLimiter {
	return &RateLimiter{
		clients:    make(map[string]*clientRate),
		ratePerSec: requestsPerMinute / 60.0,
		burst:      requestsPerMinute, // burst = full minute of tokens
	}
}

// Allow checks if a request from the given IP is allowed.
func (rl *RateLimiter) Allow(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	cr, exists := rl.clients[ip]
	if !exists {
		rl.clients[ip] = &clientRate{tokens: rl.burst - 1, lastCheck: now}
		return true
	}

	// Add tokens based on elapsed time
	elapsed := now.Sub(cr.lastCheck).Seconds()
	cr.tokens += elapsed * rl.ratePerSec
	if cr.tokens > rl.burst {
		cr.tokens = rl.burst
	}
	cr.lastCheck = now

	if cr.tokens >= 1 {
		cr.tokens--
		return true
	}
	return false
}

// Cleanup removes stale IP entries (inactive for >5 minutes).
func (rl *RateLimiter) Cleanup() {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	cutoff := time.Now().Add(-5 * time.Minute)
	for ip, cr := range rl.clients {
		if cr.lastCheck.Before(cutoff) {
			delete(rl.clients, ip)
		}
	}
}

// --- Server ---

var roomIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// Server is the signaling relay HTTP server.
type Server struct {
	mu      sync.RWMutex
	rooms   map[string]*Room
	config  Config
	limiter *RateLimiter
	now     func() time.Time // injectable clock for testing
}

// NewServer creates a new relay server with the given configuration.
func NewServer(cfg Config) *Server {
	return &Server{
		rooms:   make(map[string]*Room),
		config:  cfg,
		limiter: NewRateLimiter(cfg.RateLimit),
		now:     time.Now,
	}
}

// ServeHTTP implements http.Handler — routes requests to the appropriate handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Health endpoint
	if r.URL.Path == "/health" {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")
		return
	}

	// Rate limiting
	ip := clientIP(r)
	if !s.limiter.Allow(ip) {
		log.Printf("RATE_LIMIT ip=%s", ip)
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}

	// Parse path: /v1/signal/{roomId}/{slot}
	roomID, slot, ok := parsePath(r.URL.Path)
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	// Validate roomId
	if !roomIDPattern.MatchString(roomID) {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	// Validate slot
	if slot != "offer" && slot != "answer" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	switch r.Method {
	case http.MethodPut:
		s.handlePut(w, r, roomID, slot)
	case http.MethodGet:
		s.handleGet(w, r, roomID, slot)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// handlePut stores encrypted SDP data in the specified slot.
func (s *Server) handlePut(w http.ResponseWriter, r *http.Request, roomID, slot string) {
	// Read body with size limit (maxBody + 1 to detect oversize)
	limited := http.MaxBytesReader(w, r.Body, s.config.MaxBody+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		return
	}
	if int64(len(body)) > s.config.MaxBody {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		return
	}
	if len(body) == 0 {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	room, exists := s.rooms[roomID]
	if !exists {
		// Reject if max rooms reached
		if len(s.rooms) >= s.config.MaxRooms {
			s.mu.Unlock()
			log.Printf("MAX_ROOMS roomId=%s… limit=%d", roomID[:8], s.config.MaxRooms)
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		room = &Room{}
		s.rooms[roomID] = room
	}
	entry := &Slot{Data: body, CreatedAt: s.now()}
	if slot == "offer" {
		room.Offer = entry
	} else {
		room.Answer = entry
	}
	s.mu.Unlock()

	log.Printf("PUT %s roomId=%s… size=%d status=201", slot, roomID[:8], len(body))
	w.WriteHeader(http.StatusCreated)
}

// handleGet retrieves encrypted SDP data from the specified slot and deletes it immediately.
func (s *Server) handleGet(w http.ResponseWriter, _ *http.Request, roomID, slot string) {
	s.mu.Lock()
	room, exists := s.rooms[roomID]
	var entry *Slot
	if exists {
		if slot == "offer" {
			entry = room.Offer
			room.Offer = nil
		} else {
			entry = room.Answer
			room.Answer = nil
		}
		// Remove room if both slots are now empty
		if room.Offer == nil && room.Answer == nil {
			delete(s.rooms, roomID)
		}
	}
	s.mu.Unlock()

	if entry == nil {
		log.Printf("GET %s roomId=%s… status=204", slot, roomID[:8])
		w.WriteHeader(http.StatusNoContent)
		return
	}

	log.Printf("GET %s roomId=%s… size=%d status=200", slot, roomID[:8], len(entry.Data))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	w.Write(entry.Data)
}

// cleanup removes expired rooms and stale rate limiter entries.
func (s *Server) cleanup() {
	s.mu.Lock()
	now := s.now()
	removed := 0
	for id, room := range s.rooms {
		offerExpired := room.Offer == nil || now.Sub(room.Offer.CreatedAt) > s.config.TTL
		answerExpired := room.Answer == nil || now.Sub(room.Answer.CreatedAt) > s.config.TTL

		// Remove individual expired slots
		if room.Offer != nil && now.Sub(room.Offer.CreatedAt) > s.config.TTL {
			room.Offer = nil
		}
		if room.Answer != nil && now.Sub(room.Answer.CreatedAt) > s.config.TTL {
			room.Answer = nil
		}

		// Remove room if both slots are empty/expired
		if offerExpired && answerExpired {
			delete(s.rooms, id)
			removed++
		}
	}
	remaining := len(s.rooms)
	s.mu.Unlock()

	s.limiter.Cleanup()

	if removed > 0 {
		log.Printf("CLEANUP rooms_removed=%d rooms_remaining=%d", removed, remaining)
	}
}

// startCleanup runs the cleanup loop every 30 seconds until ctx is cancelled.
func (s *Server) startCleanup(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.cleanup()
		}
	}
}

// --- Helpers ---

// parsePath extracts roomId and slot from a path like /v1/signal/{roomId}/{slot}.
func parsePath(path string) (roomID, slot string, ok bool) {
	// Trim leading slash, split into segments
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	// Expected: ["v1", "signal", "{roomId}", "{slot}"]
	if len(parts) != 4 || parts[0] != "v1" || parts[1] != "signal" {
		return "", "", false
	}
	return parts[2], parts[3], true
}

// clientIP extracts the client IP, trusting X-Forwarded-For only from local proxies.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	// Only trust X-Forwarded-For from loopback (ingress on same host)
	if host == "127.0.0.1" || host == "::1" {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if idx := strings.IndexByte(xff, ','); idx != -1 {
				return strings.TrimSpace(xff[:idx])
			}
			return strings.TrimSpace(xff)
		}
	}
	return host
}

// --- Main ---

func main() {
	cfg := loadConfig()
	srv := NewServer(cfg)

	// Graceful shutdown context
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Start background cleanup
	go srv.startCleanup(ctx)

	httpServer := &http.Server{
		Addr:         ":" + cfg.Port,
		Handler:      srv,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Start server in goroutine
	go func() {
		log.Printf("START port=%s ttl=%s max_body=%d rate_limit=%.0f/min",
			cfg.Port, cfg.TTL, cfg.MaxBody, cfg.RateLimit)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("FATAL %v", err)
		}
	}()

	// Wait for shutdown signal
	<-ctx.Done()
	log.Println("SHUTDOWN signal received, draining connections...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("SHUTDOWN error: %v", err)
	}
	log.Println("SHUTDOWN complete")
}

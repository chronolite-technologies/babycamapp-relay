package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// validRoomID is a 32-char hex string for testing.
const validRoomID = "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4"

func testServer() *Server {
	cfg := Config{
		Port:      "8080",
		TTL:       120 * time.Second,
		MaxBody:   16384,
		RateLimit: 100, // high limit so rate limiting doesn't interfere with most tests
		MaxRooms:  128,
	}
	return NewServer(cfg)
}

func putSlot(t *testing.T, srv *Server, roomID, slot string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/v1/signal/"+roomID+"/"+slot, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/octet-stream")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	return w
}

func getSlot(t *testing.T, srv *Server, roomID, slot string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/signal/"+roomID+"/"+slot, nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	return w
}

func TestPutAndGetOffer(t *testing.T) {
	srv := testServer()
	payload := []byte("encrypted-sdp-offer-data")

	// PUT offer
	w := putSlot(t, srv, validRoomID, "offer", payload)
	if w.Code != http.StatusCreated {
		t.Fatalf("PUT offer: expected 201, got %d", w.Code)
	}

	// GET offer
	w = getSlot(t, srv, validRoomID, "offer")
	if w.Code != http.StatusOK {
		t.Fatalf("GET offer: expected 200, got %d", w.Code)
	}
	if !bytes.Equal(w.Body.Bytes(), payload) {
		t.Fatalf("GET offer: body mismatch")
	}
}

func TestGetEmptySlot(t *testing.T) {
	srv := testServer()

	w := getSlot(t, srv, validRoomID, "offer")
	if w.Code != http.StatusNoContent {
		t.Fatalf("GET empty: expected 204, got %d", w.Code)
	}
}

func TestGetDeletesSlotAfterRetrieval(t *testing.T) {
	srv := testServer()
	payload := []byte("one-time-sdp-data")

	putSlot(t, srv, validRoomID, "offer", payload)

	// First GET returns data
	w := getSlot(t, srv, validRoomID, "offer")
	if w.Code != http.StatusOK {
		t.Fatalf("first GET: expected 200, got %d", w.Code)
	}
	if !bytes.Equal(w.Body.Bytes(), payload) {
		t.Fatalf("first GET: body mismatch")
	}

	// Second GET returns 204 — data was deleted after first retrieval
	w = getSlot(t, srv, validRoomID, "offer")
	if w.Code != http.StatusNoContent {
		t.Fatalf("second GET: expected 204 (deleted after retrieval), got %d", w.Code)
	}
}

func TestGetDeletesRoomWhenBothSlotsRetrieved(t *testing.T) {
	srv := testServer()

	putSlot(t, srv, validRoomID, "offer", []byte("offer-data"))
	putSlot(t, srv, validRoomID, "answer", []byte("answer-data"))

	// Retrieve both slots
	getSlot(t, srv, validRoomID, "offer")
	getSlot(t, srv, validRoomID, "answer")

	// Room should be completely removed from memory
	srv.mu.RLock()
	_, exists := srv.rooms[validRoomID]
	srv.mu.RUnlock()
	if exists {
		t.Fatal("room should have been deleted after both slots were retrieved")
	}
}

func TestPutAndGetAnswer(t *testing.T) {
	srv := testServer()
	payload := []byte("encrypted-sdp-answer-data")

	w := putSlot(t, srv, validRoomID, "answer", payload)
	if w.Code != http.StatusCreated {
		t.Fatalf("PUT answer: expected 201, got %d", w.Code)
	}

	w = getSlot(t, srv, validRoomID, "answer")
	if w.Code != http.StatusOK {
		t.Fatalf("GET answer: expected 200, got %d", w.Code)
	}
	if !bytes.Equal(w.Body.Bytes(), payload) {
		t.Fatalf("GET answer: body mismatch")
	}
}

func TestOverwriteSlot(t *testing.T) {
	srv := testServer()
	first := []byte("first-offer")
	second := []byte("second-offer-overwritten")

	putSlot(t, srv, validRoomID, "offer", first)
	putSlot(t, srv, validRoomID, "offer", second)

	w := getSlot(t, srv, validRoomID, "offer")
	if w.Code != http.StatusOK {
		t.Fatalf("GET overwritten: expected 200, got %d", w.Code)
	}
	if !bytes.Equal(w.Body.Bytes(), second) {
		t.Fatalf("GET overwritten: expected second payload, got %q", w.Body.String())
	}
}

func TestPayloadTooLarge(t *testing.T) {
	srv := testServer()
	// Create payload larger than max (16384 + 100)
	bigPayload := make([]byte, 16484)
	for i := range bigPayload {
		bigPayload[i] = 0xAB
	}

	w := putSlot(t, srv, validRoomID, "offer", bigPayload)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("PUT too large: expected 413, got %d", w.Code)
	}
}

func TestInvalidRoomId(t *testing.T) {
	srv := testServer()
	payload := []byte("test")

	tests := []struct {
		name   string
		roomID string
	}{
		{"too short", "a1b2c3d4"},
		{"too long", "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4ff"},
		{"uppercase", "A1B2C3D4E5F6A1B2C3D4E5F6A1B2C3D4"},
		{"non-hex", "g1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := putSlot(t, srv, tt.roomID, "offer", payload)
			if w.Code != http.StatusBadRequest && w.Code != http.StatusNotFound {
				t.Fatalf("PUT invalid roomId %q: expected 400/404, got %d", tt.roomID, w.Code)
			}
		})
	}
}

func TestInvalidSlot(t *testing.T) {
	srv := testServer()
	payload := []byte("test")

	tests := []string{"sdp", "data", "foo", "OFFER", "Answer"}
	for _, slot := range tests {
		t.Run(slot, func(t *testing.T) {
			w := putSlot(t, srv, validRoomID, slot, payload)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("PUT invalid slot %q: expected 400, got %d", slot, w.Code)
			}
		})
	}
}

func TestMethodNotAllowed(t *testing.T) {
	srv := testServer()
	methods := []string{http.MethodPost, http.MethodDelete, http.MethodPatch}

	for _, method := range methods {
		t.Run(method, func(t *testing.T) {
			req := httptest.NewRequest(method, "/v1/signal/"+validRoomID+"/offer", nil)
			w := httptest.NewRecorder()
			srv.ServeHTTP(w, req)
			if w.Code != http.StatusMethodNotAllowed {
				t.Fatalf("%s: expected 405, got %d", method, w.Code)
			}
		})
	}
}

func TestRoomTTLExpiry(t *testing.T) {
	srv := testServer()
	srv.config.TTL = 50 * time.Millisecond // very short TTL for testing

	// Use a controllable clock
	now := time.Now()
	srv.now = func() time.Time { return now }

	payload := []byte("will-expire")
	putSlot(t, srv, validRoomID, "offer", payload)

	// Verify it exists
	w := getSlot(t, srv, validRoomID, "offer")
	if w.Code != http.StatusOK {
		t.Fatalf("GET before expiry: expected 200, got %d", w.Code)
	}

	// Advance time past TTL
	now = now.Add(200 * time.Millisecond)

	// Run cleanup
	srv.cleanup()

	// Verify it's gone via GET
	w = getSlot(t, srv, validRoomID, "offer")
	if w.Code != http.StatusNoContent {
		t.Fatalf("GET after expiry: expected 204, got %d", w.Code)
	}

	// Verify the room is actually deleted from memory (prevents memory leaks)
	srv.mu.RLock()
	_, roomExists := srv.rooms[validRoomID]
	srv.mu.RUnlock()
	if roomExists {
		t.Fatal("room should have been deleted from map after expiry")
	}
}

func TestHealthEndpoint(t *testing.T) {
	srv := testServer()
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("health: expected 200, got %d", w.Code)
	}
	body, _ := io.ReadAll(w.Body)
	if string(body) != "ok" {
		t.Fatalf("health: expected 'ok', got %q", string(body))
	}
}

func TestRateLimiting(t *testing.T) {
	cfg := Config{
		Port:      "8080",
		TTL:       120 * time.Second,
		MaxBody:   16384,
		RateLimit: 5, // 5 requests per minute for easy testing
		MaxRooms:  128,
	}
	srv := NewServer(cfg)
	payload := []byte("test-rate-limit")

	// First 5 requests should succeed (burst = 5)
	for i := 0; i < 5; i++ {
		w := putSlot(t, srv, validRoomID, "offer", payload)
		if w.Code == http.StatusTooManyRequests {
			t.Fatalf("request %d: got 429 too early", i+1)
		}
	}

	// 6th request should be rate limited
	w := putSlot(t, srv, validRoomID, "offer", payload)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("request 6: expected 429, got %d", w.Code)
	}
}

func TestIndependentRooms(t *testing.T) {
	srv := testServer()
	roomA := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa1"
	roomB := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb2"
	payloadA := []byte("offer-for-room-a")
	payloadB := []byte("offer-for-room-b")

	putSlot(t, srv, roomA, "offer", payloadA)
	putSlot(t, srv, roomB, "offer", payloadB)

	// Room A returns A's data
	w := getSlot(t, srv, roomA, "offer")
	if !bytes.Equal(w.Body.Bytes(), payloadA) {
		t.Fatalf("room A: expected payloadA, got %q", w.Body.String())
	}

	// Room B returns B's data
	w = getSlot(t, srv, roomB, "offer")
	if !bytes.Equal(w.Body.Bytes(), payloadB) {
		t.Fatalf("room B: expected payloadB, got %q", w.Body.String())
	}

	// Room A answer is empty
	w = getSlot(t, srv, roomA, "answer")
	if w.Code != http.StatusNoContent {
		t.Fatalf("room A answer: expected 204, got %d", w.Code)
	}
}

func TestMaxRoomsLimit(t *testing.T) {
	cfg := Config{
		Port:      "8080",
		TTL:       120 * time.Second,
		MaxBody:   16384,
		RateLimit: 1000,
		MaxRooms:  3,
	}
	srv := NewServer(cfg)
	payload := []byte("test")

	// Fill up to max rooms
	putSlot(t, srv, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa1", "offer", payload)
	putSlot(t, srv, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb2", "offer", payload)
	putSlot(t, srv, "ccccccccccccccccccccccccccccccc3", "offer", payload)

	// 4th room should be rejected with 503
	w := putSlot(t, srv, "ddddddddddddddddddddddddddddddd4", "offer", payload)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("4th room: expected 503, got %d", w.Code)
	}

	// PUT to existing room should still work
	w = putSlot(t, srv, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa1", "answer", payload)
	if w.Code != http.StatusCreated {
		t.Fatalf("existing room: expected 201, got %d", w.Code)
	}

	// After retrieving both slots from room A (auto-deletes), a new room should be allowed
	getSlot(t, srv, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa1", "offer")
	getSlot(t, srv, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa1", "answer")

	w = putSlot(t, srv, "ddddddddddddddddddddddddddddddd4", "offer", payload)
	if w.Code != http.StatusCreated {
		t.Fatalf("after room freed: expected 201, got %d", w.Code)
	}
}

func TestEmptyPutBody(t *testing.T) {
	srv := testServer()

	req := httptest.NewRequest(http.MethodPut, "/v1/signal/"+validRoomID+"/offer", strings.NewReader(""))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("PUT empty body: expected 400, got %d", w.Code)
	}
}

func TestNotFoundPaths(t *testing.T) {
	srv := testServer()
	paths := []string{
		"/",
		"/v1",
		"/v1/signal",
		"/v1/signal/" + validRoomID,
		"/v2/signal/" + validRoomID + "/offer",
		"/v1/other/" + validRoomID + "/offer",
	}

	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			w := httptest.NewRecorder()
			srv.ServeHTTP(w, req)
			if w.Code != http.StatusNotFound {
				t.Fatalf("GET %s: expected 404, got %d", path, w.Code)
			}
		})
	}
}

func TestXForwardedForFromTrustedProxy(t *testing.T) {
	cfg := Config{
		Port:      "8080",
		TTL:       120 * time.Second,
		MaxBody:   16384,
		RateLimit: 3,
		MaxRooms:  128,
	}
	srv := NewServer(cfg)
	payload := []byte("test")

	// Requests from loopback (trusted proxy) — XFF is used for rate limiting
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodPut, "/v1/signal/"+validRoomID+"/offer", bytes.NewReader(payload))
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("X-Forwarded-For", "1.2.3.4")
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)
	}

	// 4th request from same forwarded IP should be rate limited
	req := httptest.NewRequest(http.MethodPut, "/v1/signal/"+validRoomID+"/offer", bytes.NewReader(payload))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("same forwarded IP: expected 429, got %d", w.Code)
	}

	// Different forwarded IP should succeed
	req = httptest.NewRequest(http.MethodPut, "/v1/signal/"+validRoomID+"/offer", bytes.NewReader(payload))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("X-Forwarded-For", "5.6.7.8")
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code == http.StatusTooManyRequests {
		t.Fatalf("different forwarded IP: got 429 unexpectedly")
	}
}

func TestXForwardedForFromUntrustedClient(t *testing.T) {
	cfg := Config{
		Port:      "8080",
		TTL:       120 * time.Second,
		MaxBody:   16384,
		RateLimit: 3,
		MaxRooms:  128,
	}
	srv := NewServer(cfg)
	payload := []byte("test")

	// Requests from external IP — XFF should be IGNORED, RemoteAddr used instead
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodPut, "/v1/signal/"+validRoomID+"/offer", bytes.NewReader(payload))
		req.RemoteAddr = "10.0.0.1:12345"
		req.Header.Set("X-Forwarded-For", fmt.Sprintf("fake-%d.0.0.1", i))
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)
	}

	// 4th request — despite different XFF, RemoteAddr is the same → should be rate limited
	req := httptest.NewRequest(http.MethodPut, "/v1/signal/"+validRoomID+"/offer", bytes.NewReader(payload))
	req.RemoteAddr = "10.0.0.1:12345"
	req.Header.Set("X-Forwarded-For", "totally-different-ip")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("spoofed XFF should be ignored: expected 429, got %d", w.Code)
	}
}

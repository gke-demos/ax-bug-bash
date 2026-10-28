package observer

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// SSEEvent represents a Server-Sent Event broadcast to connected AX Observer clients.
type SSEEvent struct {
	Event     string    `json:"event"`
	Data      any       `json:"data"`
	Timestamp time.Time `json:"timestamp"`
}

// SSEBroker manages real-time Server-Sent Events subscriptions for GET /api/events.
type SSEBroker struct {
	mu           sync.RWMutex
	clients      map[chan SSEEvent]struct{}
	initialState func() any
}

// NewSSEBroker creates a new SSEBroker.
func NewSSEBroker(initialState func() any) *SSEBroker {
	return &SSEBroker{
		clients:      make(map[chan SSEEvent]struct{}),
		initialState: initialState,
	}
}

// Broadcast sends an SSE event to all connected clients without blocking.
func (b *SSEBroker) Broadcast(eventName string, payload any) {
	evt := SSEEvent{
		Event:     eventName,
		Data:      payload,
		Timestamp: time.Now().UTC(),
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	for ch := range b.clients {
		select {
		case ch <- evt:
		default:
		}
	}
}

// ServeHTTP handles GET /api/events as an EventSource text/event-stream endpoint.
func (b *SSEBroker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ch := make(chan SSEEvent, 32)
	b.mu.Lock()
	b.clients[ch] = struct{}{}
	b.mu.Unlock()

	defer func() {
		b.mu.Lock()
		delete(b.clients, ch)
		close(ch)
		b.mu.Unlock()
	}()

	// Emit initial state.snapshot event immediately on connection
	if b.initialState != nil {
		writeSSEFrame(w, flusher, "state.snapshot", b.initialState())
	}

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case evt, open := <-ch:
			if !open {
				return
			}
			writeSSEFrame(w, flusher, evt.Event, evt.Data)
		}
	}
}

func writeSSEFrame(w http.ResponseWriter, flusher http.Flusher, eventName string, data any) {
	raw, err := json.Marshal(data)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventName, string(raw))
	flusher.Flush()
}

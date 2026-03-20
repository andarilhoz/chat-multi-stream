package server

import (
	"context"
	"encoding/json"
	_ "embed"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/magnogouveia/chat-multi-stream/internal/domain"
)

//go:embed static/overlay.html
var overlayHTML []byte

// Server owns the HTTP server and the WebSocket broadcast hub.
type Server struct {
	httpServer *http.Server
	messages   <-chan domain.ChatMessage
	hub        *hub
}

// New creates a Server that reads from messages and broadcasts to all connected
// WebSocket clients. addr should be the listen address (e.g. ":8080").
func New(addr string, messages <-chan domain.ChatMessage) *Server {
	s := &Server{
		messages: messages,
		hub:      newHub(),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/ws",      s.handleWS)
	mux.HandleFunc("/overlay", s.handleOverlay)
	mux.HandleFunc("/health",  s.handleHealth)

	s.httpServer = &http.Server{
		Addr:    addr,
		Handler: mux,
	}
	return s
}

// Run starts the broadcaster goroutine and the HTTP server.
// It blocks until ctx is cancelled, then performs a graceful shutdown.
func (s *Server) Run(ctx context.Context) error {
	go s.broadcaster(ctx)

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.httpServer.Shutdown(shutdownCtx); err != nil {
			log.Printf("[server] shutdown error: %v", err)
		}
	}()

	if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// broadcaster reads from the aggregated message channel and fans the messages
// out to every connected WebSocket client.
func (s *Server) broadcaster(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-s.messages:
			if !ok {
				return // channel closed (aggregator done)
			}
			s.hub.broadcast(msg)
		}
	}
}

// handleWS upgrades an HTTP connection to WebSocket and registers the client
// with the hub. It then loops writing queued messages until the client
// disconnects or ctx is cancelled.
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	// InsecureSkipVerify allows connections from OBS Browser Source, which may
	// send a null or different Origin header. This server is intended for local
	// use only — do not expose it publicly without adding authentication.
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
	})
	if err != nil {
		log.Printf("[server] websocket accept: %v", err)
		return
	}

	c := &client{
		conn: conn,
		send: make(chan domain.ChatMessage, 32),
	}
	s.hub.register(c)
	defer s.hub.unregister(c)

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			conn.Close(websocket.StatusNormalClosure, "")
			return
		case msg, ok := <-c.send:
			if !ok {
				return // hub unregistered this client
			}
			data, err := json.Marshal(msg)
			if err != nil {
				log.Printf("[server] marshal: %v", err)
				continue
			}
			if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
				log.Printf("[server] write: %v", err)
				return
			}
		}
	}
}

// handleOverlay serves the embedded OBS Browser Source overlay page.
func (s *Server) handleOverlay(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(overlayHTML)
}

// handleHealth returns a simple JSON health check.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// ── Hub ───────────────────────────────────────────────────────────────────────

// hub manages the set of active WebSocket clients and routes broadcast messages.
type hub struct {
	mu      sync.RWMutex
	clients map[*client]struct{}
}

type client struct {
	conn *websocket.Conn
	send chan domain.ChatMessage
}

func newHub() *hub {
	return &hub{clients: make(map[*client]struct{})}
}

func (h *hub) register(c *client) {
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
}

// unregister removes the client from the hub and closes its send channel.
// It is idempotent — safe to call more than once for the same client.
func (h *hub) unregister(c *client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.clients[c]; ok {
		delete(h.clients, c)
		close(c.send)
	}
}

// broadcast delivers a message to every registered client.
// Slow clients (full send buffer) have the message dropped rather than blocking.
func (h *hub) broadcast(msg domain.ChatMessage) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.clients {
		select {
		case c.send <- msg:
		default:
			// Drop message for slow consumer — prefer stream continuity over
			// guaranteed delivery.
			log.Printf("[server] client send buffer full, dropping message")
		}
	}
}

package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	_ "embed"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/magnogouveia/chat-multi-stream/internal/domain"
	"github.com/magnogouveia/chat-multi-stream/internal/provider"
)

//go:embed static/overlay.html
var overlayHTML []byte

//go:embed static/obs.html
var obsHTML []byte

//go:embed static/admin.html
var adminHTML []byte

// Server owns the HTTP server and the WebSocket broadcast hub.
type Server struct {
	httpServer    *http.Server
	messages      <-chan domain.ChatMessage
	hub           *hub
	adminUser     string
	adminPassword string
	ytProvider    *provider.YouTubeProvider
}

// New creates a Server that reads from messages and broadcasts to all connected
// WebSocket clients. addr should be the listen address (e.g. ":8080").
// Provide non-empty adminUser/adminPassword and a ytProvider to enable the /admin dashboard.
func New(addr string, messages <-chan domain.ChatMessage, adminUser, adminPassword string, ytProvider *provider.YouTubeProvider) *Server {
	s := &Server{
		messages:      messages,
		hub:           newHub(),
		adminUser:     adminUser,
		adminPassword: adminPassword,
		ytProvider:    ytProvider,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/ws",      s.handleWS)
	mux.HandleFunc("/overlay", s.handleOverlay)
	mux.HandleFunc("/obs",     s.handleOBS)
	mux.HandleFunc("/health",  s.handleHealth)

	if adminUser != "" && adminPassword != "" && ytProvider != nil {
		mux.HandleFunc("/admin", s.basicAuth(s.handleAdmin))
		mux.HandleFunc("/admin/api/status", s.basicAuth(s.handleAdminStatus))
		mux.HandleFunc("/admin/api/youtube/toggle", s.basicAuth(s.handleAdminToggle))
		mux.HandleFunc("/admin/api/youtube/chaturl", s.basicAuth(s.handleAdminChatURL))
	}

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
				return
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
				return
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

// handleOBS serves the OBS overlay with black background and bubble-style chat.
func (s *Server) handleOBS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(obsHTML)
}

// handleHealth returns a simple JSON health check.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// ── Admin ─────────────────────────────────────────────────────────────────────

// basicAuth wraps a handler with HTTP Basic Authentication.
// It uses constant-time comparison to prevent timing attacks.
func (s *Server) basicAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok ||
			subtle.ConstantTimeCompare([]byte(user), []byte(s.adminUser)) != 1 ||
			subtle.ConstantTimeCompare([]byte(pass), []byte(s.adminPassword)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="admin"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// handleAdmin serves the admin dashboard HTML.
func (s *Server) handleAdmin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(adminHTML)
}

// handleAdminStatus returns the current YouTube provider state as JSON.
func (s *Server) handleAdminStatus(w http.ResponseWriter, r *http.Request) {
	state := s.ytProvider.GetState()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(state)
}

// handleAdminChatURL accepts a JSON body {"url": "..."}`, parses the YouTube
// video/stream URL, and hands the extracted video ID to the YouTube provider.
func (s *Server) handleAdminChatURL(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var body struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	if err := s.ytProvider.SetChatURL(body.URL); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	log.Printf("[admin] YouTube chat URL definida: %q", body.URL)

	state := s.ytProvider.GetState()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(state)
}

// handleAdminToggle accepts a JSON body {"enabled": bool} and updates the
// YouTube provider's enabled state.
func (s *Server) handleAdminToggle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	s.ytProvider.SetEnabled(body.Enabled)
	log.Printf("[admin] YouTube provider enabled=%v", body.Enabled)

	state := s.ytProvider.GetState()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(state)
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
			log.Printf("[server] client send buffer full, dropping message")
		}
	}
}


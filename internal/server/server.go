package server

import (
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
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
	ytEmojiCache  sync.Map // videoID → map[string]string
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
	mux.HandleFunc("/api/yt-emojis", s.handleYouTubeEmojis)

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

// ── YouTube emoji proxy ───────────────────────────────────────────────────────

// ytVideoIDRe validates YouTube video IDs (11 base64url chars, but can also be
// shorter variants; we allow 6–20 alphanumeric/-/_ chars).
var ytVideoIDRe = regexp.MustCompile(`^[A-Za-z0-9_\-]{6,20}$`)

// handleYouTubeEmojis fetches the emoji catalog for a YouTube live chat by
// scraping ytInitialData from the live_chat page. Results are cached per
// video ID for the lifetime of the server process.
//
// GET /api/yt-emojis?videoId=VIDEO_ID
// Response: {"shortcode": "https://image-url", ...}
func (s *Server) handleYouTubeEmojis(w http.ResponseWriter, r *http.Request) {
	videoID := r.URL.Query().Get("videoId")
	if !ytVideoIDRe.MatchString(videoID) {
		http.Error(w, "invalid videoId", http.StatusBadRequest)
		return
	}

	// Return cached result if available.
	if v, ok := s.ytEmojiCache.Load(videoID); ok {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "max-age=3600")
		_ = json.NewEncoder(w).Encode(v)
		return
	}

	emojis, err := fetchYTEmojiCatalog(videoID)
	if err != nil {
		log.Printf("[yt-emojis] fetch failed for %s: %v", videoID, err)
		emojis = map[string]string{} // empty — overlay shows fallback badge
	} else {
		log.Printf("[yt-emojis] loaded %d emoji for video %s", len(emojis), videoID)
	}
	s.ytEmojiCache.Store(videoID, emojis)

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "max-age=3600")
	_ = json.NewEncoder(w).Encode(emojis)
}

// fetchYTEmojiCatalog scrapes the YouTube live_chat HTML page for the given
// video ID and extracts the emoji shortcode → thumbnail URL map from the
// embedded ytInitialData JSON blob.
func fetchYTEmojiCatalog(videoID string) (map[string]string, error) {
	url := "https://www.youtube.com/live_chat?v=" + videoID + "&is_popup=1"

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch live_chat page: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024)) // 4 MB cap
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	html := string(body)

	const marker = `ytInitialData"] = `
	idx := strings.Index(html, marker)
	if idx < 0 {
		return nil, fmt.Errorf("ytInitialData not found (video not live?)")
	}
	jsonStart := strings.IndexByte(html[idx:], '{')
	if jsonStart < 0 {
		return nil, fmt.Errorf("ytInitialData JSON start not found")
	}

	// Decode only the fields we need; ignore the rest via RawMessage.
	type thumbnail struct {
		URL string `json:"url"`
	}
	type emojiEntry struct {
		EmojiID   string   `json:"emojiId"`
		Shortcuts []string `json:"shortcuts"`
		Image     struct {
			Thumbnails []thumbnail `json:"thumbnails"`
		} `json:"image"`
	}
	var data struct {
		Contents struct {
			LiveChatRenderer struct {
				Emojis []emojiEntry `json:"emojis"`
			} `json:"liveChatRenderer"`
		} `json:"contents"`
	}

	dec := json.NewDecoder(strings.NewReader(html[idx+jsonStart:]))
	if err := dec.Decode(&data); err != nil {
		return nil, fmt.Errorf("decode ytInitialData: %w", err)
	}

	result := make(map[string]string, len(data.Contents.LiveChatRenderer.Emojis))
	for _, e := range data.Contents.LiveChatRenderer.Emojis {
		thumbs := e.Image.Thumbnails
		if len(thumbs) == 0 {
			continue
		}
		// Pick the largest thumbnail (last entry).
		imgURL := thumbs[len(thumbs)-1].URL
		// Prefer 48 px size; adjust the size query appended by YouTube.
		imgURL = strings.TrimRight(imgURL, " ")

		// Map by every shortcode (e.g. ":face-blue-smiling:" → "face-blue-smiling").
		for _, sc := range e.Shortcuts {
			name := strings.Trim(sc, ":")
			if name != "" {
				result[name] = imgURL
			}
		}
		// Also map by emojiId in case the message uses raw IDs.
		if e.EmojiID != "" && len(e.Shortcuts) == 0 {
			result[e.EmojiID] = imgURL
		}
	}
	return result, nil
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


package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"time"

	"github.com/coder/websocket"
	"github.com/magnogouveia/chat-multi-stream/internal/domain"
)

// kickPusherAppKey is Kick's publicly known Pusher application key, used for
// their real-time chat WebSocket service.
const kickPusherAppKey = "32cbd69e4b950bf97679"

// kickPusherURL is the Pusher WebSocket endpoint used by Kick.
const kickPusherURL = "wss://ws-us2.pusher.com/app/" + kickPusherAppKey +
	"?protocol=7&client=chat-multi-stream&version=1.0"

// KickProvider connects to a Kick channel's chat via Pusher WebSocket.
// The channel name is resolved to a numeric chatroom ID at startup using
// Kick's public REST API. The chatroom exists regardless of live status,
// so no special offline-waiting logic is needed.
type KickProvider struct {
	channel string
}

// NewKickProvider creates a KickProvider for the given channel name.
func NewKickProvider(channel string) *KickProvider {
	return &KickProvider{channel: channel}
}

func (p *KickProvider) Name() domain.Platform {
	return domain.PlatformKick
}

// Connect opens a Pusher WebSocket for the configured channel and forwards
// messages to out. Blocks until ctx is cancelled or a fatal error occurs.
func (p *KickProvider) Connect(ctx context.Context, out chan<- domain.ChatMessage) error {
	return connectKickChannel(ctx, p.channel, out)
}

// connectKickChannel resolves the chatroom ID, opens a Pusher WebSocket,
// subscribes to the channel's chat room, and forwards messages to out.
func connectKickChannel(ctx context.Context, channelName string, out chan<- domain.ChatMessage) error {
	chatroomID, err := resolveKickChatroomID(ctx, channelName)
	if err != nil {
		return fmt.Errorf("resolve chatroom ID: %w", err)
	}
	log.Printf("[kick] channel %s → chatroom ID %d", channelName, chatroomID)

	conn, _, err := websocket.Dial(ctx, kickPusherURL, nil)
	if err != nil {
		return fmt.Errorf("dial pusher: %w", err)
	}
	defer conn.CloseNow()

	// ── Pusher handshake ────────────────────────────────────────────────────
	// The server sends pusher:connection_established before we can subscribe.
	if err := awaitPusherConnected(ctx, conn); err != nil {
		return err
	}

	// Subscribe to the chatroom channel.
	chatroomChannel := fmt.Sprintf("chatrooms.%d.v2", chatroomID)
	subPayload, _ := json.Marshal(map[string]any{
		"event": "pusher:subscribe",
		"data": map[string]string{
			"auth":    "",
			"channel": chatroomChannel,
		},
	})
	if err := conn.Write(ctx, websocket.MessageText, subPayload); err != nil {
		return fmt.Errorf("send subscribe: %w", err)
	}
	log.Printf("[kick] subscribed to %s", chatroomChannel)

	// ── Message loop ────────────────────────────────────────────────────────
	return readKickMessages(ctx, conn, channelName, out)
}

// pusherEnvelope is the outer structure for all Pusher WebSocket messages.
type pusherEnvelope struct {
	Event   string          `json:"event"`
	Channel string          `json:"channel,omitempty"`
	Data    json.RawMessage `json:"data"`
}

func awaitPusherConnected(ctx context.Context, conn *websocket.Conn) error {
	_, raw, err := conn.Read(ctx)
	if err != nil {
		return fmt.Errorf("read connection_established: %w", err)
	}
	var env pusherEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("parse connection_established: %w", err)
	}
	if env.Event != "pusher:connection_established" {
		return fmt.Errorf("unexpected first event: %s", env.Event)
	}
	return nil
}

// kickChatData is the inner JSON payload of a Kick ChatMessageEvent.
type kickChatData struct {
	Content   string `json:"content"`
	CreatedAt string `json:"created_at"`
	Sender    struct {
		Username string `json:"username"`
		Identity *struct {
			Badges []struct {
				Type string `json:"type"`
			} `json:"badges"`
		} `json:"identity"`
	} `json:"sender"`
}

func readKickMessages(ctx context.Context, conn *websocket.Conn, channelName string, out chan<- domain.ChatMessage) error {
	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil // clean shutdown
			}
			return fmt.Errorf("read: %w", err)
		}

		var env pusherEnvelope
		if err := json.Unmarshal(raw, &env); err != nil {
			continue // skip malformed frames
		}

		if env.Event != `App\Events\ChatMessageEvent` {
			continue
		}

		msg, err := parseKickChatData(env.Data, channelName)
		if err != nil {
			log.Printf("[kick] parse message on %s: %v", channelName, err)
			continue
		}

		select {
		case <-ctx.Done():
			return nil
		case out <- msg:
		}
	}
}

// parseKickChatData handles Kick's double-encoded JSON: the "data" field may be
// a JSON string whose value is also JSON, or it may already be a JSON object.
func parseKickChatData(raw json.RawMessage, channelName string) (domain.ChatMessage, error) {
	var data kickChatData

	// Try direct unmarshal first.
	if err := json.Unmarshal(raw, &data); err != nil {
		// Try double-encoded string.
		var inner string
		if e2 := json.Unmarshal(raw, &inner); e2 != nil {
			return domain.ChatMessage{}, fmt.Errorf("unmarshal data: %w", err)
		}
		if err := json.Unmarshal([]byte(inner), &data); err != nil {
			return domain.ChatMessage{}, fmt.Errorf("unmarshal inner data: %w", err)
		}
	}

	ts, err := time.Parse(time.RFC3339Nano, data.CreatedAt)
	if err != nil {
		ts = time.Now()
	}

	var kickBadges []string
	if data.Sender.Identity != nil {
		for _, b := range data.Sender.Identity.Badges {
			if b.Type != "" {
				kickBadges = append(kickBadges, b.Type)
			}
		}
	}

	return domain.ChatMessage{
		Platform:  domain.PlatformKick,
		Channel:   channelName,
		Username:  data.Sender.Username,
		Message:   data.Content,
		Badges:    kickBadges,
		Timestamp: ts,
	}, nil
}

// kickChannelAPIResponse is the relevant subset of Kick's channel API response.
type kickChannelAPIResponse struct {
	Chatroom struct {
		ID int64 `json:"id"`
	} `json:"chatroom"`
}

// resolveKickChatroomID calls Kick's public REST API to convert a channel name
// (e.g. "xqc") into the numeric chatroom ID required for Pusher subscription.
func resolveKickChatroomID(ctx context.Context, channelName string) (int64, error) {
	// url.PathEscape prevents path traversal or injection via the channel name.
	apiURL := "https://kick.com/api/v2/channels/" + url.PathEscape(channelName)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "chat-multi-stream/1.0")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("kick API returned HTTP %d for channel %q", resp.StatusCode, channelName)
	}

	var data kickChannelAPIResponse
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return 0, fmt.Errorf("decode response: %w", err)
	}
	if data.Chatroom.ID == 0 {
		return 0, fmt.Errorf("chatroom ID not found for channel %q", channelName)
	}
	return data.Chatroom.ID, nil
}

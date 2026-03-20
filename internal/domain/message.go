package domain

import (
	"context"
	"time"
)

// Platform identifies the origin streaming platform.
type Platform string

const (
	PlatformTwitch  Platform = "twitch"
	PlatformYouTube Platform = "youtube"
	PlatformKick    Platform = "kick"
)

// EmoteInfo carries enough data for the frontend to render a named emote as an image.
type EmoteInfo struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// ChatMessage is the unified, platform-agnostic message type emitted by all providers.
type ChatMessage struct {
	Platform  Platform    `json:"platform"`
	Channel   string      `json:"channel"`
	// ChannelID is the platform-native numeric channel ID (e.g. Twitch room-id).
	// The frontend uses it to fetch BTTV / FFZ / 7TV channel emote sets.
	ChannelID string      `json:"channel_id,omitempty"`
	Username  string      `json:"username"`
	Message   string      `json:"message"`
	// Emotes holds platform-native emotes detected in this message (e.g. Twitch IRC emotes).
	Emotes    []EmoteInfo `json:"emotes,omitempty"`
	Timestamp time.Time   `json:"timestamp"`
}

// ChatProvider is the Strategy interface that every platform adapter must implement.
// Connect blocks until ctx is cancelled, forwarding received messages to out.
// It returns a non-nil error only on fatal, unrecoverable failures.
// The aggregator is responsible for retrying with backoff on any returned error.
type ChatProvider interface {
	// Name returns the platform identifier for logging and routing.
	Name() Platform
	// Connect starts listening and pipes messages into out until ctx is done.
	Connect(ctx context.Context, out chan<- ChatMessage) error
}

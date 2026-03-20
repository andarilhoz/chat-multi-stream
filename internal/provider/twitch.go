package provider

import (
	"context"
	"log"
	"time"

	irc "github.com/gempir/go-twitch-irc/v4"
	"github.com/magnogouveia/chat-multi-stream/internal/domain"
)

// TwitchProvider listens to a Twitch channel as an anonymous read-only IRC client.
// No OAuth token is required — Twitch allows unauthenticated read-only connections.
// The IRC connection works regardless of whether the streamer is currently live.
type TwitchProvider struct {
	channel string
}

// NewTwitchProvider creates a TwitchProvider for the given channel name.
func NewTwitchProvider(channel string) *TwitchProvider {
	return &TwitchProvider{channel: channel}
}

func (p *TwitchProvider) Name() domain.Platform {
	return domain.PlatformTwitch
}

// Connect joins the configured channel and forwards PRIVMSG events to out.
// It blocks until ctx is cancelled or the IRC connection fails permanently.
func (p *TwitchProvider) Connect(ctx context.Context, out chan<- domain.ChatMessage) error {
	client := irc.NewAnonymousClient()

	client.OnPrivateMessage(func(msg irc.PrivateMessage) {
		ts := msg.Time
		if ts.IsZero() {
			ts = time.Now()
		}
		select {
		case <-ctx.Done():
		case out <- domain.ChatMessage{
			Platform:  domain.PlatformTwitch,
			Channel:   msg.Channel,
			Username:  msg.User.DisplayName,
			Message:   msg.Message,
			Timestamp: ts,
		}:
		}
	})

	client.Join(p.channel)
	log.Printf("[twitch] joining channel: %s", p.channel)

	// Disconnect the IRC client when the context is cancelled so that
	// client.Connect() unblocks and this function can return cleanly.
	go func() {
		<-ctx.Done()
		client.Disconnect()
	}()

	if err := client.Connect(); err != nil {
		// A disconnect triggered by us (context cancelled) is not an error.
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	return nil
}

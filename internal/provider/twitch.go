package provider

import (
	"context"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	irc "github.com/gempir/go-twitch-irc/v4"
	"github.com/magnogouveia/chat-multi-stream/internal/domain"
)

// TwitchProvider listens to a Twitch channel as an anonymous read-only IRC client.
// No OAuth token is required — Twitch allows unauthenticated read-only connections.
// The IRC connection works regardless of whether the streamer is currently live.
type TwitchProvider struct {
	channel string
	// roomIDs caches the numeric Twitch room-id for each channel name so it can
	// be included in outgoing messages for BTTV / FFZ / 7TV lookups on the frontend.
	roomIDs sync.Map // string → string
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

	// Cache the room-id from ROOMSTATE so we can attach it to every message.
	client.OnRoomStateMessage(func(msg irc.RoomStateMessage) {
		if msg.RoomID != "" {
			p.roomIDs.Store(msg.Channel, msg.RoomID)
		}
	})

	client.OnPrivateMessage(func(msg irc.PrivateMessage) {
		ts := msg.Time
		if ts.IsZero() {
			ts = time.Now()
		}

		// Deduplicate native Twitch emotes and build render URLs.
		seen := make(map[string]bool, len(msg.Emotes))
		emotes := make([]domain.EmoteInfo, 0, len(msg.Emotes))
		for _, e := range msg.Emotes {
			if !seen[e.Name] {
				seen[e.Name] = true
				emotes = append(emotes, domain.EmoteInfo{
					Name: e.Name,
					URL:  fmt.Sprintf("https://static-cdn.jtvnw.net/emoticons/v2/%s/default/dark/2.0", e.ID),
				})
			}
		}

		// Extract badge names (broadcaster, moderator, subscriber, vip, etc.).
		badgeNames := make([]string, 0, len(msg.User.Badges))
		for name := range msg.User.Badges {
			badgeNames = append(badgeNames, name)
		}
		sort.Strings(badgeNames)

		// Prefer the room-id from ROOMSTATE; fall back to the tag on the message itself.
		channelID := msg.RoomID
		if channelID == "" {
			if id, ok := p.roomIDs.Load(msg.Channel); ok {
				channelID = id.(string)
			}
		} else {
			p.roomIDs.Store(msg.Channel, channelID)
		}

		select {
		case <-ctx.Done():
		case out <- domain.ChatMessage{
			Platform:  domain.PlatformTwitch,
			Channel:   msg.Channel,
			ChannelID: channelID,
			Username:  msg.User.DisplayName,
			Message:   msg.Message,
			Badges:    badgeNames,
			Emotes:    emotes,
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

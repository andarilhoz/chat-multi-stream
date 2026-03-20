package provider

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"google.golang.org/api/option"
	"google.golang.org/api/youtube/v3"

	"github.com/magnogouveia/chat-multi-stream/internal/domain"
)

// offlineRetryInterval is how long the YouTube provider waits between checks
// when the configured channel is not currently live.
const offlineRetryInterval = 60 * time.Second

// YouTubeProvider polls YouTube Live Chat via the Data API v3.
// A valid API key is required (free tier: 10 000 quota units/day).
// YouTube does not offer a WebSocket interface — polling is the only approach.
//
// The channel handle (e.g. "@mkbhd" or "mkbhd") is resolved to a channel ID
// at startup. When the channel is not live the provider polls every minute
// until a live stream starts, then attaches to its chat automatically.
type YouTubeProvider struct {
	apiKey  string
	channel string
}

// NewYouTubeProvider creates a YouTubeProvider for the given channel handle.
// The handle may optionally include the '@' prefix (both "@mkbhd" and "mkbhd" work).
func NewYouTubeProvider(apiKey string, channel string) *YouTubeProvider {
	return &YouTubeProvider{apiKey: apiKey, channel: channel}
}

func (p *YouTubeProvider) Name() domain.Platform {
	return domain.PlatformYouTube
}

// Connect resolves the channel, waits for it to go live if needed, then polls
// the live chat. When a stream ends it loops back to waiting for the next one.
// Blocks until ctx is cancelled or a fatal error occurs.
func (p *YouTubeProvider) Connect(ctx context.Context, out chan<- domain.ChatMessage) error {
	svc, err := youtube.NewService(ctx, option.WithAPIKey(p.apiKey))
	if err != nil {
		return fmt.Errorf("youtube: create service: %w", err)
	}

	return pollChannel(ctx, svc, p.channel, out)
}

// pollChannel resolves the channel ID once, then loops forever:
//  1. Waits for an active live broadcast (polling every offlineRetryInterval).
//  2. Polls the live chat until the stream ends or an error occurs.
//  3. Goes back to step 1 to catch the next stream.
//
// Only returns on ctx cancellation or a fatal error (e.g. invalid API key,
// channel not found).
func pollChannel(ctx context.Context, svc *youtube.Service, channelName string, out chan<- domain.ChatMessage) error {
	channelID, err := resolveYouTubeChannelID(ctx, svc, channelName)
	if err != nil {
		// Fatal: channel doesn't exist or API key is bad — no point retrying.
		return fmt.Errorf("resolve channel %q: %w", channelName, err)
	}
	log.Printf("[youtube] channel %q → ID %s", channelName, channelID)

	for {
		if ctx.Err() != nil {
			return nil
		}

		// ── Wait for channel to go live ──────────────────────────────────
		videoID, err := findActiveLiveBroadcast(ctx, svc, channelID)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			log.Printf("[youtube] %q is not live — checking again in %s", channelName, offlineRetryInterval)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(offlineRetryInterval):
			}
			continue
		}
		log.Printf("[youtube] channel %q is live: video %s", channelName, videoID)

		liveChatID, err := getLiveChatID(ctx, svc, videoID)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			log.Printf("[youtube] could not get live chat for %q: %v — retrying", channelName, err)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(offlineRetryInterval):
			}
			continue
		}
		log.Printf("[youtube] channel %q live chat ID: %s", channelName, liveChatID)

		// ── Poll chat until stream ends ───────────────────────────────────
		if err := pollLiveChat(ctx, svc, liveChatID, channelName, channelID, out); err != nil && ctx.Err() == nil {
			log.Printf("[youtube] chat ended for %q (%v) — watching for next stream", channelName, err)
		}
	}
}

// resolveYouTubeChannelID converts a channel handle (e.g. "@mkbhd" or "mkbhd") or
// legacy username into a YouTube channel ID.
func resolveYouTubeChannelID(ctx context.Context, svc *youtube.Service, name string) (string, error) {
	// Normalise: ensure the handle has the '@' prefix for the forHandle API.
	handle := name
	if !strings.HasPrefix(handle, "@") {
		handle = "@" + handle
	}

	// Try forHandle first (works for all modern YouTube channels).
	resp, err := svc.Channels.List([]string{"id"}).ForHandle(handle).Context(ctx).Do()
	if err == nil && len(resp.Items) > 0 {
		return resp.Items[0].Id, nil
	}

	// Fall back to legacy forUsername lookup (older channels without handles).
	resp, err = svc.Channels.List([]string{"id"}).ForUsername(strings.TrimPrefix(name, "@")).Context(ctx).Do()
	if err != nil {
		return "", fmt.Errorf("username lookup failed: %w", err)
	}
	if len(resp.Items) == 0 {
		return "", fmt.Errorf("channel %q not found (tried handle and username lookup)", name)
	}
	return resp.Items[0].Id, nil
}

// findActiveLiveBroadcast searches for a currently live video on the given channel.
func findActiveLiveBroadcast(ctx context.Context, svc *youtube.Service, channelID string) (string, error) {
	resp, err := svc.Search.
		List([]string{"id"}).
		ChannelId(channelID).
		EventType("live").
		Type("video").
		Context(ctx).
		Do()
	if err != nil {
		return "", fmt.Errorf("search live broadcasts: %w", err)
	}
	if len(resp.Items) == 0 {
		return "", fmt.Errorf("no active live stream found for channel %s", channelID)
	}
	return resp.Items[0].Id.VideoId, nil
}

// getLiveChatID fetches the activeLiveChatId for a video that is currently live.
func getLiveChatID(ctx context.Context, svc *youtube.Service, videoID string) (string, error) {
	resp, err := svc.Videos.
		List([]string{"liveStreamingDetails"}).
		Id(videoID).
		Context(ctx).
		Do()
	if err != nil {
		return "", fmt.Errorf("get video %s: %w", videoID, err)
	}
	if len(resp.Items) == 0 {
		return "", fmt.Errorf("video %s not found", videoID)
	}
	liveChatID := resp.Items[0].LiveStreamingDetails.ActiveLiveChatId
	if liveChatID == "" {
		return "", fmt.Errorf("video %s has no active live chat", videoID)
	}
	return liveChatID, nil
}

// pollLiveChat polls a live chat for new messages, forwarding each one to out.
// It respects the pollingIntervalMillis from each API response to avoid quota exhaustion.
// channelID is the resolved YouTube channel ID and is attached to every message so the
// frontend can perform channel-scoped emoji lookups.
func pollLiveChat(ctx context.Context, svc *youtube.Service, liveChatID, channelName, channelID string, out chan<- domain.ChatMessage) error {
	var pageToken string
	for {
		if ctx.Err() != nil {
			return nil
		}

		call := svc.LiveChatMessages.
			List(liveChatID, []string{"snippet", "authorDetails"}).
			Context(ctx)
		if pageToken != "" {
			call = call.PageToken(pageToken)
		}

		resp, err := call.Do()
		if err != nil {
			return fmt.Errorf("list messages: %w", err)
		}

		for _, item := range resp.Items {
			ts, err := time.Parse(time.RFC3339Nano, item.Snippet.PublishedAt)
			if err != nil {
				ts = time.Now()
			}

			// For text messages use the raw message field so emoji shortcodes
			// are preserved exactly as the user typed them. For all other event
			// types (superChatEvent, memberMilestoneChatEvent, etc.) fall back
			// to DisplayMessage which already contains a human-readable summary.
			text := item.Snippet.DisplayMessage
			if item.Snippet.Type == "textMessageEvent" && item.Snippet.TextMessageDetails != nil {
				text = item.Snippet.TextMessageDetails.MessageText
			}

			select {
			case <-ctx.Done():
				return nil
			case out <- domain.ChatMessage{
				Platform:  domain.PlatformYouTube,
				Channel:   channelName,
				ChannelID: channelID,
				Username:  strings.TrimPrefix(item.AuthorDetails.DisplayName, "@"),
				Message:   text,
				Timestamp: ts,
			}:
			}
		}

		pageToken = resp.NextPageToken

		// Respect the polling interval the API provides to avoid quota exhaustion.
		interval := time.Duration(resp.PollingIntervalMillis) * time.Millisecond
		if interval < time.Second {
			interval = time.Second
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(interval):
		}
	}
}

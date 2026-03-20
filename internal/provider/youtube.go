package provider

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"google.golang.org/api/option"
	"google.golang.org/api/youtube/v3"

	"github.com/magnogouveia/chat-multi-stream/internal/domain"
)

// errNotLive is returned by findLiveBroadcastViaRSS when the feed is reachable
// but no active live stream is found. It is distinct from infrastructure errors
// (network failures, HTTP errors) which warrant a search.list fallback.
var errNotLive = fmt.Errorf("channel is not live")

// defaultOfflineRetryInterval is the fallback polling interval used when the
// channel is offline and no custom value was provided via config.
const defaultOfflineRetryInterval = 60 * time.Second

// YouTubeProvider polls YouTube Live Chat via the Data API v3.
// A valid API key is required (free tier: 10 000 quota units/day).
// YouTube does not offer a WebSocket interface — polling is the only approach.
//
// The channel handle (e.g. "@mkbhd" or "mkbhd") is resolved to a channel ID
// at startup. When the channel is not live the provider polls every offlineRetry
// until a live stream starts, then attaches to its chat automatically.
type YouTubeProvider struct {
	apiKey        string
	channel       string
	offlineRetry  time.Duration
}

// NewYouTubeProvider creates a YouTubeProvider for the given channel handle.
// The handle may optionally include the '@' prefix (both "@mkbhd" and "mkbhd" work).
// offlineRetry controls how often the provider checks for a live stream; pass 0
// to use the default (60 s).
func NewYouTubeProvider(apiKey string, channel string, offlineRetry time.Duration) *YouTubeProvider {
	if offlineRetry <= 0 {
		offlineRetry = defaultOfflineRetryInterval
	}
	return &YouTubeProvider{apiKey: apiKey, channel: channel, offlineRetry: offlineRetry}
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

	return pollChannel(ctx, svc, p.channel, p.offlineRetry, out)
}

// pollChannel resolves the channel ID once, then loops forever:
//  1. Waits for an active live broadcast (polling every offlineRetry).
//  2. Polls the live chat until the stream ends or an error occurs.
//  3. Goes back to step 1 to catch the next stream.
//
// Only returns on ctx cancellation or a fatal error (e.g. invalid API key,
// channel not found).
func pollChannel(ctx context.Context, svc *youtube.Service, channelName string, offlineRetry time.Duration, out chan<- domain.ChatMessage) error {
	channelID, err := resolveYouTubeChannelID(ctx, svc, channelName)
	if err != nil {
		// Fatal: channel doesn't exist or API key is bad — no point retrying.
		return fmt.Errorf("resolve channel %q: %w", channelName, err)
	}
	log.Printf("[youtube] channel %q → ID %s", channelName, channelID)

	// skipVideoID is the last video that ended; we skip it in RSS checks until the
	// API cache clears and a different (genuinely live) video appears.
	var skipVideoID string

	for {
		if ctx.Err() != nil {
			return nil
		}

		// ── Step 1: RSS pre-check (0 quota units) ────────────────────────────
		// The channel Atom feed is public and free. We pull the latest video IDs
		// and check them with one videos.list call (1 unit) for an active chat.
		// search.list (100 units) is used ONLY when the RSS feed itself is broken.
		videoID, liveChatID, err := findLiveBroadcastViaRSS(ctx, svc, channelID, skipVideoID)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if err == errNotLive {
				// RSS confirmed the channel is offline — no fallback needed.
				log.Printf("[youtube] %q is not live — checking again in %s", channelName, offlineRetry)
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(offlineRetry):
				}
				continue
			}
			// RSS feed itself failed (network/HTTP error) — fall back to search.list (100 units).
			log.Printf("[youtube] RSS unavailable for %q (%v) — falling back to search.list", channelName, err)
			videoID, err = findActiveLiveBroadcast(ctx, svc, channelID)
			if err != nil {
				log.Printf("[youtube] %q is not live — checking again in %s", channelName, offlineRetry)
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(offlineRetry):
				}
				continue
			}
			liveChatID, err = getLiveChatID(ctx, svc, videoID)
			if err != nil {
				log.Printf("[youtube] could not get live chat for %q: %v — retrying in %s", channelName, err, offlineRetry)
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(offlineRetry):
				}
				continue
			}
		}
		log.Printf("[youtube] channel %q is live: video %s, chat %s", channelName, videoID, liveChatID)
		skipVideoID = "" // clear: we have a confirmed new stream

		// ── Step 2: poll chat until stream ends ───────────────────────────────
		if err := pollLiveChat(ctx, svc, liveChatID, channelName, channelID, out); err != nil && ctx.Err() == nil {
			log.Printf("[youtube] chat ended for %q (%v) — watching for next stream", channelName, err)
		}
		// Mark this video as dead so stale API cache doesn't re-detect it as live.
		skipVideoID = videoID
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

// findLiveBroadcastViaRSS finds an active live stream using the channel's public Atom
// feed (zero API quota cost) followed by a single videos.list call (1 quota unit).
// It returns the video ID and active live chat ID, or a non-nil error when the channel
// is offline or the feed is temporarily unavailable.
func findLiveBroadcastViaRSS(ctx context.Context, svc *youtube.Service, channelID string, skipVideoID string) (videoID, liveChatID string, err error) {
	feedURL := "https://www.youtube.com/feeds/videos.xml?channel_id=" + url.QueryEscape(channelID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, feedURL, nil)
	if err != nil {
		return "", "", fmt.Errorf("build RSS request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("fetch RSS: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("RSS feed returned HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	if err != nil {
		return "", "", fmt.Errorf("read RSS body: %w", err)
	}

	re := regexp.MustCompile(`<yt:videoId>([^<]+)</yt:videoId>`)
	matches := re.FindAllSubmatch(body, 10)
	if len(matches) == 0 {
		log.Printf("[youtube/rss] channel %s: RSS feed had no video entries", channelID)
		return "", "", fmt.Errorf("no videos in RSS feed for channel %s", channelID)
	}

	ids := make([]string, 0, len(matches))
	for _, m := range matches {
		if len(m) > 1 {
			ids = append(ids, string(m[1]))
		}
	}

	// Check the latest videos (up to 5) in one videos.list call — 1 quota unit.
	if len(ids) > 5 {
		ids = ids[:5]
	}
	log.Printf("[youtube/rss] channel %s: RSS videos=%v skipVideoID=%q", channelID, ids, skipVideoID)

	vResp, err := svc.Videos.
		List([]string{"id", "liveStreamingDetails"}).
		Id(strings.Join(ids, ",")).
		Context(ctx).
		Do()
	if err != nil {
		return "", "", fmt.Errorf("videos.list: %w", err)
	}
	for _, item := range vResp.Items {
		if item.Id == skipVideoID {
			log.Printf("[youtube/rss] video %s: SKIPPED (was last dead stream)", item.Id)
			continue
		}
		d := item.LiveStreamingDetails
		if d == nil {
			log.Printf("[youtube/rss] video %s: no liveStreamingDetails", item.Id)
			continue
		}
		log.Printf("[youtube/rss] video %s: activeLiveChatId=%q actualStartTime=%q actualEndTime=%q",
			item.Id, d.ActiveLiveChatId, d.ActualStartTime, d.ActualEndTime)
		// A truly live stream has: chat ID set, stream has started (ActualStartTime != ""),
		// and stream has not ended (ActualEndTime == "").
		// Upcoming/scheduled streams also have a chat ID but ActualStartTime is empty — skip those.
		if d.ActiveLiveChatId != "" && d.ActualStartTime != "" && d.ActualEndTime == "" {
			return item.Id, d.ActiveLiveChatId, nil
		}
	}
	// Feed was reachable and videos were checked — channel is simply not live.
	log.Printf("[youtube/rss] channel %s: no active live stream found → errNotLive", channelID)
	return "", "", errNotLive
}

// findActiveLiveBroadcast searches for a currently live video on the given channel.
// Costs 100 quota units — used only as a fallback when the RSS check fails.
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

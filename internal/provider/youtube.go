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
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/api/option"
	"google.golang.org/api/youtube/v3"

	"github.com/magnogouveia/chat-multi-stream/internal/domain"
)

// errNotLive is returned by findLiveBroadcastViaRSS when the feed is reachable
// but no active live stream is found.
var errNotLive = fmt.Errorf("channel is not live")

const defaultOfflineRetryInterval = 60 * time.Second

// YouTubeQuota tracks estimated YouTube Data API v3 quota usage for the current
// server session. All fields are updated atomically and reset to zero on restart.
type YouTubeQuota struct {
	Total   int64 // total units consumed
	Search  int64 // search.list calls × 100 units each
	Video   int64 // videos.list calls × 1 unit each
	Chat    int64 // liveChatMessages.list calls × ~5 units each
	Channel int64 // channels.list calls × 1 unit each
}

// YouTubeState is a point-in-time snapshot of the YouTube provider's runtime state.
type YouTubeState struct {
	Enabled     bool
	IsLive      bool
	VideoID     string
	VideoURL    string
	ChannelName string
	LiveChatID  string
	Quota       YouTubeQuota
}

// YouTubeProvider polls YouTube Live Chat via the Data API v3.
type YouTubeProvider struct {
	apiKey       string
	channel      string
	offlineRetry time.Duration

	mu         sync.RWMutex
	enabled    bool
	isLive     bool
	videoID    string
	liveChatID string

	quotaTotal   int64
	quotaSearch  int64
	quotaVideo   int64
	quotaChat    int64
	quotaChannel int64
}

func NewYouTubeProvider(apiKey string, channel string, offlineRetry time.Duration) *YouTubeProvider {
	if offlineRetry <= 0 {
		offlineRetry = defaultOfflineRetryInterval
	}
	return &YouTubeProvider{
		apiKey:       apiKey,
		channel:      channel,
		offlineRetry: offlineRetry,
		enabled:      true,
	}
}

func (p *YouTubeProvider) Name() domain.Platform {
	return domain.PlatformYouTube
}

// SetEnabled enables or disables the YouTube provider at runtime.
// When disabled the provider stops making API calls (saving quota) but
// keeps running so it can be re-enabled without restarting the server.
func (p *YouTubeProvider) SetEnabled(v bool) {
	p.mu.Lock()
	p.enabled = v
	if !v {
		p.isLive = false
		p.videoID = ""
		p.liveChatID = ""
	}
	p.mu.Unlock()
}

// GetState returns a snapshot of the provider's current runtime state.
func (p *YouTubeProvider) GetState() YouTubeState {
	p.mu.RLock()
	enabled := p.enabled
	isLive := p.isLive
	videoID := p.videoID
	liveChatID := p.liveChatID
	p.mu.RUnlock()

	videoURL := ""
	if videoID != "" {
		videoURL = "https://www.youtube.com/watch?v=" + videoID
	}

	return YouTubeState{
		Enabled:     enabled,
		IsLive:      isLive,
		VideoID:     videoID,
		VideoURL:    videoURL,
		ChannelName: p.channel,
		LiveChatID:  liveChatID,
		Quota: YouTubeQuota{
			Total:   atomic.LoadInt64(&p.quotaTotal),
			Search:  atomic.LoadInt64(&p.quotaSearch),
			Video:   atomic.LoadInt64(&p.quotaVideo),
			Chat:    atomic.LoadInt64(&p.quotaChat),
			Channel: atomic.LoadInt64(&p.quotaChannel),
		},
	}
}

func (p *YouTubeProvider) trackQuota(units int64, category *int64) {
	atomic.AddInt64(category, units)
	atomic.AddInt64(&p.quotaTotal, units)
}

// Connect resolves the channel, waits for it to go live if needed, then polls
// the live chat. Blocks until ctx is cancelled or a fatal error occurs.
func (p *YouTubeProvider) Connect(ctx context.Context, out chan<- domain.ChatMessage) error {
	svc, err := youtube.NewService(ctx, option.WithAPIKey(p.apiKey))
	if err != nil {
		return fmt.Errorf("youtube: create service: %w", err)
	}
	return p.pollChannel(ctx, svc, out)
}

// pollChannel resolves the channel ID once then loops:
//  1. If disabled, sleeps 2 s and re-checks.
//  2. Waits for an active live broadcast (polling every offlineRetry).
//  3. Polls the live chat until the stream ends or an error occurs.
//  4. Goes back to step 1.
func (p *YouTubeProvider) pollChannel(ctx context.Context, svc *youtube.Service, out chan<- domain.ChatMessage) error {
	channelID, err := p.resolveYouTubeChannelID(ctx, svc, p.channel)
	if err != nil {
		return fmt.Errorf("resolve channel %q: %w", p.channel, err)
	}
	log.Printf("[youtube] channel %q → ID %s", p.channel, channelID)

	var skipVideoID string

	const periodicSearchInterval = 15 * time.Minute
	var lastSearchCheck time.Time

	for {
		if ctx.Err() != nil {
			return nil
		}

		// ── Enabled check — pause without consuming quota ─────────────────────
		p.mu.RLock()
		enabled := p.enabled
		p.mu.RUnlock()
		if !enabled {
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(2 * time.Second):
			}
			continue
		}

		// ── Step 1: RSS pre-check (0+1 quota units) ───────────────────────────
		videoID, liveChatID, err := p.findLiveBroadcastViaRSS(ctx, svc, channelID, skipVideoID)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if err == errNotLive {
				now := time.Now()
				if now.Sub(lastSearchCheck) >= periodicSearchInterval {
					lastSearchCheck = now
					log.Printf("[youtube] %q RSS says not live — running periodic search.list safety check", p.channel)
					vid, searchErr := p.findActiveLiveBroadcast(ctx, svc, channelID)
					if searchErr == nil {
						log.Printf("[youtube] search.list found live video %q for %q — was missing from RSS", vid, p.channel)
						chatID, chatErr := p.getLiveChatID(ctx, svc, vid)
						if chatErr == nil {
							videoID = vid
							liveChatID = chatID
							err = nil
						} else {
							log.Printf("[youtube] could not get live chat for %q (%v) — retrying in %s", p.channel, chatErr, p.offlineRetry)
						}
					} else {
						log.Printf("[youtube] search.list confirms %q is not live (%v)", p.channel, searchErr)
					}
				}

				if err == errNotLive {
					log.Printf("[youtube] %q is not live — checking again in %s", p.channel, p.offlineRetry)
					p.mu.Lock()
					p.isLive = false
					p.videoID = ""
					p.liveChatID = ""
					p.mu.Unlock()
					select {
					case <-ctx.Done():
						return nil
					case <-time.After(p.offlineRetry):
					}
					continue
				}
			} else {
				log.Printf("[youtube] RSS unavailable for %q (%v) — falling back to search.list", p.channel, err)
				lastSearchCheck = time.Now()
				videoID, err = p.findActiveLiveBroadcast(ctx, svc, channelID)
				if err != nil {
					log.Printf("[youtube] %q is not live — checking again in %s", p.channel, p.offlineRetry)
					select {
					case <-ctx.Done():
						return nil
					case <-time.After(p.offlineRetry):
					}
					continue
				}
				liveChatID, err = p.getLiveChatID(ctx, svc, videoID)
				if err != nil {
					log.Printf("[youtube] could not get live chat for %q: %v — retrying in %s", p.channel, err, p.offlineRetry)
					select {
					case <-ctx.Done():
						return nil
					case <-time.After(p.offlineRetry):
					}
					continue
				}
			}
		}

		log.Printf("[youtube] channel %q is live: video %s, chat %s", p.channel, videoID, liveChatID)
		skipVideoID = ""
		lastSearchCheck = time.Time{}

		p.mu.Lock()
		p.isLive = true
		p.videoID = videoID
		p.liveChatID = liveChatID
		p.mu.Unlock()

		// ── Step 2: poll chat until stream ends ───────────────────────────────
		if err := p.pollLiveChat(ctx, svc, liveChatID, p.channel, channelID, out); err != nil && ctx.Err() == nil {
			log.Printf("[youtube] chat ended for %q (%v) — watching for next stream", p.channel, err)
		}

		p.mu.Lock()
		p.isLive = false
		p.liveChatID = ""
		p.mu.Unlock()

		skipVideoID = videoID
	}
}

func formatDuration(d time.Duration) string {
	if d <= 0 {
		return "never"
	}
	return d.Truncate(time.Second).String()
}

func (p *YouTubeProvider) resolveYouTubeChannelID(ctx context.Context, svc *youtube.Service, name string) (string, error) {
	handle := name
	if !strings.HasPrefix(handle, "@") {
		handle = "@" + handle
	}

	resp, err := svc.Channels.List([]string{"id"}).ForHandle(handle).Context(ctx).Do()
	p.trackQuota(1, &p.quotaChannel)
	if err == nil && len(resp.Items) > 0 {
		return resp.Items[0].Id, nil
	}

	resp, err = svc.Channels.List([]string{"id"}).ForUsername(strings.TrimPrefix(name, "@")).Context(ctx).Do()
	p.trackQuota(1, &p.quotaChannel)
	if err != nil {
		return "", fmt.Errorf("username lookup failed: %w", err)
	}
	if len(resp.Items) == 0 {
		return "", fmt.Errorf("channel %q not found (tried handle and username lookup)", name)
	}
	return resp.Items[0].Id, nil
}

func (p *YouTubeProvider) findLiveBroadcastViaRSS(ctx context.Context, svc *youtube.Service, channelID string, skipVideoID string) (videoID, liveChatID string, err error) {
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

	if len(ids) > 5 {
		ids = ids[:5]
	}
	log.Printf("[youtube/rss] channel %s: RSS videos=%v skipVideoID=%q", channelID, ids, skipVideoID)

	vResp, err := svc.Videos.
		List([]string{"id", "liveStreamingDetails"}).
		Id(strings.Join(ids, ",")).
		Context(ctx).
		Do()
	p.trackQuota(1, &p.quotaVideo)
	if err != nil {
		return "", "", fmt.Errorf("videos.list: %w", err)
	}

	returnedIDs := make(map[string]struct{}, len(vResp.Items))
	for _, item := range vResp.Items {
		returnedIDs[item.Id] = struct{}{}
	}
	for _, id := range ids {
		if id == skipVideoID {
			continue
		}
		if _, found := returnedIDs[id]; !found {
			log.Printf("[youtube/rss] video %s: NOT returned by videos.list — falling back to search.list", id)
			return "", "", fmt.Errorf("video %s missing from videos.list response", id)
		}
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
		if d.ActiveLiveChatId != "" && d.ActualStartTime != "" && d.ActualEndTime == "" {
			return item.Id, d.ActiveLiveChatId, nil
		}
	}
	log.Printf("[youtube/rss] channel %s: no active live stream found → errNotLive", channelID)
	return "", "", errNotLive
}

// findActiveLiveBroadcast searches for a currently live video on the given channel.
// Costs 100 quota units — used only as a fallback when the RSS check fails.
func (p *YouTubeProvider) findActiveLiveBroadcast(ctx context.Context, svc *youtube.Service, channelID string) (string, error) {
	resp, err := svc.Search.
		List([]string{"id"}).
		ChannelId(channelID).
		EventType("live").
		Type("video").
		Context(ctx).
		Do()
	p.trackQuota(100, &p.quotaSearch)
	if err != nil {
		return "", fmt.Errorf("search live broadcasts: %w", err)
	}
	if len(resp.Items) == 0 {
		return "", fmt.Errorf("no active live stream found for channel %s", channelID)
	}
	return resp.Items[0].Id.VideoId, nil
}

// getLiveChatID fetches the activeLiveChatId for a video that is currently live.
func (p *YouTubeProvider) getLiveChatID(ctx context.Context, svc *youtube.Service, videoID string) (string, error) {
	resp, err := svc.Videos.
		List([]string{"liveStreamingDetails"}).
		Id(videoID).
		Context(ctx).
		Do()
	p.trackQuota(1, &p.quotaVideo)
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
func (p *YouTubeProvider) pollLiveChat(ctx context.Context, svc *youtube.Service, liveChatID, channelName, channelID string, out chan<- domain.ChatMessage) error {
	var pageToken string
	for {
		if ctx.Err() != nil {
			return nil
		}

		// Stop polling if disabled mid-stream.
		p.mu.RLock()
		enabled := p.enabled
		p.mu.RUnlock()
		if !enabled {
			return fmt.Errorf("provider disabled")
		}

		call := svc.LiveChatMessages.
			List(liveChatID, []string{"snippet", "authorDetails"}).
			Context(ctx)
		if pageToken != "" {
			call = call.PageToken(pageToken)
		}

		resp, err := call.Do()
		p.trackQuota(5, &p.quotaChat)
		if err != nil {
			return fmt.Errorf("list messages: %w", err)
		}

		for _, item := range resp.Items {
			ts, err := time.Parse(time.RFC3339Nano, item.Snippet.PublishedAt)
			if err != nil {
				ts = time.Now()
			}

			text := item.Snippet.DisplayMessage
			if item.Snippet.Type == "textMessageEvent" && item.Snippet.TextMessageDetails != nil {
				text = item.Snippet.TextMessageDetails.MessageText
				preview := text
				if len(preview) > 120 {
					preview = preview[:120] + "…"
				}
				log.Printf("[youtube/msg] raw=%q display=%q", preview, item.Snippet.DisplayMessage)
			}

			var ytBadges []string
			if item.AuthorDetails.IsChatOwner {
				ytBadges = append(ytBadges, "owner")
			}
			if item.AuthorDetails.IsChatModerator {
				ytBadges = append(ytBadges, "moderator")
			}
			if item.AuthorDetails.IsChatSponsor {
				ytBadges = append(ytBadges, "member")
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
				Badges:    ytBadges,
				Timestamp: ts,
			}:
			}
		}

		pageToken = resp.NextPageToken

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

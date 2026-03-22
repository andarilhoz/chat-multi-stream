package provider

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/api/option"
	"google.golang.org/api/youtube/v3"

	"github.com/magnogouveia/chat-multi-stream/internal/domain"
)

// YouTubeQuota tracks estimated YouTube Data API v3 quota usage for the current
// server session. All fields are updated atomically and reset to zero on restart.
type YouTubeQuota struct {
	Total int64 // total units consumed
	Video int64 // videos.list calls × 1 unit each
	Chat  int64 // liveChatMessages.list calls × ~5 units each
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

// YouTubeProvider reads YouTube Live Chat via the Data API v3.
// The video to monitor is set at runtime via SetChatURL — no channel polling occurs.
type YouTubeProvider struct {
	apiKey  string
	channel string // optional display name

	mu         sync.RWMutex
	enabled    bool
	isLive     bool
	videoID    string // set by SetChatURL
	liveChatID string

	quotaTotal int64
	quotaVideo int64
	quotaChat  int64
}

func NewYouTubeProvider(apiKey string, channel string) *YouTubeProvider {
	return &YouTubeProvider{
		apiKey:  apiKey,
		channel: channel,
		enabled: true,
	}
}

// extractYouTubeVideoID extracts the video ID from common YouTube URL formats:
//
//	https://www.youtube.com/watch?v=VIDEO_ID
//	https://www.youtube.com/live/VIDEO_ID
//	https://youtu.be/VIDEO_ID
func extractYouTubeVideoID(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	if u.Host == "youtu.be" {
		return strings.TrimPrefix(u.Path, "/")
	}
	if strings.HasPrefix(u.Path, "/live/") {
		return strings.TrimPrefix(u.Path, "/live/")
	}
	return u.Query().Get("v")
}

// SetChatURL parses a YouTube video/stream URL, extracts the video ID, and
// schedules it for chat polling on the next Connect loop iteration.
// Pass an empty string to stop monitoring the current stream.
func (p *YouTubeProvider) SetChatURL(rawURL string) error {
	if rawURL == "" {
		p.mu.Lock()
		p.videoID = ""
		p.isLive = false
		p.liveChatID = ""
		p.mu.Unlock()
		return nil
	}
	videoID := extractYouTubeVideoID(rawURL)
	if videoID == "" {
		return fmt.Errorf("não foi possível extrair o video ID da URL: %s", rawURL)
	}
	p.mu.Lock()
	p.videoID = videoID
	p.isLive = false
	p.liveChatID = ""
	p.mu.Unlock()
	log.Printf("[youtube] chat URL atualizado → video ID %s", videoID)
	return nil
}

func (p *YouTubeProvider) Name() domain.Platform {
	return domain.PlatformYouTube
}

// SetEnabled enables or disables the YouTube provider at runtime.
// When disabled the provider stops making API calls (saving quota) but
// keeps running so it can be re-enabled without restarting the server.
// The user-provided video URL is preserved so re-enabling resumes automatically.
func (p *YouTubeProvider) SetEnabled(v bool) {
	p.mu.Lock()
	p.enabled = v
	if !v {
		p.isLive = false
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
			Total: atomic.LoadInt64(&p.quotaTotal),
			Video: atomic.LoadInt64(&p.quotaVideo),
			Chat:  atomic.LoadInt64(&p.quotaChat),
		},
	}
}

func (p *YouTubeProvider) trackQuota(units int64, category *int64) {
	atomic.AddInt64(category, units)
	atomic.AddInt64(&p.quotaTotal, units)
}

// Connect waits for the user to set a video URL via SetChatURL, then polls the
// live chat for that video. Blocks until ctx is cancelled.
func (p *YouTubeProvider) Connect(ctx context.Context, out chan<- domain.ChatMessage) error {
	svc, err := youtube.NewService(ctx, option.WithAPIKey(p.apiKey))
	if err != nil {
		return fmt.Errorf("youtube: create service: %w", err)
	}
	return p.waitAndPoll(ctx, svc, out)
}

// waitAndPoll loops waiting for a videoID to be set via SetChatURL.
// Once set, it fetches the live chat ID and starts polling messages.
// When the stream ends or the provider is disabled, it goes back to waiting.
func (p *YouTubeProvider) waitAndPoll(ctx context.Context, svc *youtube.Service, out chan<- domain.ChatMessage) error {
	for {
		if ctx.Err() != nil {
			return nil
		}

		p.mu.RLock()
		enabled := p.enabled
		videoID := p.videoID
		p.mu.RUnlock()

		if !enabled || videoID == "" {
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(2 * time.Second):
			}
			continue
		}

		liveChatID, err := p.getLiveChatID(ctx, svc, videoID)
		if err != nil {
			log.Printf("[youtube] não foi possível obter o live chat do vídeo %s: %v — aguardando nova URL", videoID, err)
			p.mu.Lock()
			p.videoID = ""
			p.isLive = false
			p.liveChatID = ""
			p.mu.Unlock()
			continue
		}

		log.Printf("[youtube] vídeo %s → live chat %s", videoID, liveChatID)
		p.mu.Lock()
		p.isLive = true
		p.liveChatID = liveChatID
		p.mu.Unlock()

		if err := p.pollLiveChat(ctx, svc, liveChatID, p.channel, videoID, out); err != nil && ctx.Err() == nil {
			log.Printf("[youtube] chat encerrado para vídeo %s (%v) — aguardando nova URL", videoID, err)
		}

		p.mu.Lock()
		p.isLive = false
		p.liveChatID = ""
		p.videoID = ""
		p.mu.Unlock()
	}
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

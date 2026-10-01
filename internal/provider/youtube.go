package provider

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/api/option"
	"google.golang.org/api/youtube/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/magnogouveia/chat-multi-stream/internal/domain"
	"github.com/magnogouveia/chat-multi-stream/internal/provider/ytgrpc"
)

const youtubeGRPCTarget = "youtube.googleapis.com:443"

// YouTubeQuota tracks estimated YouTube Data API v3 quota usage for the current
// server session. All fields are updated atomically and reset to zero on restart.
type YouTubeQuota struct {
	Total int64 // total units consumed
	Video int64 // videos.list calls × 1 unit each
	Chat  int64 // liveChatMessages.streamList connections × 1 unit each (cost not documented by Google)
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
	conn, err := grpc.NewClient(youtubeGRPCTarget, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{})))
	if err != nil {
		return fmt.Errorf("youtube: dial grpc: %w", err)
	}
	defer conn.Close()
	return p.waitAndPoll(ctx, svc, conn, out)
}

// waitAndPoll loops waiting for a videoID to be set via SetChatURL.
// Once set, it fetches the live chat ID and starts streaming messages.
// When the stream ends or the provider is disabled, it goes back to waiting.
func (p *YouTubeProvider) waitAndPoll(ctx context.Context, svc *youtube.Service, conn *grpc.ClientConn, out chan<- domain.ChatMessage) error {
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

		if err := p.streamLiveChat(ctx, conn, liveChatID, p.channel, videoID, out); err != nil && ctx.Err() == nil {
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

// streamLiveChat consumes a live chat through the gRPC liveChatMessages.streamList
// method, forwarding each message to out. The server pushes messages as they
// arrive, so there is no polling interval. When the stream drops it reconnects
// from the last nextPageToken, so no messages are lost or repeated.
func (p *YouTubeProvider) streamLiveChat(ctx context.Context, conn *grpc.ClientConn, liveChatID, channelName, videoID string, out chan<- domain.ChatMessage) error {
	client := ytgrpc.NewV3DataLiveChatMessageServiceClient(conn)

	// Cancel the stream when the provider is disabled or the video URL changes.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				p.mu.RLock()
				stop := !p.enabled || p.videoID != videoID
				p.mu.RUnlock()
				if stop {
					cancel()
					return
				}
			}
		}
	}()

	streamCtx := metadata.AppendToOutgoingContext(ctx, "x-goog-api-key", p.apiKey)
	var pageToken string
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return fmt.Errorf("stopped")
		}

		stream, err := client.StreamList(streamCtx, &ytgrpc.LiveChatMessageListRequest{
			LiveChatId: liveChatID,
			Part:       []string{"snippet", "authorDetails"},
			MaxResults: 500,
			PageToken:  pageToken,
		})
		p.trackQuota(1, &p.quotaChat)
		if err != nil {
			return fmt.Errorf("stream list: %w", err)
		}

		for {
			resp, err := stream.Recv()
			if err != nil {
				if ctx.Err() != nil {
					return fmt.Errorf("stopped")
				}
				switch status.Code(err) {
				case codes.NotFound, codes.PermissionDenied, codes.FailedPrecondition,
					codes.InvalidArgument, codes.Unauthenticated, codes.ResourceExhausted:
					return fmt.Errorf("stream list: %w", err)
				}
				if err == io.EOF {
					// The server closes idle streams periodically; resume from the last token.
					break
				}
				// Transient error: back off, then reconnect from the last token.
				log.Printf("[youtube] stream interrompido (%v) — reconectando em %s", err, backoff)
				select {
				case <-ctx.Done():
					return fmt.Errorf("stopped")
				case <-time.After(backoff):
				}
				if backoff < 30*time.Second {
					backoff *= 2
				}
				break
			}
			backoff = time.Second

			for _, item := range resp.GetItems() {
				snip := item.GetSnippet()
				if snip.GetType() == ytgrpc.LiveChatMessageSnippet_CHAT_ENDED_EVENT {
					return fmt.Errorf("chat ended")
				}
				if snip.GetType() != ytgrpc.LiveChatMessageSnippet_TEXT_MESSAGE_EVENT {
					continue
				}

				ts, err := time.Parse(time.RFC3339Nano, snip.GetPublishedAt())
				if err != nil {
					ts = time.Now()
				}

				text := snip.GetDisplayMessage()
				if d := snip.GetTextMessageDetails(); d != nil {
					text = d.GetMessageText()
				}

				author := item.GetAuthorDetails()
				var ytBadges []string
				if author.GetIsChatOwner() {
					ytBadges = append(ytBadges, "owner")
				}
				if author.GetIsChatModerator() {
					ytBadges = append(ytBadges, "moderator")
				}
				if author.GetIsChatSponsor() {
					ytBadges = append(ytBadges, "member")
				}

				select {
				case <-ctx.Done():
					return fmt.Errorf("stopped")
				case out <- domain.ChatMessage{
					Platform:  domain.PlatformYouTube,
					Channel:   channelName,
					ChannelID: videoID,
					Username:  strings.TrimPrefix(author.GetDisplayName(), "@"),
					Message:   text,
					Badges:    ytBadges,
					Timestamp: ts,
				}:
				}
			}

			if tok := resp.GetNextPageToken(); tok != "" {
				pageToken = tok
			}
			if resp.GetOfflineAt() != "" {
				return fmt.Errorf("chat offline at %s", resp.GetOfflineAt())
			}
		}
	}
}

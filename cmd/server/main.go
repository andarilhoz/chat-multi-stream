package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/magnogouveia/chat-multi-stream/internal/aggregator"
	"github.com/magnogouveia/chat-multi-stream/internal/config"
	"github.com/magnogouveia/chat-multi-stream/internal/domain"
	"github.com/magnogouveia/chat-multi-stream/internal/provider"
	"github.com/magnogouveia/chat-multi-stream/internal/server"
)

func main() {
	log.SetFlags(log.Ltime | log.Lmsgprefix)

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	// signal.NotifyContext cancels ctx on SIGINT (Ctrl-C) or SIGTERM (systemd/Docker stop).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// ── Build providers ────────────────────────────────────────────────────
	// Providers are only created when sufficient configuration is present.
	// Missing config for a platform is silently skipped — not an error.
	var providers []domain.ChatProvider

	if cfg.TwitchChannel != "" {
		providers = append(providers, provider.NewTwitchProvider(cfg.TwitchChannel))
		log.Printf("twitch:  channel → %s", cfg.TwitchChannel)
	}

	var ytProvider *provider.YouTubeProvider
	if cfg.YouTubeAPIKey != "" {
		ytProvider = provider.NewYouTubeProvider(cfg.YouTubeAPIKey, cfg.YouTubeChannel)
		providers = append(providers, ytProvider)
		log.Printf("youtube: API key configurada (defina a URL do chat no painel admin)")
	}

	if cfg.KickChannel != "" {
		providers = append(providers, provider.NewKickProvider(cfg.KickChannel))
		log.Printf("kick:    channel → %s", cfg.KickChannel)
	}

	if len(providers) == 0 {
		log.Fatal("no active providers — check your .env configuration")
	}

	// ── Start aggregator ───────────────────────────────────────────────────
	agg := aggregator.New(providers)
	messages := agg.Run(ctx)

	// ── Start server ───────────────────────────────────────────────────────
	addr := fmt.Sprintf(":%s", cfg.ServerPort)
	srv := server.New(addr, messages, cfg.AdminUser, cfg.AdminPassword, ytProvider)

	log.Printf("server listening on %s", addr)
	log.Printf("  websocket : ws://localhost%s/ws", addr)
	log.Printf("  overlay   : http://localhost%s/overlay", addr)
	log.Printf("  health    : http://localhost%s/health", addr)
	if cfg.AdminUser != "" && cfg.AdminPassword != "" && ytProvider != nil {
		log.Printf("  admin     : http://localhost%s/admin", addr)
	}

	if err := srv.Run(ctx); err != nil {
		log.Fatalf("server: %v", err)
	}

	log.Println("shutdown complete")
}

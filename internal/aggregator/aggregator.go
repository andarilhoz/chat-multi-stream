package aggregator

import (
	"context"
	"log"
	"math"
	"sync"
	"time"

	"github.com/magnogouveia/chat-multi-stream/internal/domain"
)

const (
	// outBufferSize is the number of messages that can be queued before
	// the aggregator starts dropping (prevents a slow WebSocket hub from
	// blocking all provider goroutines).
	outBufferSize = 256

	initialBackoff = 1 * time.Second
	maxBackoff     = 30 * time.Second
)

// Aggregator fans-in messages from multiple ChatProviders into a single channel.
// Each provider runs in its own goroutine. On error or unexpected disconnect,
// providers are automatically restarted with exponential backoff.
type Aggregator struct {
	providers []domain.ChatProvider
}

// New creates an Aggregator for the given set of providers.
func New(providers []domain.ChatProvider) *Aggregator {
	return &Aggregator{providers: providers}
}

// Run starts all providers and returns a read-only merged message channel.
// The channel is closed once ctx is cancelled and all providers have stopped.
func (a *Aggregator) Run(ctx context.Context) <-chan domain.ChatMessage {
	out := make(chan domain.ChatMessage, outBufferSize)

	var wg sync.WaitGroup
	for _, p := range a.providers {
		wg.Add(1)
		go func(provider domain.ChatProvider) {
			defer func() {
				// Recover from any panic inside a provider so one bad provider
				// can't crash the whole server.
				if r := recover(); r != nil {
					log.Printf("[aggregator] provider %s panicked: %v", provider.Name(), r)
				}
				wg.Done()
			}()
			a.runProvider(ctx, provider, out)
		}(p)
	}

	// Close the output channel only after every provider goroutine has exited.
	// This is safe because each runProvider stops writing to out before returning.
	go func() {
		wg.Wait()
		close(out)
	}()

	return out
}

// runProvider runs a single provider, restarting it with exponential backoff
// whenever it returns an error or disconnects unexpectedly.
func (a *Aggregator) runProvider(ctx context.Context, p domain.ChatProvider, out chan<- domain.ChatMessage) {
	backoff := initialBackoff
	for {
		log.Printf("[aggregator] starting provider: %s", p.Name())

		err := p.Connect(ctx, out)

		// Context cancelled → clean shutdown, do not retry.
		if ctx.Err() != nil {
			return
		}

		if err != nil {
			log.Printf("[aggregator] provider %s error: %v — retrying in %s", p.Name(), err, backoff)
		} else {
			log.Printf("[aggregator] provider %s disconnected — reconnecting in %s", p.Name(), backoff)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}

		// Double the backoff on each retry, capped at maxBackoff.
		backoff = time.Duration(math.Min(float64(backoff*2), float64(maxBackoff)))
	}
}

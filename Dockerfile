# ── Build stage ───────────────────────────────────────────────────────────────
FROM golang:1.25-alpine AS builder

WORKDIR /app

# Download dependencies first (layer-cached unless go.mod/go.sum change)
COPY go.mod go.sum ./
RUN go mod download

# Copy source and build a fully static binary
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /chat-multi-stream ./cmd/server

# ── Runtime stage ─────────────────────────────────────────────────────────────
FROM alpine:3.21

# ca-certificates are required for outbound TLS calls (YouTube / Twitch APIs)
RUN apk add --no-cache ca-certificates

WORKDIR /app

COPY --from=builder /chat-multi-stream .

EXPOSE 8080

ENTRYPOINT ["./chat-multi-stream"]

# syntax=docker/dockerfile:1.7

# ---------- Build stage ----------
FROM golang:1.23-alpine AS builder
ENV GOTOOLCHAIN=auto

# build-base provides gcc/musl-dev for CGO (needed for go-sqlite3)
RUN apk add --no-cache build-base git ca-certificates

WORKDIR /src

# Cache module downloads
COPY go.mod go.sum ./
RUN go mod download

# Copy source
COPY . .

# Build a small, stripped static binary
# CGO_ENABLED=1 links against musl (Alpine-compatible)
ENV CGO_ENABLED=1 \
    GOOS=linux

RUN go build -trimpath -ldflags="-s -w" -o /out/eraser ./cmd/eraser

# ---------- Runtime stage ----------
FROM alpine:3.20

# ca-certificates: TLS for SMTP/HTTPS
# tzdata: accurate timestamps in logs/history
# sqlite-libs: runtime deps for CGO sqlite3 driver
# shadow: provides `su` for privilege dropping in entrypoint
RUN apk add --no-cache ca-certificates tzdata sqlite-libs shadow \
    && addgroup -S eraser && adduser -S -G eraser -h /home/eraser eraser

WORKDIR /home/eraser

# Copy entrypoint script (runs as root initially to fix permissions)
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod +x /usr/local/bin/docker-entrypoint.sh

# Copy binary from builder
COPY --from=builder /out/eraser /usr/local/bin/eraser

# Copy bundled data files with correct ownership
COPY --from=builder --chown=eraser:eraser /src/data /home/eraser/data
COPY --from=builder --chown=eraser:eraser /src/config.example.yaml /home/eraser/config.example.yaml

# Declare volume for persistent config + SQLite DB
VOLUME ["/home/eraser/.eraser"]

# Entrypoint handles init + privilege dropping; CMD provides default args
ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
CMD ["serve", "--port", "8080"]

# Note: USER is NOT set here — entrypoint script handles privilege dropping
# This allows the script to chown the volume before switching to eraser user

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=10s --start-period=15s --retries=3 \
  CMD wget --quiet --spider http://localhost:8080/health || exit 1

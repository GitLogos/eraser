# syntax=docker/dockerfile:1.7

# ---------- Build stage ----------
FROM golang:1.23-alpine AS builder
ENV GOTOOLCHAIN=auto

# build-base provides gcc/musl-dev for CGO (needed if go-sqlite3 is used).
# Remove these two lines + set CGO_ENABLED=0 if the project uses a pure-Go
# SQLite driver (e.g. modernc.org/sqlite).
RUN apk add --no-cache build-base git ca-certificates

WORKDIR /src

# Cache module downloads
COPY go.mod go.sum ./
RUN go mod download

# Copy source
COPY . .

# Build a small, stripped static binary.
# CGO_ENABLED=1 links against musl (Alpine-compatible).
ENV CGO_ENABLED=1 \
    GOOS=linux

RUN go build -trimpath -ldflags="-s -w" -o /out/eraser ./cmd/eraser

# ---------- Runtime stage ----------
FROM alpine:3.20

# ca-certificates: TLS for SMTP
# tzdata: accurate timestamps in history
# sqlite-libs: runtime libs if go-sqlite3 (CGO) is used; harmless otherwise
RUN apk add --no-cache ca-certificates tzdata sqlite-libs \
    && addgroup -S eraser && adduser -S -G eraser -h /home/eraser eraser

WORKDIR /home/eraser
USER eraser

# Binary
COPY --from=builder /out/eraser /usr/local/bin/eraser

# Bundled broker database & example config come along with the source tree
COPY --from=builder --chown=eraser:eraser /src/data /home/eraser/data
COPY --from=builder --chown=eraser:eraser /src/config.example.yaml /home/eraser/config.example.yaml

# Config and SQLite DB live under ~/.eraser — mount a volume here to persist
VOLUME ["/home/eraser/.eraser"]

EXPOSE 8080

ENTRYPOINT ["eraser"]
CMD ["serve", "-p", "8080"]

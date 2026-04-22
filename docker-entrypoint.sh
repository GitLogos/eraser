#!/bin/sh
set -e

# Data directory for config + SQLite database
DATA_DIR="/home/eraser/.eraser"

# Ensure directory exists (first-run with fresh volume)
mkdir -p "$DATA_DIR"

# Fix ownership: named volumes mount as root:root, but app runs as eraser
chown -R eraser:eraser "$DATA_DIR"

# Drop privileges and exec the application
# - su-exec replaces the shell with the Go binary directly
#   (no intermediate shell = proper PID 1 SIGTERM/SIGINT handling)
# - "$@" preserves argument quoting from the Dockerfile CMD / docker-compose
exec su-exec eraser:eraser eraser "$@"

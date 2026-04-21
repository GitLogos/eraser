#!/bin/sh
set -e

# Data directory for config + SQLite database
DATA_DIR="/home/eraser/.eraser"

# Ensure directory exists (first-run with fresh volume)
mkdir -p "$DATA_DIR"

# Fix ownership: named volumes mount as root:root, but app runs as eraser
chown -R eraser:eraser "$DATA_DIR"

# Drop privileges and exec the application
# - exec replaces PID 1 with the Go binary (proper SIGTERM/SIGINT handling)
# - su - eraser provides a clean environment for the user
# - "$*" preserves all arguments passed via docker-compose CMD
exec su - eraser -c "exec eraser $*"

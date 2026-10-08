#!/bin/bash
# Full reinstall: build, deploy binaries, and re-init a local workspace.
# Auth is account/team based: log in once (interactive password prompt), select
# a team, then init the local-only workspace. There is no registration password.
set -e

SERVER=https://YOUR_DOMAIN            # public HTTPS origin (server PUBLIC_URL)
USERNAME=YOUR_USERNAME
LOCAL_DEVICE=YOUR_USER-local
LOCAL_PROJECT=~/agentlink-test

echo "=== 1. Build ==="
cd "$(dirname "$0")"
go build -o agentlink ./cmd/agentlink/

echo "=== 2. Deploy binary ==="
cp agentlink ~/.local/bin/agentlink.new
mv ~/.local/bin/agentlink.new ~/.local/bin/agentlink

echo "=== 3. Log in (prompts for password) ==="
# Reuses the stored device session if already logged in; otherwise authenticates.
agentlink login --server "$SERVER" --username "$USERNAME" --device "$LOCAL_DEVICE"

echo "=== 4. Select a team ==="
# Pick an existing team, or create one: agentlink team create "Product"
agentlink team list
# agentlink team use <team_id>

echo "=== 5. Init local workspace ==="
rm -rf "$LOCAL_PROJECT"
agentlink init --force "$LOCAL_PROJECT" 2>&1 | head -5

echo "=== 6. Wait for heartbeat, then list team agents ==="
sleep 10
cd "$LOCAL_PROJECT/main"
agentlink list --all

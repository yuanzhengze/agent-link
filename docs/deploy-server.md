# Server Deployment Guide

## Prerequisites

- Linux server with **root / sudo access**
- Go 1.24+, Redis 7+
- Port 8080 accessible from the outside (configure cloud firewall / security group)

## Step 1: Build

```bash
git clone https://github.com/paparship/agent-link.git
cd agent-link
go build -o server ./cmd/server/
go build -o agentlink ./cmd/agentlink/
```

## Step 2: Install binaries

```bash
sudo useradd -r -s /usr/sbin/nologin agent-link
sudo mkdir -p /opt/agent-link /etc/agent-link /var/lib/agent-link/data
sudo cp server /opt/agent-link/
# agentlink-admin is used for out-of-band password resets (see the operations guide)
go build -o agentlink-admin ./cmd/agentlink-admin/ && sudo cp agentlink-admin /opt/agent-link/
sudo chown -R agent-link:agent-link /opt/agent-link /var/lib/agent-link
```

## Step 3: Configure Redis

Create `/etc/agent-link/redis.conf`:

```conf
bind 127.0.0.1
port 6379
dir /var/lib/agent-link/redis
maxmemory 256mb
maxmemory-policy allkeys-lru
```

```bash
sudo mkdir -p /var/lib/agent-link/redis
sudo chown agent-link:agent-link /var/lib/agent-link/redis
```

## Step 4: Create the environment file

Create `/etc/agent-link/server.env` (readable only by root). There is **no
shared registration password** — users self-register accounts through the CLI or
Web GUI, so this file only carries deployment settings:

```bash
sudo tee /etc/agent-link/server.env > /dev/null <<'EOF'
REDIS_ADDR=localhost:6379
PUBLIC_URL=https://your-domain.com
COOKIE_SECURE=true
DATA_DIR=/var/lib/agent-link/data
EOF
sudo chmod 600 /etc/agent-link/server.env
```

> **HTTPS is required in production.** With a non-loopback `PUBLIC_URL`, the
> server refuses to start unless the URL is `https://` and `COOKIE_SECURE=true`.
> Terminate TLS at Caddy/nginx (Step 6 note) and point `PUBLIC_URL` at the public
> HTTPS origin.

## Step 5: Install systemd units

```bash
sudo cp deploy/agent-link-server.service /etc/systemd/system/
sudo cp deploy/redis.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now redis
sudo systemctl enable --now agent-link-server
```

Verify:

```bash
curl http://localhost:8080/health
# → {"ok":true,"redis":"connected"}
```

## Step 6: Create the first account

Users self-register through the CLI (or the Web GUI at `PUBLIC_URL`). From any
client machine:

```bash
agentlink register --server https://your-domain.com --username kirby
agentlink team create "Product"
```

`register` prompts for a password on the terminal, creates the account, logs this
device in, and stores a device session — there is no token to copy. `team create`
makes the account the team Owner and prints a one-time invite code for teammates
to `agentlink team join <team_id> <invite_code>`.

## Environment variables

| Variable | Default | Description |
|----------|---------|-------------|
| `LISTEN_ADDR` | `:8080` | Server listen address |
| `REDIS_ADDR` | `localhost:6379` | Redis server address |
| `DATA_DIR` | `./data` | On-disk per-team project Git repositories |
| `PUBLIC_URL` | `http://localhost:8080` | Public origin; sets session cookie scope |
| `COOKIE_SECURE` | `false` | Send session cookies only over HTTPS (set `true` in prod) |

To change settings, edit `/etc/agent-link/server.env` and run
`sudo systemctl restart agent-link-server`. Forgotten passwords are reset
out-of-band with `agentlink-admin user reset-password <username>` — see
[team-auth-operations.md](team-auth-operations.md).

## Removal

```bash
# 1. Stop and disable services
sudo systemctl disable --now agent-link-server redis

# 2. Remove systemd units
sudo rm /etc/systemd/system/agent-link-server.service /etc/systemd/system/redis.service
sudo systemctl daemon-reload

# 3. Delete Redis data (agentlink v2 keys only — safe for shared Redis)
redis-cli KEYS "agentlink:v2:*" | xargs -r redis-cli DEL

# 4. Delete binaries, config, and data
sudo rm -rf /opt/agent-link /etc/agent-link /var/lib/agent-link

# 5. (Optional) Remove the user and source tree
sudo userdel agent-link
rm -rf ~/agent-link
```

## Notes

- **Redis must bind to localhost** — do not expose port 6379 to the internet.
- **Lighthouse vs CVM**: Lighthouse uses **防火墙** (not 安全组) in the console to open ports. CVM uses **安全组**.
- **Logs**: `journalctl -u agent-link-server -f`
- **HTTPS**: Add Caddy or nginx as a reverse proxy for HTTPS. Caddy auto-provisions Let's Encrypt certificates.

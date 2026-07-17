# Team Auth Operations Guide

Operational runbook for running the account + team authentication system in
production. Design reference:
[docs/superpowers/specs/2026-07-15-team-auth-design.md](superpowers/specs/2026-07-15-team-auth-design.md).
For first-time install, see [deploy-server.md](deploy-server.md).

This is the only authentication system: there is no shared registration password,
no API key keyspace, and no preview token. Users self-register accounts; the CLI
holds an opaque per-device session, and the Web GUI uses HttpOnly cookies + CSRF.

## 1. Required configuration

The server reads these environment variables (see `/etc/agent-link/server.env`):

| Variable | Required | Description |
|----------|----------|-------------|
| `PUBLIC_URL` | yes | Public origin (e.g. `https://cowork.example`). Sets the session-cookie scope and is validated at startup. |
| `COOKIE_SECURE` | yes (prod) | `true` marks session cookies `Secure` (HTTPS-only). |
| `REDIS_ADDR` | yes | Redis address, e.g. `localhost:6379`. |
| `DATA_DIR` | yes | Directory holding per-team project Git repositories. Back this up. |
| `LISTEN_ADDR` | no | Listen address, default `:8080`. |

### HTTPS requirement

The server calls `parsePublicOrigin` at startup and **refuses to boot** when:

- `COOKIE_SECURE=true` but `PUBLIC_URL` is not `https://`, or
- `PUBLIC_URL` is **non-loopback** and is not both `https://` and `COOKIE_SECURE=true`.

`localhost` / loopback may run over plain HTTP for local development only. In
production, terminate TLS at a reverse proxy (Caddy/nginx) and set `PUBLIC_URL`
to the public HTTPS origin with `COOKIE_SECURE=true`.

## 2. Password resets (admin)

Passwords are user-chosen and never stored in plaintext, so there is no
"look up a password" path. To recover a locked-out user, reset out-of-band with
the admin tool (runs against the same Redis, no server call):

```bash
REDIS_ADDR=localhost:6379 agentlink-admin user reset-password <username>
# → temporary_password=<one-time-value>
```

- Deliver the temporary password to the user over a **trusted side channel**.
- The account is flagged **must-change-password**: the user's next login forces a
  password change before any business API is allowed.
- Exit codes: `0` reset + delivered · `1` Redis/reset failure · `2` usage error ·
  `3` reset committed but stdout delivery failed — **do not retry automatically**
  (a second reset invalidates the first temporary password); capture and deliver
  it manually instead.

## 3. Session revocation & incident response

There are two session types, both revocable:

- **Device sessions (CLI)** — one opaque secret per device in
  `~/.agentlink/credentials.json` (`0600`). `agentlink logout` revokes the current
  device session server-side and deletes the local file. `agentlink uninstall`
  additionally cleans up local tmux/config state.
- **Web sessions (GUI)** — HttpOnly cookie + CSRF token; `POST /api/auth/logout`
  clears them.

If a credential is suspected compromised:

1. **Reset the user's password** (Section 2). Continue with the steps below to
   invalidate anything already issued.
2. **Revoke all of a user's sessions** by deleting their session keys in Redis:

   ```bash
   # Inspect first
   redis-cli KEYS "agentlink:v2:device_session:*"
   redis-cli KEYS "agentlink:v2:web_session:*"
   ```

   Device/web sessions carry a TTL and are re-validated (role re-read) on every
   request, so removing a member from a team revokes their team access
   immediately without needing to touch session keys.
3. **Remove or demote the actor** via the GUI or API (`DELETE /api/teams/{id}/members/{user_id}`,
   or `PATCH` the role). Team access is enforced per-request from the store.
4. **Rotate team invite codes** if an invite leaked:
   `POST /api/teams/{id}/invite/rotate` (Owner/Admin) invalidates the old code.

## 4. Backups

All durable state lives in two places:

- **Redis** — accounts, teams, memberships, sessions, locks, messages, tasks.
  Everything is namespaced under `agentlink:v2:*`.
- **`DATA_DIR`** — per-team project Git repositories (the file contents).

### Redis backup

```bash
# Option A: RDB snapshot (whole DB)
redis-cli SAVE                      # writes dump.rdb in Redis' dir
cp /var/lib/agent-link/redis/dump.rdb /backups/redis-$(date +%F).rdb

# Option B: export only agentlink v2 keys (shared-Redis friendly)
redis-cli --scan --pattern 'agentlink:v2:*' > /backups/agentlink-keys-$(date +%F).txt
```

### DATA_DIR backup

Each project is a Git repo, so a filesystem copy (or `tar`) of `DATA_DIR` while
the server is quiescent is a consistent snapshot:

```bash
tar czf /backups/agent-link-data-$(date +%F).tgz -C /var/lib/agent-link data
```

Restore Redis (`dump.rdb`) and `DATA_DIR` together — they reference each other by
project id.

## 5. Legacy v1 rollback & cleanup

The clean break removed all v1 authentication (the shared register password, the
old API-key keyspace, Bearer tokens, the preview token, and the `/preview/{id}`
route). Any v1 data that survived a prior deployment lives under the **non-`v2`
`agentlink:*` keys**; the current server never reads or writes them. All live v2
data is namespaced under `agentlink:v2:*`.

### Rollback

The v1 and v2 keyspaces are disjoint, so v1 data is left untouched by the v2
server. To roll back to a pre-cutover build, redeploy the old binary/config; its
v1 keys are still present unless you ran the cleanup below. **Do not run the v1
cleanup until the v2 deployment is accepted**, since that is the point of no
return for rollback.

### Explicit v1 cleanup (after acceptance)

Once the v2 deployment is accepted and you no longer need rollback, delete the
orphaned legacy (non-`v2`) keys. Inspect first, then delete:

```bash
# Inspect legacy (non-v2) keys — should be empty on a fresh v2 install
redis-cli --scan --pattern 'agentlink:*' | grep -v '^agentlink:v2:'

# Delete only the legacy keys, preserving all live agentlink:v2:* data
redis-cli --scan --pattern 'agentlink:*' | grep -v '^agentlink:v2:' | xargs -r redis-cli DEL
```

**Never** run a broad unfiltered `agentlink:*` delete on a live v2 deployment —
that would also destroy `agentlink:v2:*` (all live data). Always keep the
`grep -v '^agentlink:v2:'` guard.

## 6. Health & logs

```bash
curl https://your-domain.com/health     # → {"ok":true,"redis":"connected"}
journalctl -u agent-link-server -f       # server logs
```

Device-session secrets never appear in logs, URLs, or normal command output; if
you see one in a log, treat it as an incident (Section 3).

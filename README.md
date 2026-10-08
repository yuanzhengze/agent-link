# agentlink

Real-time collaborative prototyping for product teams. Multiple people edit the
same static HTML prototype with second-level sync and per-file locking, so local
coding agents never silently overwrite each other. Ships with Claude Code support.

Access is organized around **accounts** and **teams**: you register a personal
account with a password, then create or join a team. All projects, files, locks,
messages, and tasks are scoped to a team. There is no shared registration
password and no API token to copy — the CLI stores a per-device session for you.

## Quick Install (Linux / macOS)

```bash
curl -sfL https://github.com/paparship/agent-link/releases/latest/download/install.sh | sh
```

## Build from Source

```bash
make build              # build agentlink CLI
make build-server       # build server
make install            # build + install to /usr/local/bin
make uninstall          # remove from /usr/local/bin
make reinstall          # uninstall → build → install
make test               # run all tests
make clean              # remove build artifacts
```

Set `BINDIR` to override the install path:

```bash
make install BINDIR=~/.local/bin
```

## Deploy the Server

See [docs/deploy-server.md](docs/deploy-server.md) for the systemd-based install
and [docs/team-auth-operations.md](docs/team-auth-operations.md) for the full
operations guide (HTTPS, password resets, backups, session revocation).

The server requires Redis and is configured via environment variables:

| Variable | Default | Description |
|----------|---------|-------------|
| `LISTEN_ADDR` | `:8080` | Server listen address |
| `REDIS_ADDR` | `localhost:6379` | Redis server address |
| `DATA_DIR` | `./data` | On-disk project Git repositories |
| `PUBLIC_URL` | `http://localhost:8080` | Public origin; sets cookie scope |
| `PREVIEW_PUBLIC_URL` | `http://127.0.0.1:<port>` when `PUBLIC_URL` is localhost; otherwise unset | Origin that serves prototype HTML. Must differ from `PUBLIC_URL`. `off` keeps same-origin preview |
| `COOKIE_SECURE` | `false` | Send session cookies only over HTTPS |

> **HTTPS is required in production.** The server refuses to start with a
> non-loopback `PUBLIC_URL` unless it is `https://` and `COOKIE_SECURE=true`.
> `localhost` may run over plain HTTP for local development.

```bash
export REDIS_ADDR=localhost:6379
export PUBLIC_URL=https://cowork.example
export COOKIE_SECURE=true
export DATA_DIR=/var/lib/agent-link/data

./server
```

## Quick Start

```bash
agentlink register --server https://cowork.example --username kirby
agentlink team create "Product"
agentlink team use tm_a7k3p9d2
agentlink init ./agent_team
agentlink project create "Prototype"
agentlink sync <project_id> ./prototype
```

- `register` prompts for a password twice (read from the terminal, never a flag),
  creates your account, logs this device in, and stores a device session.
- `team create` makes you the team **Owner**, prints a one-time invite code, and
  selects the team. Teammates run `agentlink team join <team_id> <invite_code>`.
- `init` is **local-only**: it creates the `main/`/`worker/` tmux sessions and
  agent config for the already-selected team. It performs no network registration.
- `sync` mirrors a local directory to a team project over WebSocket, acquiring a
  per-file lock before each upload so concurrent edits never clobber each other.

## Authentication Model

- **Accounts** — username + password. Passwords are read from a TTY and are never
  accepted as flags or written to disk.
- **Teams** — every account can belong to multiple teams. Business data lives
  under `/api/teams/{team_id}/...` and is invisible to non-members.
- **Roles** — `Owner`, `Admin`, `Member`. Owners/Admins manage membership and
  invites; ownership can be transferred.
- **CLI device sessions** — `login`/`register` store one opaque device session in
  `~/.agentlink/credentials.json` (mode `0600`). Every request sends
  `Authorization: Device <session>`; the secret never appears in URLs or logs.
- **Web GUI** — browsers authenticate with an HttpOnly session cookie plus a CSRF
  token. Preview is restricted to authenticated members of the owning team.
  On localhost the server serves preview HTML from `127.0.0.1` (a different
  origin) with a short-lived read-only grant, so a prototype script cannot read
  the session or CSRF cookies. Open the GUI at `PUBLIC_URL`, not at the preview
  host. Set `PREVIEW_PUBLIC_URL` to a second hostname in production, or `off` to
  keep the previous same-origin behavior.

## CLI Usage

### Account & Team

```bash
agentlink register --server <url> --username <name> [--device <name>]
agentlink login --server <url> --username <name> [--device <name>]
agentlink logout                       # revoke this device session
agentlink team list                    # list memberships (active team marked *)
agentlink team create <name>           # create + select a team (prints invite)
agentlink team join <team_id> <code>   # join with an invite code
agentlink team use <team_id>           # switch the active team
agentlink team members                 # list members of the active team
agentlink team leave                   # leave the active team
agentlink whoami                       # show account, device, and active team
```

### Workspace, Projects & Sync

```bash
agentlink init [--agent claude] [--no-poll] [--force] [./path]
agentlink project create <name>
agentlink project list
agentlink sync <project_id> <localDir>      # live two-way sync of a directory
agentlink lock acquire|release <project_id> <path>
agentlink lock list <project_id>
```

`init` creates `main/` and `worker/` directories, each with `.agentlink.toml` +
`CLAUDE.md`, starts two tmux sessions running the configured agent (Claude Code by
default), and a background poller per session.

### Messages

```bash
agentlink send [--interrupt] [--title <title>] <target> <content>   # send
agentlink pull [--all]                                              # receive
```

`send` prints a recipient status panel (idle / busy with current task + duration /
offline) and the unread inbox depth. Targets are resolved inside your active team.

### Tasks

```bash
agentlink task send [--interrupt] [--title <title>] <target> [<task_id>] "<content>"
agentlink task result <task_id> <status> "<result>"
agentlink task resume <task_id> "<guidance>"
agentlink task cancel <task_id>
agentlink task reopen <task_id> "<reason>"
agentlink task status <task_id>
agentlink task list
```

### Device & Sessions

```bash
agentlink ping                    # heartbeat (mark online in the active team)
agentlink list [--all]            # list team agents
agentlink session add|remove <n>  # manage local sessions
agentlink attach <session>        # enter a session
agentlink restart                 # rebuild tmux + pollers after reboot
agentlink uninstall               # revoke this device + clean up local files
agentlink poll                    # run poller in foreground
```

### Recovery After Reboot

`agentlink restart` rebuilds tmux sessions and pollers from `~/.agentlink/config.toml`
without re-registering the device. Each session's Claude Code is resumed to its last
recorded `session_id` (under `[sessions]`); older configs fall back to `--continue`.

## Web GUI

Open the server's `PUBLIC_URL` in a browser to register/login, create or switch
teams, manage members, create projects, edit files, and preview prototypes live.
The GUI uses cookie + CSRF authentication only — no tokens are ever shown or pasted.

## Data Retention

Redis data is TTL-bounded to prevent unbounded growth:

| Data | TTL |
|------|-----|
| Inbox messages (unread) | 7 days |
| Delivered message records | 24 hours |
| Completed task records | 30 days |

## Architecture

```
┌─────────┐      ┌──────────────┐     ┌────────┐
│  CLI    │────▶│  API Server  │────▶│  Redis │
│ / GUI   │      └──────────────┘     └────────┘
└─────────┘            │
                  ┌────┴─────┐
                  │ Git repos│ (per-team projects, DATA_DIR)
                  └──────────┘
```

- **Server**: Go net/http + Redis (accounts, teams, sessions, locks, messages,
  tasks) with per-team Git repositories on disk for project files.
- **CLI**: device-session HTTP/WebSocket client; tmux for agent interaction.
- **Poller**: background loop that injects new messages when the agent is idle.
- **Auth**: account + team model. CLI uses an opaque per-device session
  (`Authorization: Device`); the Web GUI uses HttpOnly cookies + CSRF.
```

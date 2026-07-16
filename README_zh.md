# agentlink

面向产品团队的实时协同原型平台。多人同时编辑同一份静态 HTML 原型，秒级同步、
按文件加锁，让本地编码 Agent 不会互相静默覆盖。当前适配 Claude Code。

访问围绕**账号**与**团队**组织：先用密码注册个人账号，再创建或加入团队。所有
项目、文件、锁、消息、任务都归属于某个团队。没有共享注册密码，也不需要复制任何
API token —— CLI 会为你保存每台设备的会话凭据。

## 一键安装（Linux / macOS）

```bash
curl -sfL https://github.com/paparship/agent-link/releases/latest/download/install.sh | sh
```

## 编译

```bash
make build              # 编译 agentlink CLI
make build-server       # 编译 server
make install            # 编译 + 安装到 /usr/local/bin
make uninstall          # 从 /usr/local/bin 移除
make reinstall          # 卸载 → 编译 → 安装
make test               # 运行全部测试
make clean              # 删除编译产物
```

通过 `BINDIR` 自定义安装路径：

```bash
make install BINDIR=~/.local/bin
```

## 部署服务端

systemd 安装见 [docs/deploy-server.md](docs/deploy-server.md)，完整运维指南
（HTTPS、密码重置、备份、会话吊销）见
[docs/team-auth-operations.md](docs/team-auth-operations.md)。

服务端依赖 Redis，通过环境变量配置：

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `LISTEN_ADDR` | `:8080` | 监听地址 |
| `REDIS_ADDR` | `localhost:6379` | Redis 地址 |
| `DATA_DIR` | `./data` | 项目 Git 仓库的磁盘目录 |
| `PUBLIC_URL` | `http://localhost:8080` | 公网 origin，决定 Cookie 作用域 |
| `COOKIE_SECURE` | `false` | 仅在 HTTPS 下下发会话 Cookie |

> **生产环境必须使用 HTTPS。** 当 `PUBLIC_URL` 非回环地址时，若不是 `https://`
> 且 `COOKIE_SECURE=true`，服务端会拒绝启动。本地开发用 `localhost` 可走明文 HTTP。

```bash
export REDIS_ADDR=localhost:6379
export PUBLIC_URL=https://cowork.example
export COOKIE_SECURE=true
export DATA_DIR=/var/lib/agent-link/data

./server
```

## 快速开始

```bash
agentlink register --server https://cowork.example --username kirby
agentlink team create "Product"
agentlink team use tm_a7k3p9d2
agentlink init ./agent_team
agentlink project create "Prototype"
agentlink sync <project_id> ./prototype
```

- `register` 会两次提示输入密码（从终端读取，绝不作为命令行参数），创建账号、
  为本设备登录并保存设备会话。
- `team create` 让你成为团队 **Owner**，打印一次性邀请码并选中该团队。队友执行
  `agentlink team join <team_id> <invite_code>` 加入。
- `init` 是**纯本地**操作：为已选中的团队创建 `main/`、`worker/` 的 tmux 会话与
  Agent 配置，不做任何网络注册。
- `sync` 通过 WebSocket 把本地目录与团队项目双向镜像，每次上传前先获取文件级锁，
  确保并发编辑不会互相覆盖。

## 认证模型

- **账号** —— 用户名 + 密码。密码从 TTY 读取，绝不作为参数传入，也不落盘明文。
- **团队** —— 每个账号可属于多个团队。业务数据位于 `/api/teams/{team_id}/...`，
  对非成员不可见。
- **角色** —— `Owner`、`Admin`、`Member`。Owner/Admin 管理成员与邀请码；可转让
  所有权。
- **CLI 设备会话** —— `login`/`register` 在 `~/.agentlink/credentials.json`
  （权限 `0600`）中保存一个不透明设备会话。每个请求携带
  `Authorization: Device <session>`；密钥绝不出现在 URL 或日志里。
- **Web GUI** —— 浏览器使用 HttpOnly 会话 Cookie + CSRF token 认证；预览仅对
  所属团队的已认证成员开放。

## CLI 使用

### 账号与团队

```bash
agentlink register --server <url> --username <name> [--device <name>]
agentlink login --server <url> --username <name> [--device <name>]
agentlink logout                       # 吊销本设备会话
agentlink team list                    # 列出所属团队（当前团队标 *）
agentlink team create <name>           # 创建并选中团队（打印邀请码）
agentlink team join <team_id> <code>   # 用邀请码加入
agentlink team use <team_id>           # 切换当前团队
agentlink team members                 # 列出当前团队成员
agentlink team leave                   # 退出当前团队
agentlink whoami                       # 查看账号、设备与当前团队
```

### 工作区、项目与同步

```bash
agentlink init [--agent claude] [--no-poll] [--force] [./path]
agentlink project create <name>
agentlink project list
agentlink sync <project_id> <localDir>      # 目录实时双向同步
agentlink lock acquire|release <project_id> <path>
agentlink lock list <project_id>
```

`init` 创建 `main/` 和 `worker/` 目录，各含 `.agentlink.toml` + `CLAUDE.md`，
启动两个运行已配置 agent（默认 Claude Code）的 tmux 会话，并为每个会话附带一个
后台轮询进程。

### 消息

```bash
agentlink send [--interrupt] [--title <title>] <target> <content>   # 发送
agentlink pull [--all]                                              # 接收
```

`send` 输出接收方状态面板（空闲 / 忙碌含当前任务与时长 / 离线）及未读数。目标在
当前团队内解析。

### 任务

```bash
agentlink task send [--interrupt] [--title <title>] <target> [<task_id>] "<content>"
agentlink task result <task_id> <status> "<result>"
agentlink task resume <task_id> "<guidance>"
agentlink task cancel <task_id>
agentlink task reopen <task_id> "<reason>"
agentlink task status <task_id>
agentlink task list
```

### 设备与会话

```bash
agentlink ping                    # 心跳（在当前团队标记在线）
agentlink list [--all]            # 列出团队 agent
agentlink session add|remove <n>  # 管理本地会话
agentlink attach <session>        # 进入会话
agentlink restart                 # 重启后重建 tmux + poller
agentlink uninstall               # 吊销本设备 + 清理本地文件
agentlink poll                    # 前台运行轮询
```

### 重启恢复

`agentlink restart` 从 `~/.agentlink/config.toml` 重建 tmux 会话与 poller，
不重新注册设备。每个会话的 Claude Code 会恢复到上次记录的 `session_id`（`[sessions]`
段）；旧配置自动回退到 `--continue`。

## Web GUI

在浏览器打开服务端的 `PUBLIC_URL`，即可注册/登录、创建或切换团队、管理成员、
创建项目、编辑文件并实时预览原型。GUI 仅用 Cookie + CSRF 认证 —— 不显示、也不需要
粘贴任何 token。

## 数据保留

Redis 数据有 TTL，防止无限堆积：

| 数据 | 保留时长 |
|------|----------|
| 收件箱未读消息 | 7 天 |
| 已投递消息记录 | 24 小时 |
| 已完成任务记录 | 30 天 |

## 架构

```
┌───────────┐     ┌───────────────┐     ┌─────────┐
│ CLI / GUI │────▶│   API 服务端  │────▶│  Redis  │
└───────────┘     └───────────────┘     └─────────┘
                        │
                  ┌─────┴─────┐
                  │  Git 仓库 │ （按团队隔离，位于 DATA_DIR）
                  └───────────┘
```

- **服务端**: Go net/http + Redis（账号、团队、会话、锁、消息、任务），项目文件
  按团队存放于磁盘 Git 仓库。
- **客户端**: 基于设备会话的 HTTP/WebSocket 客户端；通过 tmux 与 agent 交互。
- **轮询器**: 后台循环，在 agent 空闲时注入新消息。
- **认证**: 账号 + 团队模型。CLI 使用不透明的每设备会话
  （`Authorization: Device`）；Web GUI 使用 HttpOnly Cookie + CSRF。
```

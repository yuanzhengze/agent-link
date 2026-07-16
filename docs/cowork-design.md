# 协同原型平台设计（cowork）

> ⚠️ **已被取代（historical record）：** 本文档记录的是**旧版 v1**（共享注册密码
> + API key + Bearer Token）的设计，仅作历史留存，**不再是有效的认证/部署说明**。
> 当前认证与团队模型请以
> [docs/superpowers/specs/2026-07-15-team-auth-design.md](superpowers/specs/2026-07-15-team-auth-design.md)
> 为准（账号 + 团队 + 角色 + 设备会话 + HttpOnly Cookie），部署见
> [docs/deploy-server.md](deploy-server.md) 与
> [docs/team-auth-operations.md](team-auth-operations.md)。

> 在 agentlink 之上增加"项目 + 文件锁 + 同步 + 预览 + GUI"协同层，解决多 PM 同一原型协作时的**秒级同步**与**Agent 互相覆盖**问题。

## 1. 背景与问题

多个产品经理（PM）在同一个原型上协作，各自用本地 coding 工具（Cursor / Claude Code 等）驱动 Agent 改代码，当前有两个痛点：

1. **无法秒级同步**：一个人改了原型，其他人看不到实时结果。
2. **无写锁、互相覆盖**：不同 Agent 改同一文件，因完成时间不同，后写的静默覆盖先写的。

agentlink 已经解决了"跨设备 Agent 通信"（消息 / 任务 / 在线状态 / 任务级忙碌锁），但它是**消息总线**，不管**文件内容**，且锁是**按 Agent**（一个 session 同时只干一个任务）而非**按文件**。本设计在 agentlink 之上补齐缺失的三块：**文件锁、文件同步、GUI**。

## 2. 目标与非目标

### 目标（v1）

- 支持 **≤10 个 PM**、**5 个原型项目并行**。
- 原型是**纯静态 HTML/CSS/JS**（PM 直接写 HTML，不引入构建）。
- **按文件写锁**：同一文件同时只允许一个 `device:session` 写入，防覆盖。
- **秒级同步与预览**：任意改动落库后，秒级广播到服务器预览页与其他人的本地副本。
- **GUI 四件事**：实时预览、文件树+锁状态、在线/状态面板、冲突告警（外加文件树右键的人工占用/释放/接管）。

### 非目标（v1 不做，YAGNI）

- 字符级实时协同编辑（CRDT/OT）——与"防 Agent 覆盖"目标冲突，明确不做。
- 同一文件内的**区域级锁**——留待未来；v1 用"拆分文件"缓解并发。
- 构建型前端预览（React/Vite 的 `npm run dev`）。
- 任务编排 GUI、聊天 GUI、完整活动流/审计。
- FUSE / 文件系统层透明拦截。

## 3. 关键决策摘要

| 议题 | 决策 | 理由 |
|------|------|------|
| 协作对象 | 一套前端**代码文件**（静态 HTML/CSS/JS） | PM 直接写 HTML，本地跑即原型，零构建 |
| 拓扑 | **本地工作副本 + 服务器唯一真相 + 文件锁 + 秒级同步** | 与 agentlink"每设备本地跑 Agent + 中心协调"天然契合 |
| 同步底座 | **服务器唯一 git 提交者 + HTTP 写入(`apply`) + WebSocket 推内容** | git（仅服务器侧）白送历史/回滚；WS 让传播达秒级；客户端免 git、复用 Bearer 鉴权 |
| 锁粒度 | **按文件**（`project:path`） | 防同文件覆盖；不同文件全并行 |
| 锁获取 | **分层**：Agent 主动抢锁为主 + push 时服务器强制校验为底 + GUI/人工接管为辅 + 租约过期 | 强制校验绕不过；Agent 抢锁贴合其工作方式 |
| 复用 agentlink | 服务端 / Redis / 身份 / 鉴权 / 在线 / 消息 / 任务 全部复用 | 不重复造轮子 |

### 串行化的边界（关键澄清）

- 锁是**按文件**的：改**不同文件**的 Agent 完全并行；只有抢**同一文件**才排队。
- 同一文件写入串行**正是防覆盖的手段**；替代方案（实时合并）会把两套矛盾改动缝成谁都不要的结果，退回到静默覆盖。
- **锁只管"写"，不管"读"**：预览是读路径，锁从不阻塞预览或其他文件。持锁者自己每次保存都会提交，其进度也秒级可见。因此**写入按文件串行**与**预览全程秒级**互不冲突。

## 4. 总体架构

```
                          ┌──────────────── 服务器（部署一次）─────────────────┐
                          │  agentlink API (已有)   ┌──────────────┐            │
   PM-A 机器              │   注册/鉴权/消息/任务    │              │            │
 ┌─────────────┐         │   在线状态 (Redis)       │    Redis     │            │
 │ coding 工具  │◀─tmux──│                          │  锁/状态/队列 │            │
 │ (Cursor等)   │         │  【新】锁服务  ─────────▶│              │            │
 │ 本地工作副本 │◀──────▶ │  【新】WebSocket Hub      └──────────────┘            │
 │ 【新】sync   │  HTTP   │  【新】Git 权威仓库 (每项目一个 repo；服务器唯一提交者)│
 │   daemon    │  +WS    │  【新】预览静态托管 (注入 live-reload)                │
 └─────────────┘         │  【新】Web GUI                                        │
   PM-B / PM-C ...       └──────────────────────────────────────────────────────┘
```

### 服务器新增 5 个组件（边界清晰、可独立测试）

| 组件 | 职责 | 依赖 |
|------|------|------|
| 锁服务 | 按 `project:path` 原子获取/释放/续租/过期 | Redis |
| Git 权威仓库 | 每项目一个 repo（真相+历史，**服务器是唯一提交者**）+ 工作树供预览 | 磁盘、git CLI |
| 写入服务（apply） | 校验持锁后，串行把文件内容落到工作树并 commit，广播变更 | 锁服务、Git 权威仓库、WS Hub |
| WebSocket Hub | 秒级广播 `file_changed`（含内容）`/ lock_changed / presence / conflict` | 内存连接表 |
| 预览静态托管 | 把项目工作树按 URL 托管，注入 live-reload 脚本 | Git 工作树、WS Hub |
| Web GUI | 项目列表 + 项目视图（预览/文件树+锁/在线/冲突） | 上述 REST + WS |

### 客户端新增 2 个

| 组件 | 职责 |
|------|------|
| sync daemon (`agentlink sync`) | 监听本地副本→校验持锁→`POST /apply` 上传改动；订阅 WS→把别人的改动写回本地；仿 poller 常驻。**客户端不使用 git** |
| `agentlink lock` 命令 | acquire / release / list；Agent（经 CLAUDE.md 规则）和人都用 |

## 5. 复用 agentlink 的点

- **鉴权与身份**：`device:session` + Bearer API key（`authMiddleware`）。锁归属与 `apply` 写入者身份直接用它。
- **原子锁写法**：照 `taskSendScript` 的 Lua"占用检查"模式，改造成按文件锁。
- **冲突返回**：锁被占的 409 照 `writeBusyError` 模式，返回当前持锁者信息。
- **消息注入**：抢锁失败 / 越权写被拒时，用 agentlink 消息系统自动通知对应 Agent。
- **在线状态**：GUI 的在线/状态面板复用 `GET /agents/list` 的 `current` 字段。
- **心跳**：锁租约的续租搭 agentlink 心跳（~60s）。

## 6. 数据模型

### Redis（沿用 `agentlink:` 前缀）

```
agentlink:lock:<project>:<path>        Hash  { owner, acquired_at, lease_expires_at, task_id? }
                                             owner = "device:session"
agentlink:locks:<device>:<session>     Set   持有的 "project:path"（掉线清理 + 状态展示）
agentlink:project:<id>                 Hash  { name, created_at, head_commit, repo_path }
                                             head_commit = 权威分支 HEAD 的 commit sha（展示/广播用，非逐文件基线）
agentlink:projects                     Set   所有 project id（列表页用）
```

- 获取锁用 Lua 原子：**不存在或已过期 → 写入 owner 并设租约 TTL**；否则返回当前 owner。
- 释放：校验 owner 匹配（或 GUI 强制接管）后删除 Hash 并从 `locks:` Set 移除。
- 租约：`lease_expires_at` 由续租刷新；过期即视为可回收。

### 磁盘

```
<data>/work/<id>/          项目工作树 = 预览托管的根，同时是一个 git 仓库
                           （服务器在此 add+commit；.git 即权威历史）
```

服务器是**唯一提交者**：`apply` 落盘到 `work/<id>/` 后就地 `git commit`，历史/回滚/diff 都在这个仓库的 `.git` 里。客户端不建 clone、不用 git。规模 ≤5 项目 ≤10 人：单机、单 Redis、本地 git 足够，无需分片。

## 7. 核心流程

### 7.1 编辑 → 同步（一次改动）

```
1. Agent 要改 index.html → 按 CLAUDE.md 规则先 `agentlink lock acquire proj1 index.html`
2. 锁服务原子判断：空闲→授予(写 Redis+租约)；被占→返回持锁者，Agent 转去改别的或等待
   （拿到锁后即独占该文件，别人改不了它，故其内容恒为最新）
3. Agent 改本地 index.html
4. sync daemon 侦测变化 → `POST /apply { project, path, content }`（Bearer 鉴权=身份）
5. 服务器 apply：拿项目内存互斥锁 → 校验"path 被本 session 持锁" →
   写 work/<id>/index.html → git add+commit → 释放互斥锁 → WS 广播 file_changed（含内容）
6. 其他设备 daemon 收 WS → 直接把内容写回本地对应文件；GUI 刷新预览与状态
7. Agent 改完 → `agentlink lock release`（或任务结束/租约过期自动释放）
```

**为何并发写不同文件永不冲突**：服务器是唯一提交者，`apply` 由项目级内存互斥锁串行执行；每次 commit 只触及"调用方持锁"的路径，而锁独占 ⇒ 并发的两次 apply 路径必然**不相交**，顺序落盘即可，从不需要三方合并。

### 7.2 锁的三层防线

- **主（Agent 主动抢锁）**：`lock acquire` + 在注入的 `CLAUDE.md` 写明"改任何文件前先抢锁、改完释放"。
- **底（apply 时强制校验）**：无论锁怎么来，`POST /apply` 时服务器校验"调用方持有该文件锁"，不满足即拒绝。这一层保证绕不过去。
- **辅（GUI 可视化 + 人工接管）**：谁锁了哪些文件一目了然；PM 可手动占用、或 Agent 卡死时抢锁/强制释放。
- **租约过期**：持锁者掉线/长时间空闲 → 锁自动过期可回收；GUI 显示"陈旧锁，是否接管"。

### 7.3 预览

- 服务器把 `work/<id>/` 托管到 `/{preview}/<id>/…`。
- 在返回的 HTML 注入一小段 live-reload 脚本，连 WS，收到本项目 `file_changed` 即 `location.reload()`。
- 纯 HTML/CSS/JS，打开即跑，零构建。

## 8. 冲突处理

| 冲突类型 | 触发 | 处理 |
|----------|------|------|
| 锁冲突 | 抢锁时文件已被占 | 返回 409 + 持锁者；Agent 换文件或等；可选注入消息提醒 |
| 越权写（兜底） | `apply` 的 path **未被调用方持锁**（绕过抢锁，或锁被接管后仍在写） | 服务器 **拒绝该 apply（409）**；daemon 保留本地改动、GUI 弹告警 + 注入消息，让 Agent 先抢锁；若期间该文件已被别人改，daemon 先把服务器最新内容写回本地供 Agent 基于最新重做 |
| 陈旧锁 | 持锁者掉线/超时空闲 | 锁租约过期可回收；GUI 提示接管 |

正常流程里**不存在"版本冲突"**：持锁期间该文件独占、无人能改，其内容恒为最新。只有绕过锁（越权写）或锁被接管后仍继续写，才会触发上面的兜底路径。

## 9. 同步机制（服务器唯一提交者 + HTTP写入 + WebSocket 推内容）

- **git = 权威 + 历史（仅服务器侧）**：每项目 `work/<id>/` 就是一个 git 仓库；服务器在 `apply` 时就地 `git add+commit`。历史/回滚/diff 全在这个仓库，客户端不碰 git。
- **写入走 HTTP `POST /apply`**：客户端上传 `{ project, path, content }`；服务器拿**项目级内存互斥锁**串行处理——校验"path 被调用方持锁"→ 写工作树 → commit → 广播。复用 agentlink 的 Bearer API key 鉴权，无需 git-over-HTTP 那套凭据管线。
- **WebSocket = 秒级传播**：commit 后服务器广播 `file_changed`（含 path + 新内容 + 新 `head_commit`）；各 daemon 收到后**直接把内容写回本地文件**，无 fetch 往返。
- **首次进入**：daemon 启动时 `GET /projects/{id}/snapshot` 拉取全量文件内容写到本地工作副本。
- **daemon 侦测**：文件系统 watch（fsnotify）；防抖后对每个变更文件调 `apply`（前提是本 session 已持该文件锁，否则先 `lock acquire`）。
- **并发保证**：唯一提交者 + 互斥锁 + 锁独占 ⇒ 并发 apply 路径不相交，顺序落盘恒无冲突（见 §7.1）。

## 10. GUI（v1）

服务器托管的轻量前端（原生 JS / 极轻量，避免构建）。

- **项目列表页**：≤5 个项目，进入某项目。
- **项目视图（三栏）**：
  - 左：文件树 + 锁徽标（持锁者 + 时长），右键"占用/释放/接管"。
  - 中：live 预览 iframe（秒级刷新）。
  - 右：在线/状态面板（复用 agentlink `current` 状态）+ 冲突告警流。
- 数据：REST（项目/锁/文件树）+ WS（file_changed / lock_changed / presence / conflict）。

## 11. 对 agentlink 的具体改动

### 新增 HTTP 端点

```
POST   /projects                创建项目（git init 工作树 + 首次 commit + Redis 记录）
GET    /projects                列出项目
GET    /projects/{id}/tree      文件树 + 每文件锁状态
GET    /projects/{id}/snapshot  全量文件内容（daemon 首次进入拉取）
POST   /projects/{id}/apply     { path, content } → 校验持锁→写工作树→commit→广播；越权 409
POST   /locks/acquire           { project, path } → 200 授予 / 409 被占(+owner)
POST   /locks/release           { project, path }
GET    /locks/list?project=     该项目所有锁
GET    /ws                      WebSocket（订阅项目事件）
(静态) /preview/<id>/...        预览托管 + 注入 live-reload
```

### 新增 CLI

```
agentlink project create <name>
agentlink project list
agentlink lock acquire <project> <path>
agentlink lock release <project> <path>
agentlink lock list <project>
agentlink sync <project>          # 常驻守护，仿 poller
```

### CLAUDE.md 注入

在已有注入（issue 08）追加锁规则：改任何文件前先 `lock acquire`，完成后 `lock release`；抢不到就改别的文件或稍后重试。

## 12. 技术选型

- **后端**：扩展现有 Go 服务（`net/http` + `go-redis`）。WebSocket 用 `coder/websocket`。git 仅**服务器侧**用 `git` 命令行（`os/exec`）。
- **GUI**：原生 JS / 极轻量，Go 直接托管，避免构建（贴合"少构建"理念）。
- **客户端 daemon**：Go，`fsnotify` 监听，仿 poller 常驻；只用 HTTP + WS，不依赖 git。

## 13. 组件边界与可测试性

每个单元有单一职责、清晰接口，可独立测试：

- **锁服务**：纯函数式判定（acquire/release/expire）+ Redis Lua；可用 miniredis 或真实 Redis 集成测。
- **Git 权威层**：封装 `git init` 工作树、apply（校验持锁→写→commit）、snapshot/tree 读取；可用临时目录测。
- **WS Hub**：连接注册 + 广播；可用内存连接 mock 测。
- **sync daemon**：watch→apply / 收 WS→写回 状态机；可注入假 watcher / 假 HTTP 测。
- **GUI**：与后端通过 REST/WS 契约解耦。

## 14. 并发瓶颈与未来区域锁

- 唯一真正代价：**大家死磕同一文件**时串行成瓶颈。
- v1 缓解：鼓励原型**按页面/组件拆分成多个文件**，天然摊开并发。
- 未来（非 v1）：同一文件内**区域级锁**（按代码块/区间），进一步降低同文件竞争。

## 15. 术语

- **device:session**：agentlink 的身份单元，一个设备上的一个 Agent 会话（如 `bob-pc:main`）。锁的归属者、apply 的写入者。
- **工作树 / 权威 repo**：服务器上每项目的 `work/<id>/` 目录，本身是 git 仓库；服务器是唯一提交者，`.git` 即权威历史。预览也托管这个目录。
- **apply**：客户端把单个文件的新内容提交给服务器的写入操作（`POST /projects/{id}/apply`），服务器校验持锁后串行落盘并广播。
- **租约（lease）**：锁的有效期，靠心跳续租；过期即可回收。

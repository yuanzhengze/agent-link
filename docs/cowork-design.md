# 协同原型平台设计（cowork）

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
| 同步底座 | **git 存历史/权威 + WebSocket 负责秒级广播**（混合） | git 白送历史/回滚/成熟合并；WS 让传播达秒级 |
| 锁粒度 | **按文件**（`project:path`） | 防同文件覆盖；不同文件全并行 |
| 锁获取 | **分层**：Agent 主动抢锁为主 + push 时服务器强制校验为底 + GUI/人工接管为辅 + 租约过期 | 强制校验绕不过；Agent 抢锁贴合其工作方式 |
| 复用 agentlink | 服务端 / Redis / 身份 / 鉴权 / 在线 / 消息 / 任务 全部复用 | 不重复造轮子 |

### 串行化的边界（关键澄清）

- 锁是**按文件**的：改**不同文件**的 Agent 完全并行；只有抢**同一文件**才排队。
- 同一文件写入串行**正是防覆盖的手段**；替代方案（实时合并）会把两套矛盾改动缝成谁都不要的结果，退回到静默覆盖。
- **锁只管"写"，不管"读"**：预览是读路径，锁从不阻塞预览或其他文件。持锁者自己每次保存都会 push，其进度也秒级可见。因此**写入按文件串行**与**预览全程秒级**互不冲突。

## 4. 总体架构

```
                          ┌──────────────── 服务器（部署一次）─────────────────┐
                          │  agentlink API (已有)   ┌──────────────┐            │
   PM-A 机器              │   注册/鉴权/消息/任务    │              │            │
 ┌─────────────┐         │   在线状态 (Redis)       │    Redis     │            │
 │ coding 工具  │◀─tmux──│                          │  锁/状态/队列 │            │
 │ (Cursor等)   │         │  【新】锁服务  ─────────▶│              │            │
 │ 本地工作副本 │◀──────▶ │  【新】WebSocket Hub      └──────────────┘            │
 │ 【新】sync   │  git    │  【新】Git 权威仓库 (每项目一个 bare repo + 工作树)   │
 │   daemon    │  +WS    │  【新】预览静态托管 (注入 live-reload)                │
 └─────────────┘         │  【新】Web GUI                                        │
   PM-B / PM-C ...       └──────────────────────────────────────────────────────┘
```

### 服务器新增 5 个组件（边界清晰、可独立测试）

| 组件 | 职责 | 依赖 |
|------|------|------|
| 锁服务 | 按 `project:path` 原子获取/释放/续租/过期 | Redis |
| Git 权威仓库 | 每项目一个 bare repo（真相+历史）+ 签出工作树供预览 | 磁盘、git CLI |
| WebSocket Hub | 秒级广播 `file_changed / lock_changed / presence / conflict` | Redis pub/sub（可选）、内存连接表 |
| 预览静态托管 | 把项目工作树按 URL 托管，注入 live-reload 脚本 | Git 工作树、WS Hub |
| Web GUI | 项目列表 + 项目视图（预览/文件树+锁/在线/冲突） | 上述 REST + WS |

### 客户端新增 2 个

| 组件 | 职责 |
|------|------|
| sync daemon (`agentlink sync`) | 监听本地副本→校验持锁+基线最新→commit→push；订阅 WS→pull 别人的改动；仿 poller 常驻 |
| `agentlink lock` 命令 | acquire / release / list；Agent（经 CLAUDE.md 规则）和人都用 |

## 5. 复用 agentlink 的点

- **鉴权与身份**：`device:session` + Bearer API key（`authMiddleware`）。锁归属直接用它。
- **原子锁写法**：照 `taskSendScript` 的 Lua"占用检查"模式，改造成按文件锁。
- **冲突返回**：锁被占的 409 照 `writeBusyError` 模式，返回当前持锁者信息。
- **消息注入**：抢锁失败 / 版本冲突时，用 agentlink 消息系统自动通知对应 Agent。
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
<data>/repos/<id>.git      bare 权威仓库（push 目标 + 历史）
<data>/work/<id>/          签出工作树（预览托管的根；push 后服务器更新它）
```

规模 ≤5 项目 ≤10 人：单机、单 Redis、本地 git 足够，无需分片。

## 7. 核心流程

### 7.1 编辑 → 同步（一次改动）

```
1. Agent 要改 index.html → 按 CLAUDE.md 规则先 `agentlink lock acquire proj1 index.html`
2. 锁服务原子判断：空闲→授予(写 Redis+租约)；被占→返回持锁者，Agent 转去改别的或等待
   授予成功后，daemon 先 git pull 保证本地该文件是最新（此后独占，别人改不了它）
3. Agent 改本地 index.html
4. sync daemon 侦测变化 → git commit → push 到服务器
5. 服务器接收 push：校验"改动的所有路径都被本 session 持锁"→ 合并进权威分支
   （因锁独占，各人改动路径互不相交，合并必然无冲突）→ 更新 work/<id>/、head_commit、WS 广播 file_changed
6. 其他设备 daemon 收 WS → git pull 同步（同样因路径不相交，pull 必然干净）；GUI 刷新预览与状态
7. Agent 改完 → `agentlink lock release`（或任务结束/租约过期自动释放）
```

**为何并行推送不同文件不会互相拒绝**：客户端不要求 push 是 fast-forward。服务器在接收侧做校验 + 合并——只要改动路径都在推送者持有的锁内，而锁又是独占的，任意两次并发推送触及的路径必然**不相交**，因此合并（和其他人的 pull）**永远干净**、无需人工解决。

### 7.2 锁的三层防线

- **主（Agent 主动抢锁）**：`lock acquire` + 在注入的 `CLAUDE.md` 写明"改任何文件前先抢锁、改完释放"。
- **底（push 时强制校验）**：无论锁怎么来，push 时服务器校验"持有这些文件锁 + 基线版本最新"，不满足即拒绝。这一层保证绕不过去。
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
| 越权写（兜底） | push 里含**未持锁**的路径（绕过锁直接改，或锁被接管后仍在改） | 服务器**拒绝整个 push**；daemon 把本地改动 stash → pull 最新 → GUI 弹告警 + 注入消息，让 Agent 先抢锁再基于最新重做 |
| 陈旧锁 | 持锁者掉线/超时空闲 | 锁租约过期可回收；GUI 提示接管 |

正常流程里**不存在"版本冲突"**：持锁者在 acquire 时已 pull 到最新，且持锁期间该文件独占、无人能改，故其基线恒为最新。只有绕过锁（越权写）或锁被接管后仍继续写，才会触发上面的兜底路径。

## 9. 同步机制（git + WebSocket 混合）

- **git = 权威 + 历史**：每项目一个服务器 bare repo；客户端本地是它的 clone。写入通过 `git push`，落到权威 repo。历史/回滚/diff 白送。
- **接收侧合并（非 fast-forward 要求）**：客户端各自基于本地 HEAD 提交并 push；服务器在接收侧校验"改动路径 ⊆ 推送者持锁"后，把提交**合并**进权威分支。因锁独占 ⇒ 并发推送路径不相交 ⇒ 合并恒无冲突。
- **WebSocket = 秒级传播**：合并落库后服务器广播 `file_changed`（含新 `head_commit`）；各 daemon 收到后 `git pull` 同步（同样恒干净）。避免"定时轮询"的秒级延迟。
- **push 鉴权与校验**：git 走 HTTP（git-http-backend 或自建代理），复用 agentlink 的 Bearer API key；"改动路径 ⊆ 持锁"的校验在 pre-receive 钩子或代理层执行，不满足则拒收。
- **daemon 侦测**：文件系统 watch（fsnotify）；防抖后 commit+push。

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
POST   /projects                创建项目（建 bare repo + 工作树 + Redis 记录）
GET    /projects                列出项目
GET    /projects/{id}/tree      文件树 + 每文件锁状态
POST   /locks/acquire           { project, path } → 200 授予 / 409 被占(+owner)
POST   /locks/release           { project, path }
GET    /locks/list?project=     该项目所有锁
GET    /ws                      WebSocket（订阅项目事件）
(git)  /git/<id>.git/...        git-http-backend / 代理（push 处插锁+版本校验）
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

- **后端**：扩展现有 Go 服务（`net/http` + `go-redis`）。WebSocket 用 `coder/websocket`。git 用 `git` 命令行（`os/exec`）。
- **GUI**：原生 JS / 极轻量，Go 直接托管，避免构建（贴合"少构建"理念）。
- **客户端 daemon**：Go，`fsnotify` 监听，仿 poller 常驻。

## 13. 组件边界与可测试性

每个单元有单一职责、清晰接口，可独立测试：

- **锁服务**：纯函数式判定（acquire/release/expire）+ Redis Lua；可用 miniredis 或真实 Redis 集成测。
- **Git 权威层**：封装 bare repo 初始化、push 接收校验、工作树更新；可用临时目录测。
- **WS Hub**：连接注册 + 广播；可用内存连接 mock 测。
- **sync daemon**：watch→commit→push 状态机；可注入假 watcher / 假 git 测。
- **GUI**：与后端通过 REST/WS 契约解耦。

## 14. 并发瓶颈与未来区域锁

- 唯一真正代价：**大家死磕同一文件**时串行成瓶颈。
- v1 缓解：鼓励原型**按页面/组件拆分成多个文件**，天然摊开并发。
- 未来（非 v1）：同一文件内**区域级锁**（按代码块/区间），进一步降低同文件竞争。

## 15. 术语

- **device:session**：agentlink 的身份单元，一个设备上的一个 Agent 会话（如 `bob-pc:main`）。锁的归属者。
- **权威 repo**：服务器上每项目的 bare git 仓库，唯一真相。
- **工作树**：服务器上该项目的签出目录，预览托管的根。
- **租约（lease）**：锁的有效期，靠心跳续租；过期即可回收。

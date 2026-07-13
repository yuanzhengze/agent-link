# 协同原型平台（cowork）实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: 用 superpowers:subagent-driven-development（推荐）或 superpowers:executing-plans 按任务逐个实现。步骤用 `- [ ]` 复选框跟踪。

**Goal:** 在 agentlink 之上增加"项目 + 文件锁 + 同步 + 预览 + GUI"协同层，让多 PM 在同一静态 HTML 原型上秒级同步、且 Agent 按文件串行写入不互相覆盖。

**Architecture:** 服务器是唯一 git 提交者——客户端 `agentlink sync` 守护进程侦测本地改动，校验持锁后 `POST /apply` 上传文件内容；服务器串行落盘 `work/<id>/` 并 `git commit`，再经 WebSocket 把内容广播给其他客户端和预览页。按文件锁（Redis + Lua 原子）在 `apply` 时强制校验，绕不过去。

**Tech Stack:** Go（`net/http` + `go-redis`，扩展现有 `pkg/api`、`pkg/cli`）、Redis（沿用 `agentlink:` 前缀）、`coder/websocket`（WS）、`fsnotify`（客户端文件监听）、`git` CLI（仅服务器侧 `os/exec`）、原生 JS GUI（Go 托管，免构建）。

对照设计文档 `docs/cowork-design.md`。

## Global Constraints

- 规模：≤10 个 PM，并行支持 **5 个原型项目**。
- 原型是**纯静态 HTML/CSS/JS**，无构建；客户端**不使用 git**。
- 锁粒度**按文件**（`project:path`），同一文件同时只允许一个 `device:session` 写。
- **服务器是唯一 git 提交者**；写入走 `POST /projects/{id}/apply`，越权（未持锁）写入返回 409。
- 复用 agentlink：`device:session` 身份 + Bearer API key（`authMiddleware`）、Redis `agentlink:` 前缀、Lua 原子写法（照 `taskSendScript`）、`writeBusyError` 冲突面板、`GET /agents/list` 在线状态、心跳续租。
- 每步做完跑 `go test ./... -count=1 -race`，不破坏现有测试。频繁提交。

---

## 现状速览

**agentlink 已有（直接复用，不改）**：
- 服务端 Go + Redis，`device:session` 身份，Bearer API key 鉴权（`pkg/api/handlers.go authMiddleware`）。
- Redis Lua 原子写法范例（`taskSendScript`）、409 冲突面板（`writeBusyError`）。
- 在线状态 `GET /agents/list` 的 `current` 字段、心跳（~60s）。
- CLI HTTP 客户端 helper `apiDo`（`pkg/cli/client.go`）、tmux/poller 常驻范式（`pkg/cli/runtime/poller.go`）、CLAUDE.md 注入（`pkg/adapter/claude.go InitTemplate`）。

**缺口（本计划补齐）**：
- 无项目概念、无文件内容存储/同步。
- 锁是按 Agent（任务忙碌锁），非按文件。
- 无秒级推送（poller 5s 轮询）、无 GUI、无预览。

## 依赖（go get）

```
go get github.com/coder/websocket
go get github.com/fsnotify/fsnotify
```

git CLI 为系统依赖（仅服务器需要）。

## 文件结构

**新增（服务端 `pkg/api/`）**：
- `projects.go` — 项目模型、`git init` 工作树、create/list/tree/snapshot。
- `locks.go` — 文件锁 acquire/release/list（Lua 原子 + 租约）。
- `apply.go` — 写入服务：校验持锁→写工作树→commit→广播；项目级互斥锁。
- `ws.go` — WebSocket Hub + `Event` 结构 + `Broadcaster` 接口实现。
- `preview.go` — 静态预览托管 + live-reload 注入。

**修改（服务端）**：
- `server.go` — 注册新路由；`Server` 加 `dataDir`、`hub` 字段。
- `cmd/server/main.go` — 读 `DATA_DIR` 环境变量传入 `Server`。

**新增（客户端 `pkg/cli/`）**：
- `net/projects.go`、`net/locks.go` — HTTP 客户端。
- `runtime/sync.go` — sync daemon（fsnotify + apply + WS 写回）。

**修改（客户端）**：
- `cmd/agentlink/main.go` — 加 `project` / `lock` / `sync` 子命令 + usage。
- `pkg/adapter/claude.go` — `InitTemplate` 追加锁规则。

**新增（GUI）**：
- `web/index.html`、`web/app.js`、`web/style.css` — 原生 JS，Go 经 `embed` 托管。

**任务 → issue 映射**（沿用本仓库 `issues/NN-*.md` 惯例，续到 32+）：

| 任务 | issue | 交付物 |
|------|-------|--------|
| 1 项目模型+工作树 | 32 | 项目 CRUD + git init |
| 2 文件锁服务 | 33 | acquire/release/list |
| 3 apply 写入服务 | 34 | 持锁校验+commit+广播 |
| 4 WebSocket Hub | 35 | 事件广播 |
| 5 tree+snapshot | 36 | 文件树/全量内容 |
| 6 预览托管+live-reload | 37 | 秒级刷新预览 |
| 7 CLI project/lock | 38 | 命令行 |
| 8 CLAUDE.md 锁规则 | 39 | 注入规则 |
| 9 sync daemon | 40 | 客户端常驻同步 |
| 10 Web GUI | 41 | 四件套界面 |
| 11 端到端集成+文档 | 42 | E2E 测试 + README |

---

## 任务清单（按依赖顺序）

### 任务 1：项目模型 + git init 工作树

**文件**：`pkg/api/projects.go`（新）、`pkg/api/server.go`（路由 + `dataDir` 字段）、`cmd/server/main.go`（`DATA_DIR` 环境变量）

**Redis**：
```
agentlink:project:<id>   Hash  { id, name, created_at, head_commit }
agentlink:projects       Set   所有 project id
```

**磁盘**：`<DATA_DIR>/work/<id>/`——create 时 `mkdir` → `git init` → `git config user.name cowork` / `user.email cowork@localhost` → 写一个 `index.html` 种子文件 → 首次 `git add -A && git commit`，记录 `head_commit`。

**接口（Produces，后续任务依赖）**：
```go
// Server 结构新增字段
type Server struct {
    // ...existing...
    dataDir string
    hub     Broadcaster // 任务 4 注入；任务 1 先留 nil
}
func (s *Server) projectDir(id string) string // 返回 <dataDir>/work/<id>
func (s *Server) gitCommit(dir, msg string) (sha string, err error) // git add -A && commit，返回 HEAD sha
```

**端点**：
```
POST /projects   body {name} → 200 {id, name, head_commit}
GET  /projects   → 200 {projects:[{id,name,created_at,head_commit}]}
```
- id：`generateID()[:8]`（复用 handlers.go）。
- git 提交身份固定为 `cowork <cowork@localhost>`（在 repo 内 `git config` 设置，不改全局）。

**测试**（`pkg/api/projects_test.go`）：
- `TestCreateProject_ok`：POST → 200，`<dataDir>/work/<id>/.git` 存在、`index.html` 存在、Redis `project:<id>` 与 `projects` Set 有记录、`head_commit` 非空。
- `TestCreateProject_missingName_400`。
- `TestListProjects`：建 2 个 → GET 返回 2 个。

**验收**：能创建项目并在磁盘得到一个含首次提交的 git 工作树；列表正确。

---

### 任务 2：文件锁服务

**文件**：`pkg/api/locks.go`（新）、`pkg/api/server.go`（路由）

**Redis**：
```
agentlink:lock:<project>:<path>     Hash { owner, acquired_at, lease_expires_at, task_id }
agentlink:locks:<device>:<session>  Set  持有的 "project|path"
```
owner = `device:session`。租约默认 120s。

**Lua（原子获取，照 taskSendScript 风格）**：
```lua
-- KEYS[1]=agentlink:lock:<project>:<path>  KEYS[2]=agentlink:locks:<device>:<session>
-- ARGV[1]=owner  ARGV[2]=now(unix)  ARGV[3]=lease_expires(unix)
-- ARGV[4]=member("project|path")  ARGV[5]=task_id
local cur = redis.call('HGET', KEYS[1], 'owner')
local exp = tonumber(redis.call('HGET', KEYS[1], 'lease_expires_at') or '0')
if cur and cur ~= ARGV[1] and exp > tonumber(ARGV[2]) then
  return {0, cur}                      -- 被别人持有且未过期
end
redis.call('HSET', KEYS[1], 'owner', ARGV[1], 'acquired_at', ARGV[2],
  'lease_expires_at', ARGV[3], 'task_id', ARGV[5])
redis.call('SADD', KEYS[2], ARGV[4])
return {1, ARGV[1]}                     -- 授予（新建/续租/过期回收）
```

**Lua（释放）**：
```lua
-- KEYS[1]=lock  KEYS[2]=locks set  ARGV[1]=owner  ARGV[2]=member  ARGV[3]=force("1"/"0")
local cur = redis.call('HGET', KEYS[1], 'owner')
if cur == false then return {1, ''} end
if cur ~= ARGV[1] and ARGV[3] ~= '1' then return {0, cur} end
redis.call('DEL', KEYS[1]); redis.call('SREM', KEYS[2], ARGV[2])
return {1, ''}
```

**接口（Produces）**：
```go
// owner 内部辅助
func lockKey(project, path string) string    // agentlink:lock:<project>:<path>
func (s *Server) lockOwner(ctx, project, path string) (owner string, expired bool)
```

**端点**：
```
POST /locks/acquire  body {project, session, path, task_id?} → 200 {owner} / 409 {error, owner}
POST /locks/release  body {project, session, path, force?}    → 200 {ok:true} / 409 {error, owner}
GET  /locks/list?project=<id>                                 → 200 {locks:[{path,owner,acquired_at,lease_expires_at}]}
```
- device 来自 auth context；session 来自 body；owner = `device+":"+session`。
- 409 复用 `writeBusyError` 风格，body 带 `owner`。

**测试**（`pkg/api/locks_test.go`）：
- `TestLockAcquire_free`：空闲文件 → 200，Redis lock 有 owner，locks set 含 member。
- `TestLockAcquire_heldByOther_409`：A 持有 → B acquire 同文件返回 409 且 body.owner=A。
- `TestLockAcquire_reacquire_refresh`：owner 再 acquire → 200，lease_expires_at 被刷新。
- `TestLockAcquire_expired_reclaim`：手动把 lease 设为过去 → 他人 acquire → 200。
- `TestLockRelease_owner`：owner 释放 → 200，lock 删除、member 移除。
- `TestLockRelease_nonOwner_409`；`TestLockRelease_force`：force=1 非 owner 也能释放。
- `TestLockAcquire_concurrent`：10 goroutine 并发 acquire 同文件 → 恰 1 个成功。`go test -race`。
- `TestLockList`。

**验收**：并发下同一文件恒一个持有者；租约过期可回收；`-race` 通过。

---

### 任务 3：apply 写入服务

**文件**：`pkg/api/apply.go`（新）、`pkg/api/server.go`（路由 + 项目互斥锁 map）

**依赖**：任务 1（工作树 + gitCommit）、任务 2（lockOwner）。广播用**接口**（任务 4 实现），本任务先定义并在测试里用 mock。

**接口（Produces / Consumes）**：
```go
// 任务 4 将实现；本任务定义并在 server 里持有一个字段
type Event struct {
    Type       string `json:"type"`        // "file_changed" | "lock_changed" | "conflict"
    Project    string `json:"project"`
    Path       string `json:"path,omitempty"`
    Content    string `json:"content,omitempty"`
    HeadCommit string `json:"head_commit,omitempty"`
    By         string `json:"by,omitempty"`    // device:session
    Owner      string `json:"owner,omitempty"` // lock_changed 用
    At         string `json:"at"`
}
type Broadcaster interface{ Broadcast(project string, ev Event) }
```

**项目互斥锁**：`server.go` 加 `projMu sync.Map // projectID -> *sync.Mutex`，helper `func (s *Server) projectLock(id string) *sync.Mutex`。

**端点**：
```
POST /projects/{id}/apply  body {session, path, content} → 200 {head_commit} / 409 {error, owner} / 400
```
**流程**：
1. device 来自 auth；owner = `device:session`。
2. `path` 安全校验：非空、不含 `..`、非绝对路径、`filepath.Clean` 后仍在项目内 → 否则 400。
3. `lockOwner(project, path)`：owner 不匹配或已过期 → 409（`writeBusyError` 风格，带当前 owner）。
4. `s.projectLock(id).Lock()`（defer Unlock）。
5. 写 `work/<id>/<path>`（`os.MkdirAll` 父目录 + `os.WriteFile`）。
6. `sha := s.gitCommit(dir, "apply "+path+" by "+owner)`；更新 `project:<id>` 的 `head_commit`。
7. `s.hub.Broadcast(id, Event{Type:"file_changed", Project:id, Path:path, Content:content, HeadCommit:sha, By:owner, At:now})`。
8. 返回 `{head_commit: sha}`。

**测试**（`pkg/api/apply_test.go`，用 mock Broadcaster 收集事件）：
- `TestApply_success`：先 acquire 锁 → apply → 200，文件写入、git log 多一条、head_commit 更新、mock 收到 file_changed（含 content）。
- `TestApply_notLocked_409`：未持锁 apply → 409。
- `TestApply_lockedByOther_409`。
- `TestApply_pathTraversal_400`：path=`../x` → 400。
- `TestApply_concurrentDifferentFiles`：两个 session 各持不同文件锁并发 apply → 都 200，git log 两条提交。

**验收**：持锁才能写；越权 409；并发不同文件均成功且历史完整。

---

### 任务 4：WebSocket Hub + 事件广播

**文件**：`pkg/api/ws.go`（新，实现任务 3 的 `Broadcaster`）、`pkg/api/server.go`（路由 + `s.hub = NewHub()`，注入 apply）

**实现**：
```go
type Hub struct {
    mu   sync.RWMutex
    subs map[string]map[*conn]struct{} // project -> set of conns
}
func NewHub() *Hub
func (h *Hub) Broadcast(project string, ev Event) // JSON 编码后写给该 project 所有订阅者
func (h *Hub) handleWS(w http.ResponseWriter, r *http.Request) // 接受连接，按 ?project= 订阅
```
- 用 `coder/websocket`：`websocket.Accept` → 读 `?project=`、`?token=` → 校验 token（`agentlink:api_key:<sha256>`，浏览器无法设 WS 头，故用 query token）→ 注册到 `subs[project]` → 阻塞读直到断开，断开时注销。
- 写超时保护；单连接写失败即注销该连接。

**端点**：`GET /ws?project=<id>&token=<api_key>`（在 `authMiddleware` 的 `skipAuth` 白名单里放行 `/ws`，改为在 handler 内自行用 query token 校验）。

**接线**：`server.go` 里 `New()` 建 `hub := NewHub()`，赋给 `s.hub`；apply 用它广播。

**测试**（`pkg/api/ws_test.go`，用 `httptest.NewServer`）：
- `TestWS_receivesBroadcast`：连 `/ws?project=p1` → `hub.Broadcast("p1", ev)` → 客户端读到该 JSON。
- `TestWS_projectFilter`：订阅 p1 的连接不应收到 p2 的广播。
- `TestWS_badToken_401`。

**验收**：广播秒达订阅者；按项目过滤；鉴权生效。

---

### 任务 5：文件树 + 快照 API

**文件**：`pkg/api/projects.go`（追加两个 handler）、`server.go`（路由）

**端点**：
```
GET /projects/{id}/tree     → 200 {files:[{path, locked, owner}]}   // 遍历 work/<id>/，跳过 .git，附锁状态
GET /projects/{id}/snapshot → 200 {head_commit, files:[{path, content}]}
```
- tree：`filepath.WalkDir` work 目录，收集相对路径，跳过 `.git/`；对每个文件查 `lockOwner`。
- snapshot：同样遍历，读取内容（文本文件；二进制以 base64？v1 只静态文本，先按 UTF-8 文本读取，二进制留 §范围外）。

**测试**（`pkg/api/projects_test.go` 追加）：
- `TestTree_listsFilesWithLocks`：建项目 + 写文件 + acquire 一个锁 → tree 里该文件 `locked=true, owner=...`。
- `TestTree_skipsGitDir`：结果不含 `.git/...`。
- `TestSnapshot_returnsAllContents`：内容与磁盘一致，含 head_commit。

**验收**：tree 反映锁状态；snapshot 供 daemon 首次拉全量。

---

### 任务 6：静态预览托管 + live-reload 注入

**文件**：`pkg/api/preview.go`（新）、`server.go`（路由 `/preview/{id}/`）

**实现**：
```go
func (s *Server) handlePreview(w http.ResponseWriter, r *http.Request)
```
- 从 URL 解析 `<id>` 与相对路径（默认 `index.html`）；`filepath.Clean` 防穿越，限制在 `work/<id>/` 内，否则 404。
- 读取文件；若 `.html`：在 `</body>` 前（找不到则末尾）注入：
```html
<script>
(function(){var ws=new WebSocket((location.protocol==='https:'?'wss':'ws')+'://'+location.host+'/ws?project=<ID>&token=<PREVIEW_TOKEN>');
ws.onmessage=function(e){try{var m=JSON.parse(e.data);if(m.type==='file_changed'&&m.project==='<ID>')location.reload();}catch(_){}}})();
</script>
```
- 预览页 token：v1 用只读的预览 token（服务器启动配置一个 `PREVIEW_TOKEN`，或复用注册密码派生的只读 token）。简化：`/ws` 对预览来源放宽为"只订阅、不触发任何写"，用一个固定只读 token。
- 非 html 按 content-type 原样返回，不注入。

**测试**（`pkg/api/preview_test.go`）：
- `TestPreview_servesFile`；`TestPreview_injectsLiveReloadIntoHTML`（响应含 `new WebSocket` 且含正确 `<ID>`）。
- `TestPreview_nonHtmlNotInjected`（css 不含脚本）。
- `TestPreview_pathTraversal_404`。

**验收**：打开预览 URL 能看到原型；改动经 WS 触发 `location.reload()`。

---

### 任务 7：CLI net 客户端 + project/lock 子命令

**文件**：`pkg/cli/net/projects.go`、`pkg/cli/net/locks.go`（新，复用 `apiDo`）、`cmd/agentlink/main.go`（`cmdProject`/`cmdLock` + usage）

**命令**：
```
agentlink project create <name>
agentlink project list
agentlink lock acquire <project> <path>   # session 取自本设备当前 session（config）
agentlink lock release <project> <path>
agentlink lock list <project>
```

**接口（Consumes）**：`apiDo(cfg, creds, method, url, body)`（`pkg/cli/client.go` 已有）。

**测试**：
- `pkg/cli/net` 层：`TestNetProjectCreate`/`TestNetLockAcquire`（对 `httptest` 假服务器断言请求方法/路径/体、解析响应）。
- `cmd/agentlink`：dispatch 冒烟（沿用现有 main 测试缺口注记，优先级低）。

**验收**：命令能建项目、抢/放/列锁，输出清晰。

---

### 任务 8：CLAUDE.md 注入锁规则

**文件**：`pkg/adapter/claude.go`（`InitTemplate` 追加）

**追加文本**（写入各 session 的 CLAUDE.md）：
```
## 协同写锁规则（cowork）
- 修改任何文件前，先执行：agentlink lock acquire <project> <相对路径>
- 若返回 409（被占用），改去做别的文件，或稍后重试；不要强行修改。
- 改完后执行：agentlink lock release <project> <相对路径>
- 不要绕过锁直接改文件——服务器会拒绝未持锁的写入（apply 409）。
```

**测试**（`pkg/adapter/claude_test.go` 追加）：
- `TestClaudeInitTemplate_containsLockRules`：`InitTemplate("main")` 含 `lock acquire`、`lock release`、`409`。

**验收**：新 init 的 session 的 CLAUDE.md 含锁规则。

---

### 任务 9：sync daemon（agentlink sync）

**文件**：`pkg/cli/runtime/sync.go`（新）、`cmd/agentlink/main.go`（`cmdSync`）

**依赖**：任务 3（apply）、任务 4（WS）、任务 5（snapshot）、任务 7（lock 客户端）。

**逻辑**：
```go
func RunSync(cfg Config, creds Creds, project, localDir, session string) error
```
1. 启动：`GET /projects/{id}/snapshot` → 把每个文件写到 `localDir`（记录内容 hash，见回写防抖）。
2. `fsnotify` 监听 `localDir`（递归；忽略隐藏文件）。
3. 本地变更（防抖 ~300ms 合并）：对每个变更文件
   - 若本 session 未持锁 → 先 `lock acquire`（失败则跳过并提示）。
   - 读内容；若与"最近由 WS 写回的内容 hash"相同 → **跳过**（避免回写→触发→再 apply 的死循环）。
   - `POST /projects/{id}/apply {session, path, content}`；409 越权则告警。
4. WS：连 `/ws?project=<id>&token=<api_key>`；收到 `file_changed` 且 `by != 自己`：
   - 记录 `path→hash(content)` 到"最近写回"表；写文件到 `localDir`。
5. 断线重连 + 退出清理。

**回写防抖关键**：维护 `map[path]lastWrittenHash`；WS 写回前后更新它，fsnotify 侧比对后跳过自身回写。

**测试**（`pkg/cli/runtime/sync_test.go`，注入假 HTTP + 手动触发事件）：
- `TestSync_snapshotWritesFiles`。
- `TestSync_localChangeCallsApply`（假 HTTP 断言收到 apply，含 path/content）。
- `TestSync_wsWritesRemoteFile`（喂一个 file_changed → 本地文件被写入）。
- `TestSync_ignoresEchoedChange`（WS 写回后触发的 fsnotify 事件不再 apply）。

**验收**：本地改动秒级上行、远端改动秒级下行、无回写死循环。

---

### 任务 10：Web GUI

**文件**：`web/index.html`、`web/app.js`、`web/style.css`（新）、`server.go`（`embed` 托管 `/`）

**功能（v1 四件套 + 文件树右键锁操作）**：
- 项目列表页：`GET /projects`。
- 项目视图三栏：
  - 左：文件树（`GET /projects/{id}/tree`）+ 锁徽标（owner+时长）；右键 acquire/release（`force` 接管）。
  - 中：`<iframe src="/preview/{id}/">`。
  - 右：在线面板（`GET /agents/list` 的 `current`）+ 冲突告警流（WS `conflict`）。
- WS：连 `/ws?project=<id>&token=` 实时更新文件树锁状态与告警。

**托管**：`//go:embed web/*` + `http.FileServer`，路由 `/`（在 `authMiddleware` 白名单放行静态资源；数据接口仍需 token，GUI 在前端保存 token）。

**测试**：
- `TestServeGUI_indexOk`：`GET /` 返回 200 且含预期挂载点。
- GUI 交互为手动/e2e，不写单测。

**验收**：能进项目、看到预览秒级刷新、看到谁锁了什么、能手动抢/放锁。

---

### 任务 11：端到端集成 + 文档

**文件**：`pkg/api/e2e_test.go`（新）、`README.md` / `README_zh.md`（追加 cowork 用法）

**集成测试**（真实 Redis + 临时 DATA_DIR）：
- `TestE2E_twoClientsDisjointFiles`：建项目 → sessionA acquire a.html、sessionB acquire b.html → 各 apply → 两次提交都在、tree 两文件、WS 各收到广播。
- `TestE2E_sameFileLockConflict`：A 持 index.html → B acquire index.html 409 → B apply index.html 409。

**文档**：README 增加"cowork：创建项目 / 抢锁 / sync / 预览 / GUI"小节。

**验收**：端到端主链路自动化通过；README 可照做。

---

## 实施顺序

按依赖：**1 → 2 → 3 → 4 → 5 → 6 → 7 → 8 → 9 → 10 → 11**。

- 1、2 是地基（项目 + 锁），可并行但先 1 后 2 稳妥。
- 3 依赖 1+2，并定义 `Broadcaster` 接口（用 mock 测）。
- 4 实现并接线 `Broadcaster`。
- 5、6、7、8 依赖 1/2/3/4，彼此独立，可任意顺序。
- 9（daemon）依赖 3/4/5/7。
- 10（GUI）依赖 1/2/4/5。
- 11 收尾。

每步：先写失败测试 → 跑到失败 → 最小实现 → 跑到通过 → `go test ./... -count=1 -race` → 提交。

## 不在本次范围

- 同一文件**区域级锁**（v1 用拆文件缓解）。
- 二进制/大文件同步优化（v1 按 UTF-8 文本；图片等留后续）。
- 任务编排 GUI、聊天 GUI、完整活动流/审计。
- 客户端本地 git 历史（服务器已存历史）。
- 把锁原语上游 PR 给 paparship（作为后续独立小 PR）。
- 构建型前端预览（React/Vite）。

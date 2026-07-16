# agentlink 账号登录与多团队认证设计

日期：2026-07-15  
状态：待用户审阅  
目标版本：cowork v2

## 1. 背景与目标

当前 agentlink/cowork 以 `device + API key` 鉴权。Web GUI 要求用户手动粘贴 API token 和 session，CLI `init` 还要求共享的 `REGISTER_PASSWORD`。这种流程对产品经理不直观，也无法隔离多个团队的数据。

本次设计引入个人账号、团队成员关系和多团队切换，并直接终止旧鉴权流程。目标是：

- 用户通过用户名和密码注册、登录。
- 用户可创建或加入多个团队。
- GUI 不再展示或要求粘贴 token/session。
- CLI 只需交互登录一次，凭据自动安全保存。
- 项目、锁、在线状态、消息和任务严格按团队隔离。
- Owner/Admin/Member 权限清晰、可测试。
- 旧数据保留在原 Redis namespace 供回滚，但新版本不读取。

团队 ID 是租户选择器，不是秘密，也不能单独证明身份。请求必须同时具备有效用户会话和对应团队成员资格。

## 2. 已确认的产品决策

- 账号：全局唯一用户名 + 密码，不要求邮箱。
- 多团队：一个账号可加入多个团队。
- 团队创建：任意已登录用户可创建团队，创建者成为 Owner。
- 加入团队：团队 ID + 可轮换邀请码。
- 角色：Owner、Admin、Member。
- CLI 登录：用户名/密码交互登录；凭据自动保存，密码不落盘。
- CLI 当前团队：登录时选择默认团队，之后使用 `agentlink team use` 切换。
- 密码恢复：服务器管理员通过管理 CLI 重置。
- 迁移：clean break；不兼容旧 API token，不迁移测试数据。
- 预览：默认仅团队成员可见，不保留匿名公开预览。

## 3. 总体架构

### 3.1 两类会话

浏览器和 CLI 使用不同会话形态，但最终解析为同一个结构化 Actor。

**浏览器 Web Session**

- 用户名/密码登录后签发随机 256-bit session ID。
- 原文仅进入 HttpOnly Cookie；Redis 只存其 SHA-256 索引。
- Cookie 设置 `HttpOnly; SameSite=Lax; Path=/`，生产环境必须设置 `Secure`。
- WebSocket 同源握手自动携带 Cookie，不再把用户凭据放入 URL。

**CLI Device Session**

- `agentlink login` 交互读取用户名/密码并登记当前设备。
- 服务端签发随机、可撤销的 Device Session。
- CLI 将凭据写入权限 `0600` 的 credentials 文件；用户不查看、不复制。
- REST 和 WebSocket 通过 `Authorization: Device <credential>` 自动发送。
- 密码永不写入本地文件。

### 3.2 统一 Actor

认证中间件向业务 handler 注入：

```text
Actor {
  user_id
  username
  team_id
  role
  device_id
  device_name
  session_name
  client_type  // web | device
}
```

浏览器锁操作自动使用 `device_name=web`、`session_name=gui`。CLI 使用真实设备名和当前本地 session。锁的展示标签为：

```text
username@device:session
```

锁记录同时保存稳定的 `user_id/device_id/session_name`，不能只依赖可变展示文本。

### 3.3 团队作用域

业务 API 使用显式 URL 作用域：

```text
/api/teams/{team_id}/...
```

每个请求依次执行：

1. 解析 Web Session 或 Device Session；
2. 加载有效用户；
3. 验证用户是 URL 中团队的成员；
4. 验证当前操作所需角色；
5. 涉及项目时验证 `project.team_id == team_id`；
6. 注入 Actor 并执行 handler。

即使调用者猜中其他团队的 project ID，也统一返回 404，避免泄漏资源存在性。

## 4. 用户流程

### 4.1 Web GUI

1. 访问 `/`，未登录时显示登录/注册页。
2. 注册需要用户名、密码、确认密码；成功后自动建立 Web Session。
3. 没有团队时展示“创建团队 / 加入团队”引导。
4. 创建团队时输入团队名称；系统返回短团队 ID 和一次性展示的邀请码。
5. 加入团队时输入团队 ID 和邀请码；成功后角色为 Member。
6. 已加入多个团队时，顶部团队切换器选择当前团队。
7. 当前团队 ID 只保存在客户端偏好中；服务端不维护全局“当前团队”，避免多标签页互相覆盖。
8. 团队设置页按角色显示成员管理能力。
9. 会话失效时回到登录页；退出登录立即撤销服务端会话。
10. GUI 不再显示 token 或 session 设置栏。

### 4.2 CLI

```bash
agentlink register --server https://example.com
agentlink login --server https://example.com
agentlink team list
agentlink team create "产品团队"
agentlink team join <team_id> <invite_code>
agentlink team use <team_id>
agentlink team members
agentlink logout
```

- `register` 和 `login` 从 TTY 读取密码，不接受明文密码参数。
- 登录成功后列出团队并选择默认团队。
- `team use` 将当前团队写入本地 config。
- `init/project/lock/sync/message/task` 自动使用当前团队和 Device Session。
- `logout` 只撤销当前设备，不影响账号和其他设备。
- `agentlink init` 仅初始化本地 session/CLAUDE.md/tmux，不再执行服务端注册。

### 4.3 管理员密码重置

服务器管理员使用：

```bash
agentlink-admin user reset-password <username>
```

命令生成一次性临时密码、设置 `must_change_password=true`，并撤销该用户全部 Web/Device Session。用户下次登录后必须先修改密码。

## 5. 团队角色与约束

| 操作 | Owner | Admin | Member |
|---|---:|---:|---:|
| 查看团队与成员 | 是 | 是 | 是 |
| 创建/编辑项目 | 是 | 是 | 是 |
| 锁、apply、sync | 是 | 是 | 是 |
| 轮换邀请码 | 是 | 是 | 否 |
| 移除 Member | 是 | 是 | 否 |
| 任命/撤销 Admin | 是 | 否 | 否 |
| 移除 Admin | 是 | 否 | 否 |
| 转让 Owner | 是 | 否 | 否 |
| 主动退出团队 | 转让后 | 是 | 是 |

约束：

- 每个团队恰好有一个 Owner。
- Owner 不能直接退出、被移除或降级；必须先转让所有权。
- Admin 不能修改 Owner 或其他 Admin 的角色。
- Admin/Member 可主动退出团队；退出后立即失去该团队访问权。
- 用户被移出团队后，其现有会话仍可用于其他团队，但立即失去该团队权限。

## 6. 数据模型

新版本统一使用 `agentlink:v2:*`。

### 6.1 用户与索引

```text
agentlink:v2:user:<user_id>                Hash
  username
  username_normalized
  password_phc
  password_version
  status
  must_change_password
  created_at

agentlink:v2:username:<normalized>         String -> user_id
agentlink:v2:user:<user_id>:teams          Set(team_id)
agentlink:v2:user:<user_id>:web_sessions   Set(session_hash)
agentlink:v2:user:<user_id>:devices        Set(device_id)
```

v1 用户名明确限制为 3–32 位 ASCII 字母、数字、`-`、`_`，并以小写形式建立唯一索引；显示时保留用户输入的原始大小写。该约束避免 Unicode 同形字符产生歧义。

### 6.2 团队与成员

```text
agentlink:v2:team:<team_id>                Hash
  name
  owner_user_id
  invite_hash
  invite_version
  created_at

agentlink:v2:team:<team_id>:members        Hash(user_id -> role)
agentlink:v2:team:<team_id>:projects       Set(project_id)
agentlink:v2:team:<team_id>:devices        Set(device_id)
```

团队 ID 使用人类可读的随机短 ID，例如 `tm_a7k3p9d2`，生成时做碰撞检测。邀请码使用高熵随机值，服务端只保存哈希；轮换后旧邀请码立即失效。

### 6.3 会话

```text
agentlink:v2:web_session:<sha256(id)>      Hash
  user_id
  csrf_hash
  password_version
  created_at
  last_seen_at
  absolute_expires_at

agentlink:v2:device_session:<sha256(id)>   Hash
  user_id
  device_id
  password_version
  created_at
  last_seen_at

agentlink:v2:device:<device_id>            Hash
  user_id
  name
  last_seen_at
  created_at
```

- Web Session：12 小时空闲超时，最长 7 天。
- Device Session：90 天，可在再次登录时轮换。
- Session 中保存 `password_version`；修改/重置密码后旧版本立即失效。

### 6.4 业务数据

project 元数据增加 `team_id`。锁、在线状态、消息和任务的 key 或索引均包含 team ID。project ID 继续全局随机，但任何访问仍必须校验团队归属。

## 7. HTTP 与 WebSocket API

### 7.1 认证

```text
POST /api/auth/register
POST /api/auth/login
POST /api/auth/logout
GET  /api/auth/me
POST /api/auth/change-password
POST /api/auth/device-login
POST /api/auth/device-logout
```

`register/login` 成功后返回用户与团队摘要。Web 客户端同时获得 HttpOnly session Cookie 和一个非 HttpOnly、非鉴权用途的 `al_csrf` Cookie；GUI 读取后放入 `X-CSRF-Token`。`GET /api/auth/me` 会轮换 CSRF 值并重新设置该 Cookie，因此页面刷新后无需保存 token 到 localStorage。Device login 返回一次 Device Session 原文，之后无法再次读取。

管理员重置后，登录响应包含 `must_change_password=true`。该会话只能调用 `me`、`change-password` 和 `logout`；其他受保护接口返回 403 `password change required`。成功改密会清除标记、递增密码版本并撤销全部旧会话，客户端随后重新登录。

### 7.2 团队

```text
GET    /api/teams
POST   /api/teams
POST   /api/teams/join
GET    /api/teams/{team_id}
GET    /api/teams/{team_id}/members
POST   /api/teams/{team_id}/invite/rotate
PATCH  /api/teams/{team_id}/members/{user_id}
DELETE /api/teams/{team_id}/members/{user_id}
POST   /api/teams/{team_id}/transfer-owner
POST   /api/teams/{team_id}/leave
```

### 7.3 团队业务

现有 cowork、设备、消息和任务 API 迁移为：

```text
/api/teams/{team_id}/projects
/api/teams/{team_id}/projects/{project_id}/...
/api/teams/{team_id}/locks/...
/api/teams/{team_id}/agents/...
/api/teams/{team_id}/messages/...
/api/teams/{team_id}/tasks/...
/api/teams/{team_id}/ws?project=<project_id>
/preview/{team_id}/{project_id}/...
```

浏览器 WebSocket 使用 Cookie；CLI WebSocket 使用 Device Authorization Header。用户凭据不再出现在 query string。

## 8. 密码、CSRF 与防滥用

- 密码至少 10 位，不要求人为复杂度组合。
- 使用 Argon2id、独立随机 salt 和 PHC 格式编码。
- 登录失败统一返回“用户名或密码错误”，避免账号枚举。
- 同一规范化用户名或来源 IP 在 15 分钟内最多 5 次失败，超限返回 429。
- 成功登录清除对应失败计数。
- Cookie 写请求必须同时满足：
  - 有效 Web Session；
  - 同源 `Origin`；
  - `X-CSRF-Token` 与 `al_csrf` Cookie 相同，且其 SHA-256 与 Web Session 中的 `csrf_hash` 一致。
- `register/login` 尚无 session，无法校验 CSRF；若请求带 `Origin`，必须与配置的同源地址完全一致。无 `Origin` 的 CLI 注册请求可继续处理。
- CLI Device Session 不受 CSRF 约束。
- 生产模式必须使用 HTTPS；只有 localhost 开发模式允许非 Secure Cookie。
- 日志不得记录密码、session ID、Device Session、CSRF token 或邀请码。

## 9. 预览

预览默认要求团队成员登录：

```text
/preview/{team_id}/{project_id}/
```

服务端验证 Web Session、团队成员关系和 project 归属后才读取文件。iframe 和 live-reload WebSocket 复用浏览器 Cookie。删除现有全局 preview token。

> **过渡实现说明**：在删除 v1 `/preview/{id}` 路由之前，v2 预览临时挂在
> `/preview/teams/{team_id}/{project_id}/`（多出一个字面量 `teams/` 段），以避免
> Go ServeMux 因 v2 模式更具体而覆盖 v1 预览路由。Plan 4 清理 v1 后回落到上述规范路径。

> **已知安全限制（同源存储型 XSS）**：预览与 `/api/...`、GUI 同源提供成员自行写入的
> 任意 HTML，因此某成员植入的 `<script>` 在另一成员打开预览时会在应用 origin 上执行，
> 可读取可读的 `al_csrf` 并以查看者身份发起认证写操作（团队内提权）。这是内部可信成员之间
> 的威胁，与 v1 既有模型一致。已做的最小缓解：预览响应统一带 `X-Content-Type-Options:
> nosniff`。
>
> **决策（2026-07-16）**：当前接受该风险（内部约 10 名可信成员、植入者可追责），继续开发；
> 将「预览独立 origin/子域硬化」列为后续任务（Plan 3/4 阶段落地）。彻底修复方向：预览从独立
> origin/子域提供，使预览页拿不到 app 的 Cookie、也无法同源发认证请求；预览改用只读、可撤销、
> 短时的独立鉴权（独立作用域 Cookie 或预览专用短时 token），并可叠加 `<iframe sandbox>` + CSP。

外部匿名分享不在本期范围；后续可单独增加可撤销、只读、限时的分享链接。

## 10. 错误语义

| 状态码 | 场景 |
|---|---|
| 400 | 请求格式、密码规则、邀请码格式错误 |
| 401 | 未登录、凭据失效、用户名或密码错误 |
| 403 | 已登录但不是成员或角色不足 |
| 404 | 资源不存在，或跨团队访问项目 |
| 409 | 用户名占用、已加入团队、状态冲突 |
| 429 | 登录限流 |

错误响应继续使用稳定的 `{"error":"..."}` 结构；前端不得依赖完整自然语言文案判断逻辑。

## 11. 实施分解

按以下顺序独立交付：

1. 认证核心：用户、密码、Web/Device Session、CSRF、限流。
2. 团队与角色：创建、加入、邀请码、成员管理、中间件。
3. 业务团队化：project/lock/apply/tree/snapshot/WS/agents/messages/tasks。
4. GUI：登录注册、团队引导/切换、成员管理，删除 token/session 设置。
5. CLI：register/login/logout/team 命令、自动凭据、init/sync 迁移。
6. 删除旧链路：REGISTER_PASSWORD、旧注册端点、API key、preview token。
7. E2E、文档、运维命令和旧 namespace 清理说明。

不得长期保留新旧双认证分支。

## 12. 测试与验收

### 12.1 单元测试

- Argon2id hash/verify 与错误密码。
- 用户名规范化、格式和唯一性。
- 邀请码 hash/轮换。
- Owner/Admin/Member 角色矩阵。
- CSRF、session TTL、password_version 失效。

### 12.2 API 集成测试

- 注册后自动登录。
- 创建团队、轮换邀请、第二用户加入。
- 多团队列表与切换。
- 登录错误、限流、登出、过期、改密后全会话失效。
- Device login、调用和撤销。
- Owner 转让及最后 Owner 约束。

### 12.3 隔离测试

- Team A 无法列出、读取或 apply Team B 项目。
- 猜中其他团队 project ID 仍返回 404。
- 同一路径锁在不同团队互不影响。
- 被移除成员立即失去团队访问权。
- WebSocket 只接收当前团队事件。
- 非成员无法打开团队预览。

### 12.4 端到端测试

- GUI：注册 → 创建团队 → 复制邀请 → 第二用户加入 → 协作编辑。
- CLI：登录 → 选择团队 → init → lock → sync。
- 浏览器和 CLI 流程均不要求用户查看、复制或粘贴 token。

## 13. 上线与回滚

- 新版本只读写 `agentlink:v2:*`。
- 上线前备份 Redis。
- 用户重新注册账号、创建团队并重新建立项目。
- 旧二进制仍可读取旧 namespace，出现问题时可回滚。
- 验收完成后由管理员执行显式旧 namespace 清理；服务端不会自动删除。
- 生产未启用 HTTPS 时，认证模式拒绝启动。

## 14. 不在本期范围

- 邮箱、邮件找回、OAuth/SSO。
- JWT access/refresh token。
- 匿名公开预览和外部分享链接。
- 旧 API key 或旧数据自动迁移。
- 计费、组织层级、跨团队项目共享。
- 团队管理员重置全局用户密码。

## 15. 成功标准

Web 新用户从零到协作只需：

1. 注册账号；
2. 输入团队 ID + 邀请码，或创建团队；
3. 进入项目。

CLI 新设备从零到工作只需：

1. `agentlink login`；
2. 选择团队；
3. `agentlink init` 或 `agentlink sync`。

任何正常流程都不要求用户查看、复制或粘贴 token。

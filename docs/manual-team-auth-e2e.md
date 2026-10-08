# 账号与多团队 Web GUI 手动端到端验收清单

本清单验证 Plan 1–3 的用户可见流程：账号登录/注册、团队引导与切换、角色化成员管理，以及团队作用域的项目仪表盘与预览。全部通过浏览器完成，不需要任何 API token。

## 前置准备

1. 启动 Redis（本地默认 `localhost:6379`）。
2. 构建并启动服务器：

   ```bash
   make build-server
   DATA_DIR=./.data ./server
   ```

   - 本地 `http://localhost:8080` 下 Cookie 为非 Secure，可直接用浏览器访问。
   - 若配置了非本地 `PUBLIC_URL`，服务器会强制要求 HTTPS 与 `COOKIE_SECURE=true`，否则启动即 panic。

3. 打开浏览器访问 `http://localhost:8080/`。

## 验收步骤

1. **注册 Alice**：在登录页点击“Create one”，用户名 `alice`、密码 ≥10 位完成注册，自动进入引导页。
2. **创建团队并复制邀请码**：创建团队 `Product`；弹窗一次性展示团队 ID 与邀请码，点击 Copy 复制。关闭弹窗后邀请码不再展示。
3. **新窗口注册 Bob 并加入**：开隐身/新窗口注册 `bob`，在引导页用团队 ID + 邀请码加入团队。
4. **Alice 将 Bob 提升为 Admin**：Alice 打开“Members”面板，把 Bob 由 Member 改为 Admin；Bob 刷新后可见管理动作。
5. **Bob 创建项目并加锁**：Bob 新建项目 `Prototype`，进入项目，右键 `index.html`（或先创建）执行 Acquire lock，并通过本地工具/编辑触发 apply。
6. **Alice 通过 WebSocket 看到更新**：Alice 打开同一项目，文件树锁状态与预览应在秒级内自动刷新（Alerts 面板出现 file/lock 事件）。
7. **跨团队 URL 返回 404**：Alice/Bob 手动访问一个不属于本团队的 `/api/teams/<other>/projects/<id>/tree` 或 `/preview/teams/<other>/<id>/index.html`，应返回 404，而非泄露存在性。
8. **登出回到登录页**：点击 Sign out，应清空界面并回到登录页；再次访问受保护数据接口返回 401。
9. **浏览器存储只含团队 ID**：在开发者工具中检查 `localStorage`，只应存在 `cowork_current_team_id`，绝不出现任何密码、session token、CSRF、邀请码等敏感值。

## 角色矩阵抽查

- **Owner**：可提升/降级 Admin、移除 Admin/Member、转让所有权、轮换邀请码。
- **Admin**：可移除 Member、轮换邀请码；不可修改角色（服务器返回 403）。
- **Member**：成员列表只读。
- **Admin/Member**：可“Leave team”；**Owner** 会被提示“先转让所有权”。

## 安全性抽查

- 所有变更请求（POST/PATCH/DELETE）都带 `X-CSRF-Token`，缺失时返回 403。
- 所有来自服务器的字符串（用户名、项目名、路径、锁归属、消息标题）均通过 `textContent` 渲染，构造含 `<script>` 的用户名/项目名不应产生脚本执行。
- WebSocket 连接 URL 不含 `token=`，仅依赖同源 Cookie。

## 结果记录

| 步骤 | 结果 (PASS/FAIL) | 备注/截图 |
| ---- | ---------------- | --------- |
| 1 注册 Alice |  |  |
| 2 创建团队+邀请码一次性展示 |  |  |
| 3 Bob 加入 |  |  |
| 4 提升 Bob 为 Admin |  |  |
| 5 Bob 建项目+加锁 |  |  |
| 6 Alice WS 秒级更新 |  |  |
| 7 跨团队 404 |  |  |
| 8 登出回登录页 |  |  |
| 9 存储仅 team id |  |  |

> 自动化回归：`go test ./pkg/api/... -run TestE2EV2BrowserJourney -count=1 -race -v` 覆盖注册→建团队→加入→加锁 apply→预览与跨团队 404 的服务端路径。

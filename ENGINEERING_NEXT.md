# 本轮范围（已锁定）

面向：给负责人演示 cowork，路径必须一眼能走完。
基线：`feat/team-auth`（账号 + 团队 + 设备会话）。`main` 几乎是空的，不要从那边开工。

本轮演示路径只有这一条：

**注册 → 创建或加入团队 → 创建项目 → CLI 同步静态原型 → 浏览器预览**，再加上 **两个人锁文件、互不覆盖**。

明确不做：消息和任务的改造、多机运维操作。

## 本地怎么跑

```bash
redis-server --port 6379
make test          # 需要本机 Redis；管理命令测试的时钟必须跟 Redis TIME 对齐
make build-server
PUBLIC_URL=http://localhost:8080 ./server
```

浏览器打开 `http://localhost:8080`（不要打开 `http://127.0.0.1:8080`，登录的 Origin 校验会对不上）。

同步需要先选定团队，再把本地静态目录推进项目：

```bash
agentlink team use <team_id>
agentlink sync <project_id> ./prototype
```

空项目的文件区和预览区会写出这两行命令。

## 这一刀：第一次打开不要迷路

空项目以前没有下一步，预览 iframe 是空白的，看不出必须先用 CLI 同步。新建项目用的是浏览器 `prompt`。邀请只复制邀请码，但加入需要团队 ID 和邀请码两样。界面保持英文。

现在：

- 没有文件、或有文件但没有 `index.html` / `index.htm` 时，预览区不加载 iframe，而是说明要先同步，并给出可复制的 `team use` + `sync` 命令。文件区同样说明同步，并写明右键锁文件，避免两个人互相覆盖。
- 「New project」是表单弹窗。创建成功后直接打开这个空项目，下一步就是同步。
- 邀请弹窗一次复制可粘贴的两行：`Team ID:` 和 `Invite code:`。加入表单写明两样都要。注册和改密码写明至少 10 个字符。

## 风险：预览仍可能和 GUI 同源

预览隔离 **不是本轮要继续做的功能**，但演示时必须当成风险说清楚。

localhost 上，预览已经拆到 `http://127.0.0.1:<端口>`，路径里带只读、限时的 `pg_` 授权，iframe 没有 `allow-same-origin`。这只覆盖「浏览器打开的就是 `localhost`」这一种情况。生产主机不会自动拆：`PUBLIC_URL` 不是 localhost 时，预览仍和 GUI 同源，成员写的 HTML 可以读会话 cookie。`PREVIEW_PUBLIC_URL=off`（或 `same-origin` / `disabled`）会故意退回同源预览，同样有这份风险。

生产上的第二个预览主机名、根路径资源改写、授权失败时的人话提示，都留到演示路径之外。不要为了演示把已经落地的 localhost 拆分撤掉。

## 不在本轮

- 消息、任务面板的改造。
- 多机部署、反向代理、独立预览域名。
- 在 GUI 里塞示例页面，代替 CLI 同步。演示就是同步静态原型。
- 匿名只读分享链接。
- 自助找回密码。密码重置仍只有服务器上的 `agentlink-admin`，演示账号不要走这条。

## 测试时钟

`pkg/auth` 和 `agentlink-admin` 的测试以前把时钟钉在 2026-07-15。Web 会话创建会拿绝对过期时间和 Redis 自己的时间比，超过 7 天就被当成「会话已过期」。这些测试现在跟当前时间对齐。不要再写一个会过期的固定日期。

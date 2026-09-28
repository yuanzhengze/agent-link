# 下一轮工程切片

面向：要把 cowork 演示给不会盯着我们调试的负责人。
基线：`feat/team-auth`（账号 + 团队 + 设备会话）。`main` 几乎是空的，不要从那边开工。

本地怎么跑：

```bash
redis-server --port 6379
make test          # 需要本机 Redis；管理命令测试的时钟必须跟 Redis TIME 对齐
make build-server
PUBLIC_URL=http://localhost:8080 ./server
```

浏览器打开 `http://localhost:8080`（不要打开 `http://127.0.0.1:8080`）。localhost 上预览会自动拆到 `http://127.0.0.1:8080`。

## 已经落地的这一刀

预览不再和 GUI 同源执行成员写的 HTML。localhost 默认开启；生产环境要另设 `PREVIEW_PUBLIC_URL`，或用 `off` 保持旧行为。详见 `docs/team-auth-operations.md` 的 Preview origin。

## 1. 生产环境的预览域名

本地用 `localhost` / `127.0.0.1` 就能演示隔离。生产的 compose 默认仍是同源预览（`PUBLIC_URL=https://cowork.example` 时不会自动拆）。

验收：

- 反向代理把两个主机名转到同一个进程，`PREVIEW_PUBLIC_URL` 是另一个 `https://` 源，`COOKIE_SECURE=true`。
- 用成员账号打开项目后，预览 iframe 的地址在预览源上，路径里带 `pg_` 授权，没有 `al_session`。
- 在原型里放一段脚本：读 `document.cookie`、访问 `parent.document`、向 `PUBLIC_URL` 发带凭证的 `POST /api/teams/...`。三者都失败，预览里的静态页和相对路径的 CSS/JS 仍能打开。
- 把该成员移出团队后，同一条预览 URL 立即 401。应用源上的 `/preview/...` 返回 404。

## 2. 不靠 CLI 的第一条原型

GUI 能建团队、建项目、看文件树，但不能往项目里放文件。空项目的预览是 404，负责人第一次点进去会以为坏了。文件今天只能走 `agentlink sync` 或 API `apply`。

验收：

- 新团队创建项目后，有一个明确的动作（例如「放入示例页面」）生成 `index.html`，预览立刻显示它。
- 不需要安装 CLI、tmux 或 Claude。
- 示例页使用相对路径引用它自己的 CSS，在隔离预览里也能加载。
- 空项目在按下这个动作之前，预览区写明「还没有 index.html」，而不是一片白。

## 3. 原型里的根路径资源

预览挂在 `/preview/{team}/{project}/g/{grant}/` 下。`styles.css` 这种相对地址会带上授权；`/styles.css` 这种站点根路径不会，隔离之后也拿不到应用源的 cookie。很多静态原型用根路径。

验收：

- HTML 里的根相对 `href` / `src`（不以 `//` 开头）在预览里改写到授权前缀下，或用文档说明加 `<base>` 后仍能工作。
- 改写不会把授权写进会随 Referer 离开预览源的外链。
- 现有「相对路径 CSS 原样返回」的测试继续通过。

## 4. 预览失败要能看懂

授权过期、打开了错误的主机、或 `index.html` 不存在时，iframe 里是 JSON 404/401，工具栏没有说明。

验收：

- 预览区用一句人话区分：未登录、不是成员、没有 index.html、预览源连不上。
- 文件变更后 iframe 会换一条新授权再刷新；授权剩不到 2 分钟时也会换。
- 刷新页面不要求用户重新复制任何 token。

## 5. 需要产品拍板，不要我们猜

这些不做进下一刀，除非负责人选定：

- 邀请码只显示一次。丢了只能轮换。要不要同时给「复制团队 ID + 邀请码」和邮件/链接？
- 密码重置只有服务器上的 `agentlink-admin`。GUI 要不要提示「找管理员」，还是继续不提？
- 匿名只读分享链接明确不在本期。演示时如果有人问「把链接发给客户」，答案是还不行。
- 生产预览域名用独立站点（`preview.example`）还是应用的子域。子域在同源策略上已经够用，但是否要独立 cookie 域需要他们认。

## 测试时钟

`pkg/auth` 和 `agentlink-admin` 的测试以前把时钟钉在 2026-07-15。Web 会话创建会拿绝对过期时间和 Redis 自己的时间比，超过 7 天就被当成「会话已过期」。这些测试现在跟当前时间对齐。不要再写一个会过期的固定日期。

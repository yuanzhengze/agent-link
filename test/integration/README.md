# Integration Test Design

## Goal

覆盖 agentlink 所有功能端到端，可在本地（CI）或双机环境运行。

## 测试架构

```
┌─────────────────────────────┐     ┌─────────────────────────────┐
│  Test Runner (host machine) │     │  Server (YOUR_SERVER_IP)     │
│                             │     │                             │
│  agentlink CLI (device-a)   │────▶│  agentlink-server           │
│  curl → API                 │     │  agentlink CLI (device-b)   │
│                             │     │  redis-cli                  │
└─────────────────────────────┘     └─────────────────────────────┘
```

## 测试场景

### 1. API 层测试（curl）

不依赖 CLI 配置，直接测试 v2 服务端 API（账号 + 团队 + 设备会话）。认证使用
`Authorization: Device <credential>`，业务路径均以 `/api/teams/{team_id}/` 为前缀。
`api_test.sh` 覆盖以下冒烟场景：

| 场景 | 方法 | 端点 | 验证 |
|------|------|------|------|
| 健康检查 | GET | /health | 200 + redis connected |
| 注册账号 | POST | /api/auth/register | 2xx |
| 设备登录 | POST | /api/auth/device-login | 2xx + device_credential |
| 未认证请求 | GET | /api/teams | 401 |
| 错误凭据 | GET | /api/teams | 401 |
| 创建团队 | POST | /api/teams | 2xx + team.id + invite_code |
| 创建项目 | POST | /api/teams/{id}/projects | 2xx + id |
| 获取文件锁 | POST | /api/teams/{id}/locks/acquire | 2xx |
| 应用文件 | POST | /api/teams/{id}/projects/{pid}/apply | 2xx + head_commit |
| 读取快照 | GET | /api/teams/{id}/projects/{pid}/snapshot | 2xx + 内容一致 |
| 团队成员 | GET | /api/teams/{id}/members | 2xx + ≥1 成员 |
| 设备登出 | POST | /api/auth/device-logout | 2xx |
| 登出后失效 | GET | /api/teams | 401 |

### 2. CLI 层测试（双机 / 双账号）

依赖两个已 `login` 且加入同一团队的设备，测试跨设备交互（目标在当前团队内解析）：

| 场景 | 命令 | 验证 |
|------|------|------|
| 登录 + 选团队 | `agentlink login` / `agentlink team use` | 保存设备会话 + current_team |
| 心跳 | `agentlink ping` | 状态变 online |
| 团队 agent 列表 | `agentlink list --all` | 显示两个设备 |
| 发消息（A→B） | `agentlink send` | 状态面板显示 |
| 拉消息（B） | `agentlink pull` | 收到消息 + 显示 ID |
| 发任务（A→B） | `agentlink task send` | 返回 task_id |
| 任务状态（A） | `agentlink task status` | issued → in_progress |
| 完成任务（B） | `agentlink task result` | completed |
| 取消任务 | `agentlink task cancel` | cancelled |
| 活跃任务列表 | `agentlink task list` | 只显示活跃的 |
| 目录同步（A→B） | `agentlink sync <project> <dir>` | 秒级同步 + 文件锁串行写入 |

### 3. 边界和错误场景

| 场景 | 预期 |
|------|------|
| 无效 device name / session | 400 |
| 空 content | 400 |
| 超长 content（>3000） | 400 |
| 不存在的 target | 404 |
| 未认证请求 | 401 |
| 非团队成员访问他团队路由 | 403 |
| 跨团队引用他团队项目 | 404 |
| 不存在的 task_id | 404 |
| 已完成 task 再 cancel | 400 |

### 4. 需要双机交互的场景（手动 / 脚本轮询）

| 场景 | 流程 |
|------|------|
| 设备忙碌时 send → 状态面板 | B 先 pull task (变 busy)，A send → 显示 busy |
| device offline 时 send | B 停心跳超过 2 分钟，A send → 显示 offline |
| 中断 --interrupt | A 发 task，B busy 时 A send --interrupt → B 被中断 |
| 多消息未读 | A 连续发 3 条 → B pull → 看到全部 + 未读计数 |

## 输出格式

```
=== Health ===
  PASS  health check returned ok

=== Device login ===
  PASS  device logged in, credential received

=== Team create ===
  PASS  team created (tm_...)

=== Lock + apply ===
  PASS  file lock acquired
  PASS  file applied (committed)

=== Logout revokes device session ===
  PASS  revoked credential returns 401

==========================================
 Results: 15 passed / 0 failed
==========================================
```

## 实现方式

脚本化：一个 bash + curl 脚本做 API 层测试，一个配套 SSH 脚本做双机 CLI 测试。

API 测试可独立运行（不依赖 init），CLI 测试需要先 `deploy.sh` 确保环境就绪。

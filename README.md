# Go AI Studio

基于 Go 的 OpenAI-compatible LLM 推理网关，支持普通推理、SSE 流式响应、多轮会话、用户认证和多实例部署。

## Demo 演示

![Go AI Studio Demo](assets/demo.gif)

## 架构

```text
Client
  │ HTTP / SSE
  ▼
Nginx → Go API × 2
          │
          ├─ stream=false → Worker Pool → RabbitMQ → inference-worker → Dynamo/SGLang
          │
          └─ stream=true  → Dynamo/SGLang → SSE

MySQL：用户、会话、消息和登录 Session
Redis：跨实例限流和聊天历史缓存
```

- 普通请求通过 Worker Pool 和 RabbitMQ 解耦 API 与模型推理。
- 流式请求直连 Dynamo/SGLang，避免逐 Token 经过消息队列。
- MySQL 使用事务、行锁和 `turn_id` 保证同一会话的消息顺序。

## 目录

```text
backend/       Go API、Worker Pool、RabbitMQ、Dynamo 客户端和业务代码
frontend/      React + TypeScript 前端
middleware/    Docker Compose、Nginx、MySQL、RabbitMQ、Redis、Dynamo 配置
scripts/       本地一键启动脚本
```

## 快速启动

依赖：Go、Docker、Node.js、npm、uv；使用真实模型还需要 NVIDIA GPU 环境。

### 本地开发

```bash
cp .env.example .env
make dev-up
```

脚本会启动 MySQL、RabbitMQ、Redis、Dynamo、Go API、inference-worker 和 React Vite。
日志位于 `logs/`，按 `Ctrl+C` 停止服务。

访问地址：

```text
前端：http://127.0.0.1:5173
Go API：http://127.0.0.1:18080
Dynamo：http://127.0.0.1:8000
```

前后端分开启动：

```bash
make dev-backend
make dev-frontend   # 另一个终端执行
```

### Docker 部署

先启动宿主机上的 Dynamo frontend 和 SGLang worker：

```bash
cd middleware/dynamo
uv sync

# 终端 1
uv run python -m dynamo.frontend --http-port 8000 --discovery-backend file

# 终端 2
CUDA_VISIBLE_DEVICES=0 uv run python -m dynamo.sglang \
  --model-path Qwen/Qwen3-0.6B \
  --discovery-backend file
```

回到项目根目录：

```bash
cp .env.docker.example .env
make docker-up
curl http://127.0.0.1:8088/api/readyz
```

Docker API 地址：`http://127.0.0.1:8088`。

停止服务：

```bash
make docker-down
```

没有 GPU 时，可以将 `.env` 中的 `BACKEND` 改为 `mock`，用于验证网关、队列和数据库流程。

## API

### OpenAI-compatible 推理接口

```bash
curl http://127.0.0.1:8088/api/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -H 'X-Client-ID: local-browser-1' \
  -d '{"model":"Qwen/Qwen3-0.6B","messages":[{"role":"user","content":"你好"}],"stream":false}'
```

流式请求将 `stream` 改为 `true`，并使用 `curl -N`。

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/api/v1/chat/completions` | 普通或流式推理 |
| GET | `/api/healthz` | 健康检查 |
| GET | `/api/livez` | 存活检查 |
| GET | `/api/readyz` | 依赖就绪检查 |
| GET | `/api/stats` | Worker Pool 统计 |
| GET | `/api/metrics` | Prometheus 指标 |
| GET | `/api/conversations` | 获取会话列表 |
| DELETE | `/api/conversations/:id` | 删除会话 |

### 用户认证

配置 `AUTH_JWT_SECRET` 后启用用户系统：

```env
AUTH_JWT_SECRET=至少32个字符的随机字符串
```

接口：

```text
POST /api/auth/register   注册
POST /api/auth/login      登录
POST /api/auth/refresh    刷新 Token
POST /api/auth/logout     注销 Session
GET  /api/auth/me         获取当前用户
```

业务请求携带：

```text
Authorization: Bearer <access_token>
```

Access Token 默认有效期 15 分钟，Refresh Token 默认有效期 30 天。未启用用户认证时，
本地开发可以使用 `X-Client-ID` 隔离匿名会话。

## 核心配置

```env
BACKEND=dynamo
DYNAMO_URL=http://127.0.0.1:8000
QUEUE_URL=amqp://guest:guest@127.0.0.1:5672/
MYSQL_DSN=root:root@tcp(127.0.0.1:3306)/go_ai_studio?parseTime=true
REDIS_URL=redis://127.0.0.1:6379/0
MAX_IN_FLIGHT=8
GLOBAL_MAX_IN_FLIGHT=16
```

Docker 容器内部使用服务名通信：`mysql:3306`、`rabbitmq:5672`、`redis:6379`。

## 测试与构建

```bash
make test
make race
make vet
make frontend
make build
make integration
```

日志目录：

```text
本地：logs/
Docker：logs/api-1/、logs/api-2/、logs/worker-1/
```

当前项目是学习型生产工程 MVP，未包含 TLS、RabbitMQ TTL/DLQ 和正式压测报告，不建议直接暴露到公网。

# Go AI Studio

基于 Go/Gin、RabbitMQ、NVIDIA Dynamo/SGLang 和 React 的分布式 LLM 推理网关。
项目重点：请求路由、Worker Pool、消息队列、SSE、多实例部署和会话持久化。

## 架构

```text
React / OpenAI SDK
        │ HTTP / SSE
        ▼
Nginx:8088 ──▶ Go API api-1/api-2
                    │
          ┌─────────┴─────────┐
          │                   │
      stream=false        stream=true
          │                   │
      Worker Pool        Dynamo SSE
          │                   │
      RabbitMQ             │
          │                   │
   inference-worker ───────┘
          │
   Dynamo / SGLang ──▶ GPU

MySQL：用户、登录会话、聊天会话和消息持久化
Redis：多 API 实例全局限流和聊天历史短缓存
etcd + NATS：Dynamo 服务发现和事件通信
```

- 非流式请求：Go API → Worker Pool → RabbitMQ → inference-worker → Dynamo。
- 流式请求：Go API 直接连接 Dynamo，返回 OpenAI-compatible SSE。
- MySQL 保存会话历史，`X-Client-ID` 用于匿名会话隔离。

## 目录

```text
backend/                 Go API、Worker 和业务代码
backend/Dockerfile       生产镜像构建文件
frontend/                React + TypeScript + Vite
middleware/              Compose、Nginx、MySQL、RabbitMQ、Dynamo 配置
Makefile                 测试、构建和部署命令
```

## Docker 启动

### 1. 配置

```bash
cp .env.docker.example .env
```

没有 Dynamo 时，将 `.env` 中的 `BACKEND` 改成 `mock`，可先验证完整网关链路。

### 2. 启动真实模型

Dynamo/SGLang 使用 GPU，当前作为宿主机服务运行：

```bash
cd middleware/dynamo
uv sync
```

单机开发不需要 Dynamo 源码、etcd 或 NATS。使用 `file` 发现模式启动 frontend 和
SGLang worker，两个进程必须使用相同的发现模式：

```bash
# 终端 1
uv run python -m dynamo.frontend \
  --http-port 8000 \
  --discovery-backend file
```

# 终端 2
```bash
CUDA_VISIBLE_DEVICES=0 uv run python -m dynamo.sglang \
  --model-path Qwen/Qwen3-0.6B \
  --discovery-backend file
```

如果要学习 Dynamo 多节点部署，再额外使用 etcd + NATS，并切换为
`--discovery-backend etcd`。这时需要单独准备基础设施或获取 Dynamo 源码仓库。

### 3. 启动项目

在仓库根目录执行：

```bash
make docker-up
curl http://127.0.0.1:8088/api/readyz
```

Compose 会启动：Nginx、两个 Go API、inference-worker、RabbitMQ、Redis 和 MySQL。

```bash
make docker-logs    # 查看日志
make docker-down    # 停止服务，保留数据卷
```

Docker 中 Dynamo 地址由 `DOCKER_DYNAMO_URL` 配置，默认是：

```env
DOCKER_DYNAMO_URL=http://host.docker.internal:8000
```

容器之间使用服务名通信，例如 `mysql:3306`、`rabbitmq:5672`、`redis:6379`，不要使用容器内的
`127.0.0.1`。

## 本地开发

### 一键启动完整本地链路

确保已安装 Docker、Go、Node.js、uv，并准备好 GPU 驱动后，在项目根目录执行：

```bash
cp .env.example .env
make dev-up
```

脚本会启动 MySQL、RabbitMQ、Redis、Dynamo frontend、SGLang worker、Go API、
inference-worker 和 React Vite。按 `Ctrl+C` 会停止本地进程及 MySQL/RabbitMQ/Redis
容器，但保留 Docker 数据卷。日志位于 `logs/`。

Redis 默认用于跨 API 实例的全局推理并发限制和聊天历史短缓存；Redis 暂时不可用时，
聊天历史回退到 MySQL，本地 `MAX_IN_FLIGHT` 限流仍然生效。

也可以前后端分离启动：

```bash
# 终端 1：MySQL、RabbitMQ、Dynamo、Go API、inference-worker
make dev-backend

# 终端 2：React Vite 热重载
make dev-frontend
```

如果只想运行 Mock 后端，将 `.env` 中的 `BACKEND` 改成 `mock`，并清空
`QUEUE_URL`、`MYSQL_DSN`；脚本会跳过 inference-worker，但仍会启动 Dynamo 进程。

### 分终端启动

```bash
cp .env.example .env
docker compose -f middleware/docker-compose.yml up -d mysql rabbitmq

# 终端 1
go run ./backend/cmd/server

# 终端 2
WORKER_ID=worker-1 go run ./backend/cmd/inference-worker

# 终端 3
cd frontend
npm ci
npm run dev
```

- 前端：<http://127.0.0.1:5173>
- 本地 API：<http://127.0.0.1:18080>
- Docker API：<http://127.0.0.1:8088>

可以启动多个 Worker；它们共同消费 RabbitMQ 队列，不需要监听不同端口。

## API

Docker 模式示例：

```bash
curl http://127.0.0.1:8088/api/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -H 'X-Client-ID: local-browser-1' \
  -d '{"model":"Qwen/Qwen3-0.6B","messages":[{"role":"user","content":"你好"}],"stream":false}'
```

流式请求将 `stream` 改为 `true`，并使用 `curl -N`。

| 方法 | 路径 | 作用 |
|---|---|---|
| GET | `/api/healthz` | 基础健康检查 |
| GET | `/api/livez` | 存活检查 |
| GET | `/api/readyz` | Dynamo、RabbitMQ、MySQL 就绪检查 |
| GET | `/api/stats` | Worker Pool 统计 |
| GET | `/api/metrics` | Prometheus 指标 |
| GET | `/api/conversations` | 会话列表 |
| DELETE | `/api/conversations/:id` | 删除会话 |
| POST | `/api/v1/chat/completions` | OpenAI-compatible 推理接口 |

配置 `AUTH_JWT_SECRET`（至少 32 个字符）后启用用户系统：

```bash
AUTH_JWT_SECRET=$(openssl rand -base64 32)
```

```text
POST /api/auth/register   注册并返回 Access/Refresh Token
POST /api/auth/login      登录
POST /api/auth/refresh    轮换 Refresh Token
POST /api/auth/logout     注销登录会话
GET  /api/auth/me         获取当前用户
```

业务请求使用：

```text
Authorization: Bearer <access_token>
```

Access Token 默认 15 分钟，Refresh Token 默认 30 天。每个用户可以拥有多个
登录会话；聊天会话通过 `conversation_id` 归属于登录用户。未配置
`AUTH_JWT_SECRET` 时，本地兼容使用 `X-Client-ID`，不适合生产环境。

快速验证用户系统：

```bash
curl -s http://127.0.0.1:8088/api/auth/register \
  -H 'Content-Type: application/json' \
  -d '{"username":"alice","email":"alice@example.com","password":"password123"}'

curl -s http://127.0.0.1:8088/api/auth/me \
  -H 'Authorization: Bearer <access_token>'
```

启用 MySQL 用户认证后，同一个 `conversation_id` 同时只允许一个推理任务，
服务端使用 MySQL 行锁分配 `turn_id`；不同会话仍可并行执行。消息按
`turn_id` 和 `message_order` 读取，不依赖 HTTP 连接或 RabbitMQ 的投递顺序。

配置 `API_KEY` 后，也可以使用以下请求头进行服务级鉴权：

```text
Authorization: Bearer <API_KEY>
```

## 测试与构建

```bash
make test
make race
make vet
make frontend
make build
make integration
docker compose -f middleware/docker-compose.yml config --quiet
```

日志位置：

```text
本地：logs/api.log、logs/inference-worker.log
Docker：logs/api-1/、logs/api-2/、logs/worker-1/
```

## 当前边界

这是一个学习型生产工程 MVP，已经具备 Docker Compose、RabbitMQ RPC、Worker Pool、
 Dynamo/SGLang、SSE、MySQL 用户/登录会话/聊天会话和基础可观测性。尚未包含 TLS、
RabbitMQ TTL/DLQ 和正式压测报告，不建议直接暴露到公网。

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

MySQL：会话和消息持久化
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

然后启动 Dynamo 依赖的 etcd、NATS、frontend 和 SGLang worker。示例：

```bash
cd /path/to/dynamo
docker compose -f dev/docker-compose.yml up -d

cd /path/to/go-ai-studio-fullstack/middleware/dynamo
uv run python -m dynamo.frontend \
  --http-port 8000 \
  --discovery-backend etcd \
  --router-min-initial-workers 1

CUDA_VISIBLE_DEVICES=0 uv run python -m dynamo.sglang \
  --model-path Qwen/Qwen3-0.6B
```

### 3. 启动项目

在仓库根目录执行：

```bash
make docker-up
curl http://127.0.0.1:8088/api/readyz
```

Compose 会启动：Nginx、两个 Go API、inference-worker、RabbitMQ 和 MySQL。

```bash
make docker-logs    # 查看日志
make docker-down    # 停止服务，保留数据卷
```

Docker 中 Dynamo 地址由 `DOCKER_DYNAMO_URL` 配置，默认是：

```env
DOCKER_DYNAMO_URL=http://host.docker.internal:8000
```

容器之间使用服务名通信，例如 `mysql:3306`、`rabbitmq:5672`，不要使用容器内的
`127.0.0.1`。

## 本地开发

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

配置 `API_KEY` 后，使用以下请求头鉴权：

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
Dynamo/SGLang、SSE、MySQL 会话和基础可观测性。尚未包含完整用户系统、JWT、TLS、
RabbitMQ TTL/DLQ 和正式压测报告，不建议直接暴露到公网。

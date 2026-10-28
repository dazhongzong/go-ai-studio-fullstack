# Go AI Studio

基于 Go/Gin、RabbitMQ、NVIDIA Dynamo/SGLang 和 React 的分布式 LLM 推理网关，适合作为实习项目展示。

## 架构

```text
React/Vite ──HTTP/SSE──▶ Nginx(可选) ──▶ Go API Gateway / Gin
                                             │
                         ┌───────────────────┴──────────────────┐
                         │                                      │
                   stream=false                           stream=true
                         │                                      │
                    Worker Pool ──▶ RabbitMQ              Dynamo SSE
                         │                                      │
                    inference-worker ──▶ Dynamo/SGLang ──▶ GPU

                         MySQL：会话和消息持久化
                         etcd + NATS：Dynamo 服务发现和事件通信
```

### 请求链路

```mermaid
flowchart LR
    A[React / OpenAI SDK] --> B[Nginx 可选]
    B --> C[Go API Gateway]
    C --> D{stream?}
    D -- false --> E[Worker Pool]
    E --> F[RabbitMQ RPC]
    F --> G[inference-worker]
    G --> H[Dynamo / SGLang]
    H --> G --> F --> E --> C
    D -- true --> I[Dynamo SSE]
    I --> C --> A
    C --> J[(MySQL)]
```

- 非流式请求经过 Worker Pool 和 RabbitMQ，适合排队、限流和多 Worker 扩展。
- 流式请求使用 OpenAI-compatible SSE，当前直接连接 Dynamo，不经过 RabbitMQ Worker Pool。
- MySQL 保存会话和消息；前端通过 `X-Client-ID` 做匿名会话隔离。

## 配置

不要提交本地 `.env`。首次运行：

```bash
cp .env.example .env
```

```env
BACKEND=dynamo
PORT=18080
DYNAMO_URL=http://127.0.0.1:8000
DYNAMO_MODEL=Qwen/Qwen3-0.6B
API_KEY=
QUEUE_URL=amqp://guest:guest@127.0.0.1:5672/
QUEUE_RETRY_INTERVAL=2s
MYSQL_DSN=root:root@tcp(127.0.0.1:3306)/go_ai_studio?parseTime=true
MAX_IN_FLIGHT=8
POOL_WORKERS=3
POOL_QUEUE_SIZE=5
TASK_TIMEOUT=90s
ASK_TIMEOUT=100s
STREAM_TIMEOUT=180s
```

`root:root` 只适用于本地 Compose 学习环境，非本地环境必须更换账号密码。Go API 和 inference-worker 会自动读取项目根目录 `.env`。

## 启动流程

### 1. 启动基础设施

```bash
docker compose up -d mysql rabbitmq
docker compose ps
```

### 2. 启动 Dynamo

在 Dynamo 源码目录启动 etcd 和 NATS：

```bash
docker compose -f dev/docker-compose.yml up -d
```

启动 Dynamo frontend：

```bash
python -m dynamo.frontend --http-port 8000 --discovery-backend etcd --router-min-initial-workers 1
```

启动 SGLang Worker：

```bash
CUDA_VISIBLE_DEVICES=0 python -m dynamo.sglang --model-path Qwen/Qwen3-0.6B
```

检查 Dynamo：

```bash
curl -i http://127.0.0.1:8000/live
curl -i http://127.0.0.1:8000/health
curl http://127.0.0.1:8000/v1/models
```

### 3. 启动 Go API、Worker 和前端

```bash
go run ./cmd/server
WORKER_ID=worker-1 go run ./cmd/inference-worker
```

可以启动多个 Worker；它们共同消费 `inference.jobs`，不需要监听不同端口。

```bash
cd frontend
npm ci
npm run dev
```

前端地址：http://127.0.0.1:5173；API 地址：http://127.0.0.1:18080。生产构建后，Go 从 `frontend/dist` 提供页面；Nginx 配置位于 `deploy/nginx.conf`。

## API

```bash
curl http://127.0.0.1:18080/api/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer YOUR_API_KEY' \
  -H 'X-Client-ID: local-browser-1' \
  -d '{"model":"Qwen/Qwen3-0.6B","messages":[{"role":"user","content":"你好"}],"stream":false}'
```

流式请求将 `stream` 改为 `true`，并使用 `curl -N`。

| 方法 | 路径 | 作用 |
|---|---|---|
| GET | `/api/healthz` | 基础健康检查 |
| GET | `/api/livez` | 进程存活检查 |
| GET | `/api/readyz` | Dynamo、RabbitMQ、MySQL 就绪检查 |
| GET | `/api/stats` | Worker Pool 统计 |
| GET | `/api/metrics` | Prometheus 指标 |
| GET | `/api/conversations` | 当前客户端会话列表 |
| DELETE | `/api/conversations/:id` | 删除当前客户端会话 |
| POST | `/api/ask` | Worker Pool 学习接口 |
| POST | `/api/v1/chat/completions` | OpenAI-compatible 推理接口 |

## 故障恢复

- API 启动时连接 RabbitMQ 失败，会按照 `QUEUE_RETRY_INTERVAL` 重试。
- Worker 断线后按照 1、2、4、8 秒退避重连，最大间隔 30 秒。
- Worker 使用手动 ACK；Worker 崩溃时，未 ACK 的消息会被 RabbitMQ 重新投递。
- API 和 Worker 的消息发布启用了 Publisher Confirm，避免仅写入客户端 socket 就误判成功。
- `/readyz` 反映 Dynamo、RabbitMQ、MySQL 状态，便于摘除未就绪实例。
- 当前未实现消息 TTL、死信队列和业务失败重试，生产环境需要补充这些策略。

## 测试、构建和日志

```bash
make test
make race
make vet
make build
make integration   # 需要 Docker 和 RabbitMQ
```

日志输出到 `logs/api.log` 和 `logs/inference-worker.log`。

## 项目边界

这是一个具备生产工程实践的学习型 MVP，重点展示分布式推理网关、Worker Pool、RabbitMQ 调度、Dynamo/SGLang 推理和 SSE。当前没有完整用户登录、JWT、TLS、RabbitMQ DLQ/TTL、Go 服务 Docker 化和压测报告，不建议直接暴露到公网。

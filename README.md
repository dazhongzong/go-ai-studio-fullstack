# Go AI Studio

基于 Go、React、NVIDIA Dynamo 和 SGLang 的本地 LLM 推理网关学习项目。

## 架构

~~~text
React + Vite
     │
     ▼
Go API Gateway（Gin）
     ├── stream=false → Worker Pool → RabbitMQ（可选）→ inference-worker
     │                                                        │
     │                                                        ▼
     │                                                     Dynamo
     │
     └── stream=true  ─────────────────────────────────────► Dynamo SSE
                                                              │
                                                              ▼
                                                           SGLang/GPU
~~~

- etcd + NATS：Dynamo 的服务发现和事件通信。
- RabbitMQ：项目中的非流式任务队列，可选。
- MySQL：会话和消息持久化，可选。
- 流式请求使用 OpenAI-compatible SSE，不经过 RabbitMQ Worker Pool。

## 目录

~~~text
cmd/server/              Go API 网关
cmd/inference-worker/    RabbitMQ 推理消费者
internal/api/            Gin 路由和 OpenAI API
internal/dynamo/         Dynamo HTTP/SSE 客户端
internal/llm/            推理协议和错误类型
internal/queue/          RabbitMQ RPC
internal/store/          MySQL 存储
workerpool/              Worker Pool
frontend/                React + TypeScript + Vite
deploy/nginx.conf        Nginx 负载均衡配置
deploy/nginx.dev.conf    Nginx 开发环境配置
docker-compose.yml       MySQL、RabbitMQ
pyproject.toml           uv/Dynamo 依赖
~~~

## 配置

~~~bash
cp .env.example .env
~~~

常用配置：

~~~env
BACKEND=dynamo
PORT=18080
DYNAMO_URL=http://127.0.0.1:8000
DYNAMO_MODEL=Qwen/Qwen3-0.6B
API_KEY=
MAX_IN_FLIGHT=8
POOL_WORKERS=3
POOL_QUEUE_SIZE=5

# 可选：RabbitMQ 非流式任务队列
QUEUE_URL=amqp://guest:guest@127.0.0.1:5672/

# 可选：MySQL 多轮会话存储
MYSQL_DSN=root:root@tcp(127.0.0.1:3306)/go_ai_studio?parseTime=true
~~~

Go 服务和 inference-worker 会自动读取项目根目录的 .env。

## 启动 Dynamo

使用 Python/uv 安装 Dynamo 后，etcd 和 NATS 仍需要单独启动。进入 Dynamo 源码目录执行：

~~~bash
docker compose -f dev/docker-compose.yml up -d
~~~

然后使用相同配置启动 Dynamo 前端和 Worker：

~~~bash
export DYN_DISCOVERY_BACKEND=etcd
export DYN_EVENT_PLANE=nats
export DYN_REQUEST_PLANE=tcp
export ETCD_ENDPOINTS=http://127.0.0.1:2379
export NATS_SERVER=nats://127.0.0.1:4222
export DYN_NAMESPACE=go-ai-studio
~~~

启动 Dynamo frontend：

~~~bash
python -m dynamo.frontend \
  --http-port 8000 \
  --discovery-backend etcd \
  --router-min-initial-workers 1
~~~

启动 SGLang Worker：

~~~bash
CUDA_VISIBLE_DEVICES=0 python -m dynamo.sglang \
  --model-path Qwen/Qwen3-0.6B
~~~

确认 Dynamo 正常：

~~~bash
curl -i http://127.0.0.1:8000/live
curl -i http://127.0.0.1:8000/health
curl http://127.0.0.1:8000/v1/models
~~~

## 启动项目

如果启用 MySQL 和 RabbitMQ：

~~~bash
docker compose up -d mysql rabbitmq
~~~

启动 Go API：

~~~bash
go run ./cmd/server
~~~

默认地址：http://127.0.0.1:18080

如果配置了 API_KEY，前端开发环境还需要在 `frontend/.env.local` 中配置相同的 `VITE_API_KEY`。

启动 React 开发服务：

~~~bash
cd frontend
npm install
npm run dev
~~~

前端地址：http://127.0.0.1:5173

生产构建：

~~~bash
cd frontend
npm run build
cd ..
go run ./cmd/server
~~~

构建后的页面由 Go 服务从 frontend/dist 提供。

生产环境使用 `deploy/nginx.conf`；本地 Vite 开发环境使用 `deploy/nginx.dev.conf`。

## RabbitMQ Worker 模式

启动一个或多个推理消费者：

~~~bash
WORKER_ID=worker-1 go run ./cmd/inference-worker
~~~

多个 Worker 使用不同的 WORKER_ID，不需要监听不同端口。它们共同消费 RabbitMQ 的 inference.jobs 队列。

## API

OpenAI-compatible 非流式请求：

~~~bash
curl http://127.0.0.1:18080/api/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{
    "model":"Qwen/Qwen3-0.6B",
    "messages":[{"role":"user","content":"你好"}],
    "stream":false
  }'
~~~

OpenAI-compatible 流式请求：

~~~bash
curl -N http://127.0.0.1:18080/api/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{
    "model":"Qwen/Qwen3-0.6B",
    "messages":[{"role":"user","content":"介绍一下 Go"}],
    "stream":true
  }'
~~~

其他接口：

~~~text
GET  /api/healthz   健康检查
GET  /api/livez     进程存活检查
GET  /api/readyz    依赖就绪检查
GET  /api/stats     Worker Pool 统计
POST /api/ask       Worker Pool 学习接口
~~~

## 测试和日志

~~~bash
go test ./...
go vet ./...

cd frontend
npm run build
~~~

也可以使用：

~~~bash
make test
make race
make vet
make build
~~~

日志输出到终端和：

~~~text
logs/api.log
logs/inference-worker.log
~~~

当前项目是本地学习用 MVP，尚未包含用户登录、权限控制、限流认证和生产级密钥管理，不建议直接暴露到公网。

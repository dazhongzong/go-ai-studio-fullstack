# Go AI Studio — Go + Vue 3 全栈 AI 推理工作台

在上一课的 Go Worker Pool + Dynamo + SSE 工程上增量开发，适合熟悉 C++、希望在 1–2 周内掌握 Go 全栈项目的同学。

## 技术栈与目前功能

- **后端（Go）：** Gin、net/http、Middleware、Context、Worker Pool、限流、优雅关闭、Dynamo OpenAI-compatible HTTP 客户端、RabbitMQ 请求/回复队列。
- **前端（Vue 3 + TypeScript + Vite）：** 聊天窗口、对话列表、POST + SSE 流式输出、停止生成、后端健康状态、Worker Pool 监控、响应式布局。
- **数据保存：** 默认使用浏览器 `localStorage`；配置 `MYSQL_DSN` 后，后端会把会话和消息保存到 MySQL，并按 `conversation_id` 从数据库恢复上下文；当前仍未实现用户登录。
- **任务调度：** `/ask` 经过本地 Worker Pool，可进一步通过 RabbitMQ 转发到独立推理消费者；`/ask/stream` 走 Dynamo SSE 路径，共用入口并发限制。
- **分布式学习模式：** 配置 `QUEUE_URL` 后，多个 Go API 实例通过 RabbitMQ 请求/回复队列调用独立的 `inference-worker`；Nginx 负责 API 和 SSE 连接的负载均衡。

## 目录

```text
go-ai-studio-fullstack/
├── cmd/
│   ├── server/main.go             # Go API、/api/info、可选生产静态页面
│   ├── server/main_test.go        # API + 静态文件联调测试
│   ├── inference-worker/main.go  # RabbitMQ 推理消费者，调用 Dynamo
│   ├── mockollama/main.go        # 旧版 Ollama 模拟服务，保留作协议测试
│   └── demo/main.go
├── internal/
│   ├── api/server.go             # /ask、/ask/stream、/healthz、/stats
│   ├── dynamo/client.go          # Dynamo OpenAI-compatible HTTP/SSE 客户端
│   ├── ollama/client.go          # 旧版 Ollama 客户端和测试协议
│   ├── queue/rpc.go              # RabbitMQ 请求/回复
│   └── store/mysql.go            # MySQL 会话和消息存储
├── workerpool/pool.go           # Worker Pool
├── pyproject.toml               # Python/uv Dynamo 依赖声明
├── uv.lock                      # Python/uv 依赖锁文件
├── .env.example                 # 本地环境变量模板
├── frontend/
│   ├── index.html
│   ├── vite.config.ts           # /api 本地开发代理到 Go 18080
│   ├── package.json
│   └── src/
│       ├── App.vue              # 页面和聊天交互
│       ├── api/client.ts        # POST JSON / fetch SSE 解析
│       ├── styles.css
│       └── main.ts
└── go.mod
```

## 运行路线 A：仅模拟，不需要 GPU

所需环境：Go 1.27+，Node.js 20.19+ 或 22.12+（建议使用当前 Node LTS），npm。

使用 Mock 后端运行 Go 网关。Windows PowerShell：

```powershell
$env:BACKEND = 'mock'
$env:PORT = '18080'
go run ./cmd/server
```

Linux/macOS Bash：

```bash
BACKEND=mock PORT=18080 go run ./cmd/server
```

**终端 3** 安装前端依赖并启动 Vite 开发服务（安装依赖第一次需要联网）：

```bash
cd frontend
npm install
npm run dev
```

浏览器访问 **http://127.0.0.1:5173**。Mock 后端支持非流式请求；要测试真实 SSE，需要启动 Dynamo。

> 如果将 `BACKEND=mock` 作为直接后端，非流式 `/ask` 仍能使用，但 `/ask/stream` 不可用，前端会自动读取 `/api/info` 并关闭流式功能。

## 运行路线 B：Dynamo + GPU

Dynamo 官方支持 vLLM、SGLang 和 TensorRT-LLM，并通过 OpenAI-compatible API 提供 `/v1/completions` 和 `/v1/chat/completions`。请先按照 NVIDIA 官方 Quickstart 启动 Dynamo Frontend 和 Worker，再启动本项目网关。

```bash
set -a
source .env
set +a
export PORT=18080
go run ./cmd/server
```

Go API 和 `inference-worker` 启动时会自动读取项目根目录的 `.env`；已经存在的 shell、Docker 或 Kubernetes 环境变量优先级更高。复制 `.env.example` 后按本机服务修改，不要提交真实 `.env`。

Python/uv 依赖声明在 `pyproject.toml`，完整解析结果在 `uv.lock`。使用已有的 Dynamo 虚拟环境同步：

```bash
source .dynamo-venv/bin/activate
uv sync --active
```

也可以让 uv 使用项目默认环境：

```bash
uv sync
```

Dynamo 的本地部署需要兼容的 GPU、驱动和后端运行时；具体版本以 NVIDIA 官方文档为准。

### 运行时配置与请求指标

Go 网关支持通过环境变量配置模型和超时。时间配置使用 Go duration 格式，例如 `30s`、`2m`。

| 环境变量 | 默认值 | 作用 |
|---|---:|---|
| `DYNAMO_URL` | `http://127.0.0.1:8000` | Dynamo Frontend 地址 |
| `DYNAMO_MODEL` | `Qwen/Qwen2.5-0.5B-Instruct` | Dynamo 服务的模型名称 |
| `ASK_TIMEOUT` | mock `1.5s` / Dynamo `100s` | 非流式请求总超时 |
| `STREAM_TIMEOUT` | `2m` / Dynamo `180s` | 流式请求总超时 |
| `TASK_TIMEOUT` | mock `700ms` / Dynamo `90s` | Worker Pool 或推理消费者单任务超时 |
| `MAX_PROMPT_LENGTH` | `8192` | prompt 最大字符数 |
| `MAX_CONTEXT_MESSAGES` | `24` | 单次请求最多携带的历史消息数 |
| `MAX_CONTEXT_LENGTH` | `32768` | 单次请求上下文内容最大字符数 |
| `MYSQL_DSN` | 空 | 例如 `root:root@tcp(127.0.0.1:3306)/go_ai_studio?parseTime=true` |
| `QUEUE_URL` | 空 | 例如 `amqp://guest:guest@127.0.0.1:5672/`；为空时使用本地 Worker Pool |

非流式 `/ask` 响应会返回 `request_id`、`task_id`、`worker_id`、`queue_ms`、`duration_ms` 和 `model`，同时响应头包含 `X-Request-ID`。流式 `/ask/stream` 会在 SSE 的 `token`、`done`、`error` 事件中携带请求 ID 和耗时；流式路径当前不经过 Worker Pool，因此 `queue_ms` 为 `0`。

`/stats` 现在还会返回 `active`、`submitted`、`canceled`、`rejected`、`max_queued`、`queue_capacity` 和累计排队时间等 Worker Pool 指标。

## Go 单服务托管前端（构建后部署）

先在 `frontend` 目录构建静态页面：

```bash
npm run build
```

然后**回到项目根目录**运行 Go：

```powershell
$env:BACKEND = 'dynamo'
$env:DYNAMO_URL = 'http://127.0.0.1:8000'
$env:DYNAMO_MODEL = 'Qwen/Qwen2.5-0.5B-Instruct'
$env:PORT = '18080'
go run ./cmd/server
```

访问 **http://localhost:18080**，Go 将自动提供 `frontend/dist` 中的静态页面。需要指定其他静态文件目录时设置 `WEB_DIST`。

开发模式：浏览器 → Vite :5173 → `/api` 代理 → Go :18080（改写为 `/ask` 等原路径）。

生产模式：浏览器 → Go :18080，前端直接访问 `/api/ask`、`/api/ask/stream`，Go 在同一源下同时提供 API 和前端资源。

## 调用链（建议阅读顺序）

### 非流式

```text
frontend/src/App.vue submit()
  → frontend/src/api/client.ts ask()
  → POST /api/ask （Vite 开发时代理改写为 /ask）
  → internal/api/server.go /ask Handler
  → workerpool/pool.go Submit() → jobs chan
  → runWorker() → p.handler()
  → RabbitMQ（启用 QUEUE_URL 时）
  → inference-worker
  → internal/dynamo/client.go Generate()
  → /v1/chat/completions → Dynamo Frontend
  → Result reply chan → Go JSON Response
  → App.vue 更新消息状态
```

### 流式

```text
App.vue submit()
  → api/client.ts askStream()
  → POST /api/ask/stream
  → server.go /ask/stream Handler
  → dynamo.Client.Stream()
  → http.Client.Do() → Dynamo SSE
  → SSE frame decode
  → onChunk 回调 → SSE 格式写入 → Flush()
  → fetch().body.getReader()
  → 解析 event: token / done / error
  → Vue 响应式更新页面
```

> 浏览器端采用 **`fetch` + `ReadableStream`**，因为接口是 POST + JSON，不能直接用原生 GET-only 的 `EventSource`。

## 验证

后端测试和数据竞争检查：

```bash
go test ./...
go test -race ./...
```

前端有网络可安装依赖时：

```bash
cd frontend
npm install
npm run build
```

此环境已完成 Go 测试和竞态检测；构建 Vue 前端依赖需要访问 npm registry，因此在无法访问 registry 的离线环境中不能验证 `npm install` / `npm run build`。请在本机联网后执行。

Go API 测试：

```bash
curl http://localhost:18080/api/info
curl http://localhost:18080/api/stats
curl -N http://localhost:18080/api/ask/stream -H 'Content-Type: application/json' -d '{"prompt":"Hello"}'

# 多轮上下文：把同一会话中已完成的消息一起提交
curl http://localhost:18080/api/ask -H 'Content-Type: application/json' -d '{"messages":[{"role":"user","content":"什么是 Go channel？"},{"role":"assistant","content":"channel 用于 goroutine 之间通信。"},{"role":"user","content":"它和 C++ 队列有什么区别？"}]}'
```

## MySQL + RabbitMQ + 多实例学习模式

先启动 MySQL 和 RabbitMQ：

```bash
docker compose up -d mysql rabbitmq
```

启动两个 API 实例。两个实例使用同一个 MySQL 和 RabbitMQ，但监听不同端口：

```bash
MYSQL_DSN='root:root@tcp(127.0.0.1:3306)/go_ai_studio?parseTime=true' \
QUEUE_URL='amqp://guest:guest@127.0.0.1:5672/' \
BACKEND=dynamo DYNAMO_URL='http://127.0.0.1:8000' \
DYNAMO_MODEL='Qwen/Qwen2.5-0.5B-Instruct' PORT=18080 go run ./cmd/server
```

```bash
MYSQL_DSN='root:root@tcp(127.0.0.1:3306)/go_ai_studio?parseTime=true' \
QUEUE_URL='amqp://guest:guest@127.0.0.1:5672/' \
BACKEND=dynamo DYNAMO_URL='http://127.0.0.1:8000' \
DYNAMO_MODEL='Qwen/Qwen2.5-0.5B-Instruct' PORT=18081 go run ./cmd/server
```

再启动一个或多个推理消费者：

```bash
QUEUE_URL='amqp://guest:guest@127.0.0.1:5672/' \
BACKEND=dynamo DYNAMO_URL='http://127.0.0.1:8000' \
DYNAMO_MODEL='Qwen/Qwen2.5-0.5B-Instruct' go run ./cmd/inference-worker
```

最后使用 `deploy/nginx.conf` 启动 Nginx，访问 `http://localhost:8088`。非流式 `/ask` 会经过 RabbitMQ；流式 `/ask/stream` 由某个 API 实例直接连接 Dynamo，并通过 Nginx 维持 SSE 长连接。

非流式请求的取消也会跨越 RabbitMQ 传播：API 超时或客户端断开后，会向 `inference.cancels` fanout exchange 发布取消通知；推理 Worker 通过独立的控制队列接收通知，并取消对应任务的 context。该 context 会继续传递给 Dynamo HTTP 请求。取消通知是尽力而为的，RabbitMQ 或 Dynamo 不可用时，API 仍会先向客户端返回超时错误。

RabbitMQ 管理页面是 `http://localhost:15672`，默认账号密码为 `guest/guest`。这是学习用配置，不要直接用于公网部署。

## 10 天项目学习路线

1. 看 `cmd/server/main.go`，理解依赖组装、静态页面和 Gin API 路由。
2. 看 `frontend/vite.config.ts`，理解开发代理及端口。
3. 看 `frontend/src/api/client.ts`，调试 `fetch`、JSON、SSE 帧解析及 AbortController。
4. 看 `frontend/src/App.vue`，理解 Vue 的 `ref`、`computed`、`watch`、事件和状态更新。
5. 追踪 Go `internal/api/server.go` 的 Gin 路由、中间件、`/ask` 和 `/ask/stream`。
6. 追踪 `workerpool/pool.go` 的 `Submit` 和 `runWorker`。
7. 追踪 `internal/dynamo/client.go`，理解 OpenAI-compatible HTTP、SSE 与 Context 取消。
8. 追踪 `internal/queue/rpc.go` 和 `cmd/inference-worker`，理解 RabbitMQ 请求/回复。
9. 自己添加用户登录和会话权限。
10. 让流式与非流式 API 共享 Dynamo 的真实推理并发预算，加入 Eino 的 Tool Calling / MCP，并做 Benchmark、部署。

**安全说明：** 当前项目是本地教学 MVP，未包含登录认证、防滥用与正式部署所需安全措施；请勿直接暴露到公网。页面会将会话保存在当前浏览器本地存储中。

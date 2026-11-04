#!/usr/bin/env bash

# Start the local learning stack in one terminal.
# Ctrl+C stops locally started processes and the MySQL/RabbitMQ containers;
# Docker volumes are preserved.

set -Eeuo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ENV_FILE="${ROOT_DIR}/.env"
LOG_DIR="${ROOT_DIR}/logs"
COMPOSE_FILE="${ROOT_DIR}/middleware/docker-compose.yml"
DYNAMO_DIR="${ROOT_DIR}/middleware/dynamo"

if [[ ! -f "${ENV_FILE}" ]]; then
  cp "${ROOT_DIR}/.env.example" "${ENV_FILE}"
  echo "[dev-up] .env 不存在，已从 .env.example 创建：${ENV_FILE}"
fi

dotenv_value() {
  local key="$1"
  local value
  value="$(awk -v wanted="${key}" '
    /^[[:space:]]*#/ || /^[[:space:]]*$/ { next }
    {
      candidate = $0
      sub(/^[[:space:]]*/, "", candidate)
      split(candidate, fields, "=")
      if (fields[1] == wanted) {
        sub(/^[^=]*=/, "", candidate)
        print candidate
        exit
      }
    }
  ' "${ENV_FILE}")"
  value="${value#"${value%%[![:space:]]*}"}"
  value="${value%"${value##*[![:space:]]}"}"
  if [[ "${value}" == \"*\" && "${value}" == *\" ]]; then
    value="${value:1:${#value}-2}"
  elif [[ "${value}" == \'*\' && "${value}" == *\' ]]; then
    value="${value:1:${#value}-2}"
  fi
  printf '%s' "${value}"
}

config_value() {
  local name="$1"
  if [[ -n "${!name:-}" ]]; then
    printf '%s' "${!name}"
  else
    dotenv_value "${name}"
  fi
}

BACKEND="$(config_value BACKEND)"
DYNAMO_MODEL="$(config_value DYNAMO_MODEL)"
DYNAMO_HTTP_PORT="$(config_value DYNAMO_HTTP_PORT)"
DYNAMO_DISCOVERY_BACKEND="$(config_value DYNAMO_DISCOVERY_BACKEND)"
CUDA_VISIBLE_DEVICES="$(config_value CUDA_VISIBLE_DEVICES)"
PORT="$(config_value PORT)"
WORKER_ID="$(config_value WORKER_ID)"
QUEUE_URL_VALUE="$(config_value QUEUE_URL)"
START_FRONTEND="${START_FRONTEND:-1}"

BACKEND="${BACKEND:-dynamo}"
DYNAMO_MODEL="${DYNAMO_MODEL:-Qwen/Qwen3-0.6B}"
DYNAMO_HTTP_PORT="${DYNAMO_HTTP_PORT:-8000}"
DYNAMO_DISCOVERY_BACKEND="${DYNAMO_DISCOVERY_BACKEND:-file}"
CUDA_VISIBLE_DEVICES="${CUDA_VISIBLE_DEVICES:-0}"
PORT="${PORT:-18080}"
WORKER_ID="${WORKER_ID:-worker-1}"

mkdir -p "${LOG_DIR}"

declare -a CHILD_PIDS=()
INFRA_STARTED=0

log() {
  printf '[dev-up] %s\n' "$*"
}

die() {
  printf '[dev-up] ERROR: %s\n' "$*" >&2
  exit 1
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || die "缺少命令：$1"
}

wait_for_tcp() {
  local host="$1"
  local port="$2"
  local name="$3"
  local timeout="${4:-60}"
  local started now
  started="$(date +%s)"
  while true; do
    if (echo >/dev/tcp/"${host}"/"${port}") >/dev/null 2>&1; then
      log "${name} 已监听 ${host}:${port}"
      return 0
    fi
    now="$(date +%s)"
    if (( now - started >= timeout )); then
      return 1
    fi
    sleep 1
  done
}

wait_for_http() {
  local url="$1"
  local name="$2"
  local timeout="${3:-60}"
  local started now
  started="$(date +%s)"
  while true; do
    if curl -fsS --max-time 2 "${url}" >/dev/null 2>&1; then
      log "${name} 已就绪：${url}"
      return 0
    fi
    now="$(date +%s)"
    if (( now - started >= timeout )); then
      return 1
    fi
    sleep 1
  done
}

wait_for_container_health() {
  local service="$1"
  local name="$2"
  local timeout="${3:-90}"
  local started now container status
  started="$(date +%s)"
  while true; do
    container="$(docker compose --env-file "${ENV_FILE}" -f "${COMPOSE_FILE}" ps -q "${service}" 2>/dev/null || true)"
    if [[ -n "${container}" ]]; then
      status="$(docker inspect --format '{{.State.Health.Status}}' "${container}" 2>/dev/null || true)"
      if [[ "${status}" == "healthy" ]]; then
        log "${name} 健康检查通过"
        return 0
      fi
      if [[ "${status}" == "unhealthy" ]]; then
        return 1
      fi
    fi
    now="$(date +%s)"
    if (( now - started >= timeout )); then
      return 1
    fi
    sleep 1
  done
}

start_process() {
  local name="$1"
  local logfile="$2"
  shift 2
  log "启动 ${name}，日志：${logfile}"
  (
    cd "${ROOT_DIR}"
    exec "$@"
  ) >"${logfile}" 2>&1 &
  CHILD_PIDS+=("$!")
}

stop_all() {
  trap - EXIT INT TERM
  log "正在停止本地服务..."
  for pid in "${CHILD_PIDS[@]:-}"; do
    if kill -0 "${pid}" >/dev/null 2>&1; then
      kill "${pid}" >/dev/null 2>&1 || true
    fi
  done
  for pid in "${CHILD_PIDS[@]:-}"; do
    wait "${pid}" >/dev/null 2>&1 || true
  done
  if (( INFRA_STARTED == 1 )); then
    log "停止 MySQL、RabbitMQ 和 Redis 容器（数据卷保留）"
    docker compose --env-file "${ENV_FILE}" -f "${COMPOSE_FILE}" stop mysql rabbitmq redis >/dev/null 2>&1 || true
  fi
  log "所有本地服务已停止"
}

trap stop_all EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

require_command docker
require_command go
require_command uv
require_command curl
if [[ "${START_FRONTEND}" == "1" ]]; then
  require_command npm
fi

if [[ "${BACKEND}" != "dynamo" ]]; then
  log "警告：当前 BACKEND=${BACKEND}，Dynamo 会启动，但 Go API 不会使用 Dynamo。"
fi

log "启动 MySQL、RabbitMQ 和 Redis"
docker compose --env-file "${ENV_FILE}" -f "${COMPOSE_FILE}" up -d mysql rabbitmq redis
INFRA_STARTED=1
wait_for_tcp 127.0.0.1 3306 MySQL 90 || die "MySQL 启动超时"
wait_for_tcp 127.0.0.1 5672 RabbitMQ 90 || die "RabbitMQ 启动超时"
wait_for_tcp 127.0.0.1 6379 Redis 90 || die "Redis 启动超时"
wait_for_container_health mysql MySQL 90 || die "MySQL 健康检查失败"
wait_for_container_health rabbitmq RabbitMQ 90 || die "RabbitMQ 健康检查失败"
wait_for_container_health redis Redis 90 || die "Redis 健康检查失败"

if [[ ! -x "${DYNAMO_DIR}/.venv/bin/python" ]]; then
  log "Dynamo 虚拟环境不存在，执行 uv sync"
  (cd "${DYNAMO_DIR}" && uv sync)
fi

start_process "Dynamo frontend" "${LOG_DIR}/dynamo-frontend.log" \
  bash -lc "cd '${DYNAMO_DIR}' && exec uv run python -m dynamo.frontend --http-port '${DYNAMO_HTTP_PORT}' --discovery-backend '${DYNAMO_DISCOVERY_BACKEND}'"
wait_for_http "http://127.0.0.1:${DYNAMO_HTTP_PORT}/health" "Dynamo frontend" 90 || die "Dynamo frontend 启动超时"

start_process "Dynamo SGLang worker" "${LOG_DIR}/dynamo-sglang.log" \
  bash -lc "cd '${DYNAMO_DIR}' && exec env CUDA_VISIBLE_DEVICES='${CUDA_VISIBLE_DEVICES}' uv run python -m dynamo.sglang --model-path '${DYNAMO_MODEL}' --discovery-backend '${DYNAMO_DISCOVERY_BACKEND}'"

start_process "Go API" "${LOG_DIR}/api-process.log" \
  go run ./backend/cmd/server

if [[ -n "${QUEUE_URL_VALUE}" ]]; then
  start_process "inference-worker (${WORKER_ID})" "${LOG_DIR}/inference-worker-process.log" \
    env WORKER_ID="${WORKER_ID}" go run ./backend/cmd/inference-worker
else
  log "QUEUE_URL 为空，跳过 inference-worker"
fi

if [[ "${START_FRONTEND}" == "1" ]]; then
  if [[ ! -x "${ROOT_DIR}/frontend/node_modules/.bin/vite" ]]; then
    log "前端依赖不存在，执行 npm ci"
    (cd "${ROOT_DIR}/frontend" && npm ci)
  fi
  start_process "React Vite" "${LOG_DIR}/frontend.log" \
    npm --prefix "${ROOT_DIR}/frontend" run dev -- --host 127.0.0.1
else
  log "START_FRONTEND=0，跳过 React Vite；请另开终端运行 make dev-frontend"
fi

log "全部服务已启动"
if [[ "${START_FRONTEND}" == "1" ]]; then
  log "前端：http://127.0.0.1:5173"
fi
log "Go API：http://127.0.0.1:${PORT}"
log "Dynamo：http://127.0.0.1:${DYNAMO_HTTP_PORT}"
log "按 Ctrl+C 停止全部本地服务"

set +e
wait -n "${CHILD_PIDS[@]}"
exit_code=$?
set -e
log "检测到一个服务退出，正在停止其余服务"
exit "${exit_code}"

#!/usr/bin/env bash

set -Eeuo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LOG_DIR="${ROOT_DIR}/logs"
mkdir -p "${LOG_DIR}"

command -v npm >/dev/null 2>&1 || {
  echo "[dev-frontend] ERROR: 缺少 npm" >&2
  exit 1
}

if [[ ! -x "${ROOT_DIR}/frontend/node_modules/.bin/vite" ]]; then
  echo "[dev-frontend] 前端依赖不存在，执行 npm ci"
  (cd "${ROOT_DIR}/frontend" && npm ci)
fi

echo "[dev-frontend] 启动 React Vite：http://127.0.0.1:5173"
cd "${ROOT_DIR}"
exec npm --prefix "${ROOT_DIR}/frontend" run dev -- --host 127.0.0.1 \
  2>&1 | tee -a "${LOG_DIR}/frontend.log"

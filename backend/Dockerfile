# syntax=docker/dockerfile:1

# Build the React application inside the same image build so the API image is
# self-contained for production deployment.
FROM node:22-alpine AS frontend-build

WORKDIR /src/frontend
COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci
COPY frontend/ ./

# VITE_API_KEY is optional. If API_KEY is enabled for the gateway, passing the
# same value here allows the browser client to authenticate its API requests.
ARG VITE_API_KEY=
ENV VITE_API_KEY=${VITE_API_KEY}
RUN npm run build

FROM golang:1.27-bookworm AS backend-build

WORKDIR /src/backend
COPY backend/go.mod backend/go.sum ./
RUN go mod download
COPY backend/ ./

RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/inference-worker ./cmd/inference-worker

FROM alpine:3.22 AS runtime

RUN apk add --no-cache ca-certificates
WORKDIR /app

COPY --from=backend-build /out/server /app/server
COPY --from=backend-build /out/inference-worker /app/inference-worker
COPY --from=frontend-build /src/frontend/dist /app/frontend/dist

ENV PORT=8080 \
    WEB_DIST=/app/frontend/dist \
    GIN_MODE=release

EXPOSE 8080

FROM runtime AS api
ENTRYPOINT ["/app/server"]

FROM runtime AS worker
ENTRYPOINT ["/app/inference-worker"]

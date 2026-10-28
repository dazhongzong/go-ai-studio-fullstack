.PHONY: test race vet integration frontend build run run-mock infra-up infra-down docker-build docker-up docker-down docker-logs

test:
	go test ./backend/...

race:
	go test -race ./backend/...

vet:
	go vet ./backend/...

integration:
	docker compose -f middleware/docker-compose.yml up -d rabbitmq
	cd backend && RABBITMQ_URL=amqp://guest:guest@127.0.0.1:5672/ go test -tags=integration ./internal/queue

frontend:
	cd frontend && npm ci && npm run build

build: frontend
	mkdir -p bin
	go build -o bin/server ./backend/cmd/server
	go build -o bin/inference-worker ./backend/cmd/inference-worker

run:
	go run ./backend/cmd/server

run-mock:
	BACKEND=mock go run ./backend/cmd/server

infra-up:
	docker compose -f middleware/docker-compose.yml up -d mysql rabbitmq

infra-down:
	docker compose -f middleware/docker-compose.yml down

docker-build:
	docker compose -f middleware/docker-compose.yml build

docker-up:
	docker compose -f middleware/docker-compose.yml up -d --build

docker-down:
	docker compose -f middleware/docker-compose.yml down

docker-logs:
	docker compose -f middleware/docker-compose.yml logs -f --tail=200

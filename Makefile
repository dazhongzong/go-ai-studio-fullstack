.PHONY: test race vet frontend build run run-mock infra-up infra-down

test:
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./...

frontend:
	cd frontend && npm ci && npm run build

build: frontend
	go build ./cmd/server
	go build ./cmd/inference-worker

run:
	go run ./cmd/server

run-mock:
	BACKEND=mock go run ./cmd/server

infra-up:
	docker compose up -d mysql rabbitmq

infra-down:
	docker compose down

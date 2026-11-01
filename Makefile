COMPOSE := docker compose -f deploy/docker/docker-compose.yml

.PHONY: run test vet build up down logs redis

# Gateway local contra Redis del compose (make redis) y cobros en :8080.
run:
	JWT_SECRET="$${JWT_SECRET:-change-me-in-production-needs-32-chars!!}" go run ./cmd/gateway

test:
	go test -race ./...

vet:
	go vet ./...

build:
	CGO_ENABLED=0 go build -o bin/gateway ./cmd/gateway

redis:
	$(COMPOSE) up -d redis

up:
	$(COMPOSE) up -d --build

down:
	$(COMPOSE) down

logs:
	$(COMPOSE) logs -f gateway

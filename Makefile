.PHONY: build test image up down

build:
	go build ./...

# The tests start Postgres (and Redis) in Docker through testcontainers.
test:
	go test ./...

image:
	docker build -t tknowledge/tk-personal:latest .

up:
	docker compose up -d

down:
	docker compose down

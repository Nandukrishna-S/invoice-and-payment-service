TEST_DATABASE_URL ?= postgres://invoice:invoice@localhost:5432/invoice?sslmode=disable

.PHONY: build test lint up down

build:
	go build ./...

# Integration tests need Postgres; compose provides it.
test:
	docker compose up -d --wait postgres
	TEST_DATABASE_URL='$(TEST_DATABASE_URL)' go test -race -coverpkg=./internal/... -coverprofile=cover.out ./...
	go tool cover -func=cover.out | tail -1

lint:
	gofmt -l .
	go vet ./...
	golangci-lint run

up:
	docker compose up --build -d

down:
	docker compose down

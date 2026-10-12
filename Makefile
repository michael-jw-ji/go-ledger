.PHONY: up down run test test-integration lint web web-install web-test

# Extra flags for go test, e.g. TESTFLAGS=-race (needs cgo; CI sets it).
TESTFLAGS ?=

up:
	docker compose up -d --wait

down:
	docker compose down

run:
	go run ./cmd/ledger

test:
	go test $(TESTFLAGS) ./...

# Starts throwaway Postgres/Redis containers with Testcontainers.
test-integration:
	go test $(TESTFLAGS) -tags=integration -count=1 ./test/...

lint:
	go vet ./...
	docker run --rm -v "$(CURDIR):/app" -w /app golangci/golangci-lint:v2.14.0 golangci-lint run --build-tags=integration

# React test console on http://localhost:5173 (proxies /api to :8088).
web-install:
	cd web && npm install

web:
	cd web && npm run dev

web-test:
	cd web && npm test

.PHONY: up down obs run recon test test-integration lint web web-install web-test load-steady load-hot

# Stop Git Bash on Windows rewriting container paths like /app in docker args.
export MSYS_NO_PATHCONV := 1

# Extra flags for go test, e.g. TESTFLAGS=-race (needs cgo; CI sets it).
TESTFLAGS ?=

up:
	docker compose up -d --wait

down:
	docker compose --profile obs down

# Prometheus on :9090 and Grafana on :3000, scraping the API from make run.
obs:
	docker compose --profile obs up -d --wait

# One-off reconciliation; exits 1 if anything disagrees.
recon:
	go run ./cmd/ledger recon

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

# k6 load tests via Docker against the API from make run. Override RATE and
# DURATION, e.g. make load-hot RATE=500 DURATION=2m.
K6 = docker run --rm -i --add-host=host.docker.internal:host-gateway \
	-v "$(CURDIR)/loadtest:/scripts" -w /scripts -e RATE -e DURATION -e BASE_URL grafana/k6:1.2.0 run

load-steady:
	$(K6) steady.js

load-hot:
	$(K6) hot_account.js

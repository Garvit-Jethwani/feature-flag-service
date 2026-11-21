SHELL := /bin/bash

.PHONY: env up down logs test test-unit lint fmt build

# Create a local .env with random credentials. Never overwrites an existing
# file, and never prints the generated values.
env:
	@if [ -f .env ]; then \
		echo ".env already exists; leaving it alone"; \
	else \
		umask 077; \
		{ \
			echo "POSTGRES_PASSWORD=$$(openssl rand -hex 24)"; \
			echo "API_KEYS=local-admin:admin:$$(openssl rand -hex 32),local-reader:reader:$$(openssl rand -hex 32)"; \
		} > .env; \
		echo "wrote .env with random local credentials"; \
	fi

# Build and start the API and Postgres.
up: env
	docker compose up --build -d --wait

down:
	docker compose down

logs:
	docker compose logs -f api

# Run every test, including the ones that need Postgres.
test: env
	docker compose up -d --wait db
	@set -a; . ./.env; set +a; \
	export TEST_DATABASE_URL="postgres://flags:$${POSTGRES_PASSWORD}@127.0.0.1:$${POSTGRES_PORT:-5432}/flags?sslmode=disable"; \
	go test -race -count=1 ./...

# Run only the tests that need no database (database-backed tests skip).
test-unit:
	go test -race -count=1 ./...

lint:
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then echo "gofmt needed on:"; echo "$$unformatted"; exit 1; fi
	go vet ./...

fmt:
	gofmt -w .

build:
	CGO_ENABLED=0 go build -trimpath -o bin/server ./cmd/server

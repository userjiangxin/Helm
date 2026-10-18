.PHONY: test test-go test-ts dev-db helmd

test: test-go test-ts

test-go:
	cd daemon && go vet ./... && go test ./...

test-ts:
	cd server && pnpm typecheck && pnpm test

dev-db:
	docker compose up -d postgres

helmd:
	cd daemon && CGO_ENABLED=0 go build -o ../bin/helmd ./cmd/helmd

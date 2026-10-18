.PHONY: test test-go test-ts test-web dev-db helmd web demo

test: test-go test-ts test-web

test-go:
	cd daemon && go vet ./... && go test ./...

test-ts:
	cd server && pnpm typecheck && pnpm test

dev-db:
	docker compose up -d postgres

helmd:
	cd daemon && CGO_ENABLED=0 go build -o ../bin/helmd ./cmd/helmd

test-web:
	cd web && pnpm typecheck

web:
	cd web && pnpm build

# Zero-install single machine: embedded Postgres, board served by the server on :8080.
demo: web helmd
	cd server && DATABASE_URL=pglite:../.helm-data pnpm start

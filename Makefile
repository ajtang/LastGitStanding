.PHONY: dev web test integration balance proto build deps

dev:          ## the whole backend, one process, in-memory unless REDIS_URL/DATABASE_URL are set
	go run ./cmd/lgs dev

web:
	npm --prefix web run dev

deps:         ## local Redis and Postgres, no Docker. Prints the URLs to export.
	scripts/dev-deps.sh start

test:
	go test ./...
	npm --prefix web run typecheck

integration:  ## needs REDIS_URL and DATABASE_URL
	go test -count=1 ./test/integration/

balance:      ## headless games. Run before and after touching a number in the catalog.
	go run ./cmd/lgs balance

proto:
	scripts/gen-proto.sh

build:
	go build -o bin/lgs ./cmd/lgs

# `make` builds the dashboard app and then the binary that embeds it.

.PHONY: build web test lint dev-station clean

build: web
	go build -o bin/ ./cmd/...

web:
	cd web && bun install --frozen-lockfile
	rm -rf web/dist/assets web/dist/index.html
	cd web && bun run build

test:
	go vet ./...
	go test ./...
	cd web && bun run typecheck && bun run lint

# A stand-in for the station, for running icegator locally.
dev-station:
	go run ./cmd/fakestation

clean:
	rm -rf bin web/dist/assets web/dist/index.html

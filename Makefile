# `make` builds the dashboard app and then the binary that embeds it.

.PHONY: build web test lint dev-station bump bump-minor bump-major clean

# The version stamped into the binary. On a release tag it is v0.5.0, and
# between releases it looks like v0.5.0-3-gd85f798.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

build: web
	go build -ldflags "-X main.version=$(VERSION)" -o bin/ ./cmd/...

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

# Cut a release. `make bump` raises the last number of the version, and the
# other two raise the minor or major number.
bump:
	cd web && bun run bump

bump-minor:
	cd web && bun run bump minor

bump-major:
	cd web && bun run bump major

clean:
	rm -rf bin web/dist/assets web/dist/index.html

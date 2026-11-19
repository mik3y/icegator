# Builds the icegator image. It holds one binary with the dashboard inside it.
# Run it like this.
#
#   docker run -v ./icegator.yaml:/etc/icegator/icegator.yaml \
#     -v icegator-state:/data -p 8000:8000 -p 8080:8080 ghcr.io/mik3y/icegator
#
# Both build stages run on the builder's own architecture and Go
# cross-compiles, so a multi-platform build needs no emulation.

FROM --platform=$BUILDPLATFORM oven/bun:1.4.4 AS web
WORKDIR /src/web
COPY web/package.json web/bun.lock ./
RUN bun install --frozen-lockfile
COPY web/ ./
RUN bun run build

FROM --platform=$BUILDPLATFORM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/web/dist/ web/dist/
ARG TARGETOS TARGETARCH
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o /out/icegator ./cmd/icegator

# Distroless static has CA certificates and time zones and nothing else. It
# runs as root so that a volume mounted at /data is writable however the
# host created it.
FROM gcr.io/distroless/static-debian12
ARG VERSION=dev
ENV ICEGATOR_VERSION=$VERSION
LABEL org.opencontainers.image.title="icegator" \
      org.opencontainers.image.source="https://github.com/mik3y/icegator" \
      org.opencontainers.image.version=$VERSION
COPY --from=build /out/icegator /usr/local/bin/icegator
# state_dir is relative to here by default, so state lands in /data/state.
WORKDIR /data
EXPOSE 8000 8080
ENTRYPOINT ["/usr/local/bin/icegator"]
CMD ["-config", "/etc/icegator/icegator.yaml"]

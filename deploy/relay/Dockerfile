# magnum-relay: receives the GitHub App's webhooks and keeps their routing envelopes for magnum's daemon
# (deploy/relay/README.md). Build from the repository root:
#
#   docker build -f deploy/relay/Dockerfile -t magnum-relay .
#
# The binary is static (CGO off: modernc.org/sqlite is pure Go) and runs in an empty image as a non-root
# user; /data holds the journal. The build copies only the relay's packages and the module files.

ARG GO_VERSION=1.27.3

FROM golang:${GO_VERSION}-alpine AS build
ENV CGO_ENABLED=0 GOTOOLCHAIN=local GOFLAGS=-trimpath
WORKDIR /src
COPY go.mod go.sum ./
COPY cmd/magnum-relay ./cmd/magnum-relay
COPY internal/relay ./internal/relay
ARG VERSION=dev
RUN go build -ldflags "-s -w -X main.version=${VERSION}" -o /out/magnum-relay ./cmd/magnum-relay \
 && mkdir -p /out/data

FROM scratch
COPY --from=build /out/magnum-relay /magnum-relay
# Owned by the user the relay runs as, so a new named volume mounted here starts writable.
COPY --from=build --chown=65532:65532 /out/data /data
USER 65532:65532
ENV MAGNUM_RELAY_ADDR=:8080 MAGNUM_RELAY_DB=/data/relay.db
EXPOSE 8080
VOLUME ["/data"]
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 CMD ["/magnum-relay", "healthcheck"]
ENTRYPOINT ["/magnum-relay"]
CMD ["serve"]

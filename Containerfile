# agentbus: the broker and the CLI in one small image.
#
#   podman build -f Containerfile --build-arg VERSION=v0.1.0 -t agentbus:v0.1.0 .
#   (docker build takes the same arguments; "make image" runs this)
#
# The image runs "agentbus serve" as an unprivileged user. Config and
# tokens live in /config, the broker's data and audit log in /data; mount
# a volume on each. See docs/CONTAINERS.md.

ARG GO_VERSION=1.26
ARG ALPINE_VERSION=3.22

# Build on the machine's own architecture and cross-compile, so a
# multi-arch build doesn't run the compiler under emulation.
FROM --platform=$BUILDPLATFORM docker.io/library/golang:${GO_VERSION}-alpine AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOWORK=off GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/agentbus ./cmd/agentbus

# Alpine rather than scratch: "agentbus handle --exec" and agent handlers
# run shell commands, and a shell is what makes "exec" into the container
# useful. It also brings the CA certificates for outbound TLS.
FROM docker.io/library/alpine:${ALPINE_VERSION}
RUN addgroup -g 65532 agentbus \
 && adduser -D -H -u 65532 -G agentbus agentbus \
 && install -d -o agentbus -g agentbus -m 0700 /config /data
COPY --from=build /out/agentbus /usr/local/bin/agentbus

# Every agentbus command in the container finds the config here, so
# "exec ... agentbus console" needs no flags.
ENV AGENTBUS_CONFIG_DIR=/config \
    AGENTBUS_STATE_DIR=/data
USER 65532:65532
VOLUME ["/config", "/data"]
# 1883 plaintext MQTT, 8883 MQTT over TLS. Which one is open is the
# config's "listen"; mcp_listen and join_listen are yours to choose.
EXPOSE 1883 8883
ENTRYPOINT ["agentbus"]
CMD ["serve"]

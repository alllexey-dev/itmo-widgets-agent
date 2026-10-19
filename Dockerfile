# syntax=docker/dockerfile:1
# BuildKit sets BUILDPLATFORM; the legacy builder needs --build-arg BUILDPLATFORM=linux/<arch>.
ARG BUILDPLATFORM
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
# Build identity for X-Client: only the release workflow sets channel github and the source repository.
ARG TARGETOS TARGETARCH VERSION=dev COMMIT= CHANNEL=custom SOURCE=local
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION} \
      -X main.commit=${COMMIT} -X main.channel=${CHANNEL} -X main.source=${SOURCE}" -o /out/itmo-widgets-agent ./cmd/itmo-widgets-agent \
 && mkdir -m 700 /out/data

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/itmo-widgets-agent /itmo-widgets-agent
COPY --from=build --chown=10001:10001 /out/data /data
USER 10001:10001
ENV IW_STATE_DIR=/data
VOLUME /data
HEALTHCHECK --interval=60s --timeout=5s --start-period=90s CMD ["/itmo-widgets-agent", "health"]
ENTRYPOINT ["/itmo-widgets-agent"]
CMD ["run"]

# Cross-compile natively for the target platform: no QEMU emulation of the Go toolchain.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
COPY internal ./internal
ARG ARKNET_VERSION=
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags "${ARKNET_VERSION:+-X main.version=$ARKNET_VERSION}" -o /arknet-agent .

FROM alpine:3.23
RUN addgroup -g 10001 arknet && adduser -D -u 10001 -G arknet arknet \
    && install -d -o 10001 -g 10001 -m 0700 /srv/arknet/transfers /srv/arknet/media
COPY --from=build /arknet-agent /usr/local/bin/arknet-agent
USER arknet
EXPOSE 8081
ENTRYPOINT ["/usr/local/bin/arknet-agent"]

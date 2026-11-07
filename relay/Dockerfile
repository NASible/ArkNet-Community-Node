# Cross-compile natively for the target platform: no QEMU emulation of the Go toolchain.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/arknet-relay .

FROM alpine:3.23
COPY --from=build /out/arknet-relay /usr/local/bin/arknet-relay
ENV ARKNET_RELAY_BIND_ADDR=0.0.0.0:7443 \
    ARKNET_RELAY_CONTROL_BIND_ADDR=0.0.0.0:7080
USER 65534:65534
EXPOSE 7443 7080
HEALTHCHECK --interval=10s --timeout=3s --start-period=2s --retries=3 CMD wget -q -O - http://127.0.0.1:7080/health | grep -qx ok
ENTRYPOINT ["/usr/local/bin/arknet-relay"]

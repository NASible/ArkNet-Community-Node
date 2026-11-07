# Cross-compile natively for the target platform: no QEMU emulation of the Go toolchain.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -o /arknet-core .

FROM alpine:3.23
RUN addgroup -g 10001 arknet && adduser -D -u 10001 -G arknet arknet
COPY --from=build /arknet-core /usr/local/bin/arknet-core
USER arknet
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/arknet-core"]

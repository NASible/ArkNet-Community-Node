# Cross-compile natively for the target platform: no QEMU emulation of the Go toolchain.
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod ./
COPY *.go ./
COPY backend ./backend
COPY engine ./engine
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /arknet-transfer .

FROM ubuntu:24.04
ENV DEBIAN_FRONTEND=noninteractive \
    PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates python3 python3-libtorrent python3-cryptography iptables util-linux \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --gid 10001 arknet \
    && useradd --uid 10001 --gid 10001 --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin arknet \
    && install -d -o 10001 -g 10001 -m 0700 /srv/arknet/transfers /srv/arknet/transfer-state /srv/arknet/media
COPY --from=build /arknet-transfer /usr/local/bin/arknet-transfer
COPY worker.py relay_connector.py /usr/local/lib/arknet/
COPY entrypoint.py /usr/local/lib/arknet/entrypoint.py
RUN chmod 0555 /usr/local/lib/arknet/*.py
# Startup installs the namespace firewall, then setpriv removes all capabilities
# and switches to 10001:10001 before the service or worker can process input.
USER 0:0
EXPOSE 8084
ENTRYPOINT ["/usr/bin/python3", "-B", "/usr/local/lib/arknet/entrypoint.py"]

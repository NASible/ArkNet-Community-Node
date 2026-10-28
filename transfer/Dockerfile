FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
COPY backend ./backend
COPY engine ./engine
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /arknet-transfer .

FROM ubuntu:24.04
ENV DEBIAN_FRONTEND=noninteractive \
    PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates python3 python3-libtorrent \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --gid 10001 arknet \
    && useradd --uid 10001 --gid 10001 --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin arknet \
    && install -d -o 10001 -g 10001 -m 0700 /srv/arknet/transfers /srv/arknet/transfer-state /srv/arknet/media
COPY --from=build /arknet-transfer /usr/local/bin/arknet-transfer
COPY --chown=10001:10001 worker.py /usr/local/lib/arknet/worker.py
RUN chmod 0555 /usr/local/lib/arknet/worker.py
USER 10001:10001
EXPOSE 8084
ENTRYPOINT ["/usr/local/bin/arknet-transfer"]

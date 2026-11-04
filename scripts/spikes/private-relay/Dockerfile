# Throwaway libtorrent peer image for the Prompt 48 relay spike.
FROM ubuntu:24.04
RUN apt-get update && apt-get install -y --no-install-recommends python3 python3-libtorrent \
    && rm -rf /var/lib/apt/lists/*

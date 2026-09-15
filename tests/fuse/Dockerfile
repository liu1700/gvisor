FROM debian:trixie-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
    bash ca-certificates fuse3 gcc git libfuse3-dev pkg-config python3 sqlite3 \
 && rm -rf /var/lib/apt/lists/*

ARG JUICEFS_SHA256
COPY juicefs /usr/local/bin/juicefs
RUN echo "${JUICEFS_SHA256}  /usr/local/bin/juicefs" | sha256sum -c - \
 && chmod 0755 /usr/local/bin/juicefs

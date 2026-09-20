# Production SuperDB image: builds the server and runs hosted mode.
FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/superdb ./cmd/superdb

FROM debian:bookworm-slim
RUN useradd -r -u 10001 superdb && mkdir -p /data && chown superdb /data
COPY --from=build /out/superdb /usr/local/bin/superdb
USER superdb
# Durable state lives in /data: mount a persistent volume there.
VOLUME ["/data"]
EXPOSE 7432
ENV SUPERDB_HOST=0.0.0.0 SUPERDB_PORT=7432 SUPERDB_DATA_DIR=/data SUPERDB_MODE=wal
ENTRYPOINT ["superdb", "serve"]

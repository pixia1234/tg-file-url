FROM golang:1.26.1-bookworm AS builder

WORKDIR /src

ARG TARGETOS=linux
ARG TARGETARCH

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=1 GOOS="$TARGETOS" GOARCH="$TARGETARCH" go build -o /out/tg-file-url ./cmd/tg-file-url

FROM debian:12-slim

RUN apt-get update && \
    apt-get install -y --no-install-recommends \
        ca-certificates \
        sqlite3 \
        libsqlite3-0 \
    && apt-get clean && \
    rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY --from=builder /out/tg-file-url /app/tg-file-url

ENV PORT=8080 \
    BIND_ADDRESS=0.0.0.0 \
    SQLITE_PATH=/app/data/tg-file-url.db

EXPOSE 8080

CMD ["/app/tg-file-url"]

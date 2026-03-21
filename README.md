## tg-file-url

`tg-file-url` is a Go + SQLite Telegram file-to-link service.

Core flow:

1. A user sends a Telegram file to the bot, or replies to a file with `/link`.
2. The bot forwards that file into `BIN_CHANNEL`.
3. Metadata is stored in SQLite.
4. The service generates stable watch and download URLs.
5. Files are streamed directly from Telegram over MTProto.

## Stack

- Go 1.26.1
- SQLite
- Native Go HTTP server
- Telegram Bot API for updates and message actions
- MTProto for file streaming

## Features

- Private chat file ingestion
- `/link` on replied media messages
- Stable stream and download links
- Watch page with inline preview
- Owner commands:
  `/status` `/stats` `/users` `/authorize` `/deauthorize` `/listauth`
  `/ban` `/unban` `/broadcast` `/log` `/restart`
- Single-container Docker Compose setup

## Configure

Copy `config_sample.env` to `config.env` and set at least:

```env
BOT_TOKEN=123456:ABCDEF
API_ID=12345
API_HASH=your_api_hash
BIN_CHANNEL=-1001234567890
OWNER_ID=123456789
PUBLIC_BASE_URL=http://your-server-ip:8080
SQLITE_PATH=data/tg-file-url.db
LOG_PATH=data/tg-file-url.log
```

## Run

```bash
go mod tidy
go run ./cmd/filetolink
```

## Build

```bash
go build -o tg-file-url ./cmd/filetolink
```

## Test

```bash
go test ./...
```

## Docker

```bash
docker build -t tg-file-url .
docker run --rm -p 8080:8080 --env-file config.env tg-file-url
```

## Docker Compose

```bash
cp compose.env.example compose.env
# edit compose.env

docker-compose pull
docker-compose up -d
```

Service:

- `tg-file-url` on `http://<server-ip>:8080`

Important:

- `PUBLIC_BASE_URL` should stay IP-based for now if you are not enabling HTTPS yet.
- `TELEGRAM_API_BASE_URL` can stay on the official Bot API endpoint: `https://api.telegram.org`.

## HTTP Endpoints

- `GET /status`
- `GET /watch/{token}`
- `GET /{token}`

## Project Layout

```text
cmd/filetolink          main program
internal/config         env parsing and runtime config
internal/database       SQLite storage
internal/telegram       Telegram Bot API client and bot logic
internal/httpserver     HTTP routes, preview page, download proxy
internal/files          link, hash, filename, media helpers
```

## License

This repository remains under the original license in [LICENSE](LICENSE).

# Hammer

Lightweight HTTP(S) load-test tool in Go.

## Build

Requires Go 1.24+ (uses modules).

```shell
go build -o hammer .
```

Cross-compile for Linux from macOS:

```shell
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o hammer.linux .
```

## Run

```shell
./hammer -profile profiles/httpbin.json -rps 100 -duration 30s
```

### Flags

| Flag         | Default       | Description                                              |
|--------------|---------------|----------------------------------------------------------|
| `-profile`   | (required)    | Path to traffic profile JSON file                        |
| `-rps`       | `100`         | Target requests per second                               |
| `-duration`  | `0`           | Total run time (e.g. `30s`, `5m`); `0` runs until Ctrl+C |
| `-timeout`   | `30s`         | Per-request HTTP timeout                                 |
| `-slow`      | `1s`          | Log responses slower than this threshold                 |
| `-proxy`     | `""`          | HTTP proxy URL (e.g. `http://127.0.0.1:8888`)            |
| `-insecure`  | `false`       | Skip TLS certificate verification                        |
| `-debug`     | `false`       | Verbose request/response logging                         |
| `-stats-addr`| `:9001`       | Address for `/stats` endpoint; empty to disable          |

While running, `GET http://localhost:9001/stats` returns counters and a per-call breakdown.

On exit (Ctrl+C or `-duration` elapses) a summary is printed with min/p50/p90/p95/p99/max latency.

## Profile format

A profile file is a stream of JSON `Call` objects:

```json
{
  "Weight": 40,
  "Method": "GET",
  "URL": "https://httpbin.org/get",
  "Body": ""
}
{
  "Weight": 20,
  "Method": "POST",
  "URL": "https://httpbin.org/post",
  "Body": "{\"test\":\"hammer\"}",
  "Type": "REST"
}
```

Fields:

- `Weight` – relative weight for random selection (must be positive)
- `Method` – HTTP method (GET/POST/PUT/PATCH/DELETE/…)
- `URL` – full request URL
- `Body` – request body (optional)
- `Type` – optional Content-Type hint for write methods:
  - `"REST"` → `application/json; charset=utf-8`
  - `"WWW"` → `application/x-www-form-urlencoded`
  - any other non-empty value is used as the Content-Type directly

## Files

- `hammer.go` – the load generator
- `server.go` – tiny echo server for local testing (`go run server.go` on port 9000)
- `profile/` – traffic-profile parser
- `profiles/` – example profiles

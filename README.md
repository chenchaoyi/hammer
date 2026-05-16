# Hammer

Lightweight, single-binary HTTP(S) load generator written in Go.

- **Constant-rate**: drives a configurable RPS using a tick loop
- **Weighted traffic mix**: define multiple endpoints with relative weights in a JSON file
- **Per-call headers**: set `Authorization`, API keys, tracing headers, … in the profile
- **URL / body templating**: `{{ uuid }}`, `{{ randInt 1000000 }}`, `{{ pickOne "us" "eu" }}` to avoid cache-hit skew
- **Live monitor**: per-second `SendPS / ReceivePS / AvgRT / Pending / Err% / Slow%` log line
- **Final report**: latency percentiles + per–status-code histogram + network-error categories
- **Structured JSON report** on exit (`-json-out`) for CI / baseline diffing
- **HTTP stats endpoint**: `GET /stats` while running
- **Graceful shutdown**: SIGINT/SIGTERM or `-duration`
- **Per-request timeout**: stops slow servers from piling up goroutines

## Install

Requires Go 1.24+.

```shell
git clone https://github.com/chenchaoyi/hammer
cd hammer
go build -o hammer .
```

Cross-compile for Linux:

```shell
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o hammer.linux .
```

## Quick start

Hit `httpbin.org` for 10 seconds at 50 rps:

```shell
./hammer -profile profiles/httpbin.json -rps 50 -duration 10s
```

Expected output:

```
2026/05/16 11:00:00 Stats endpoint: http://:9001/stats
2026/05/16 11:00:00 Hammering @ 50 rps for 10s (timeout=30s, profile=profiles/httpbin.json)
2026/05/16 11:00:01  SendPS:   50  ReceivePS:   49  AvgRT: 0.1820s  Pending: 1  Err: 0|0.00%  Slow: 0.00%
...
2026/05/16 11:00:10 Stopping...

=== Summary ===
Sent:     500
Received: 498
Errors:   0
Slow:     0  (> 1s)

Status codes:
  200: 498

Latency (ms):  min=140.12  p50=180.34  p90=220.55  p95=240.07  p99=310.82  max=410.43

--- Per call ---
API: GET https://httpbin.org/get
Total Call: 332
Avg RT: 0.1830s
+++++++
API: POST https://httpbin.org/post
Total Call: 166
Avg RT: 0.1815s
+++++++
```

If the target returns mixed statuses or network errors, the summary breaks them down:

```
Status codes:
  200: 412
  404: 78
  500: 8

Network errors:
  timeout: 2
  conn: 1
```

## Flags

| Flag           | Default | Description                                                              |
|----------------|---------|--------------------------------------------------------------------------|
| `-profile`     | _(required)_ | Path to traffic profile JSON file                                   |
| `-rps`         | `100`   | Target requests per second                                               |
| `-duration`    | `0`     | Total run time (e.g. `30s`, `5m`); `0` runs until `Ctrl+C`               |
| `-timeout`     | `30s`   | Per-request HTTP timeout                                                 |
| `-slow`        | `1s`    | Log + count responses slower than this threshold                         |
| `-proxy`       | `""`    | HTTP proxy URL, e.g. `http://127.0.0.1:8888`                             |
| `-insecure`    | `false` | Skip TLS certificate verification                                        |
| `-debug`       | `false` | Verbose request/response logging (one log line per request)              |
| `-stats-addr`  | `:9001` | Address for the `/stats` HTTP endpoint; empty string disables it         |
| `-ok`          | `""`    | Extra status codes treated as success (e.g. `-ok "404,409"`). 2xx and 3xx are always OK. |
| `-json-out`    | `""`    | Path to write a structured JSON report on exit; empty to skip               |

Run `./hammer -h` for the live list.

## Profile format

A profile file is a **stream of JSON `Call` objects** (no enclosing array — just concatenate them, whitespace between objects is fine).

```json
{
  "Weight": 40,
  "Method": "GET",
  "URL": "https://httpbin.org/get",
  "Body": "",
  "Headers": {
    "Authorization": "Bearer your-token",
    "X-Trace-Id": "hammer-load-test"
  }
}
{
  "Weight": 20,
  "Method": "POST",
  "URL": "https://httpbin.org/post",
  "Body": "{\"test\":\"hammer\"}",
  "Type": "REST"
}
```

| Field     | Required | Description                                                                |
|-----------|----------|----------------------------------------------------------------------------|
| `Weight`  | yes      | Positive float; selection probability is `Weight / sum(Weight)`            |
| `Method`  | yes      | HTTP method (`GET`, `POST`, `PUT`, `PATCH`, `DELETE`, …)                   |
| `URL`     | yes      | Full request URL including scheme                                          |
| `Body`    | no       | Request body string (used for `POST`/`PUT`/`PATCH`)                        |
| `Type`    | no       | Content-Type hint for write methods (see below)                            |
| `Headers` | no       | Map of HTTP headers sent with this call; overrides the `Type`-inferred Content-Type if specified |

### URL / Body templating

`URL` and `Body` fields are run through Go's `text/template` package on every request, so you can inject random or per-request values to avoid hitting the same cache key every time.

```json
{
  "Weight": 1,
  "Method": "POST",
  "URL": "https://api.example.com/v1/users/{{ randInt 1000000 }}",
  "Body": "{\"id\":\"{{ uuid }}\",\"region\":\"{{ pickOne \"us\" \"eu\" \"ap\" }}\",\"score\":{{ randIntRange 0 100 }}}",
  "Type": "REST"
}
```

Functions available inside `{{ }}`:

| Function                      | Returns                                                |
|-------------------------------|--------------------------------------------------------|
| `uuid`                        | A random UUID v4 string                                |
| `randInt N`                   | A random int in `[0, N)`                               |
| `randIntRange LO HI`          | A random int in `[LO, HI)`                             |
| `randString N`                | A random alphanumeric string of length `N`             |
| `pickOne A B C ...`           | One of the provided string arguments, chosen at random |
| `now`                         | Current Unix time in seconds (int64)                   |
| `nowNano`                     | Current Unix time in nanoseconds (int64)               |

Templates are compiled once when the profile is loaded; if a field has no `{{` it's used as a plain string with zero per-request overhead. Bad templates fail loudly at load time, not at request time.

`Type` values:

- `"REST"` → `Content-Type: application/json; charset=utf-8`
- `"WWW"`  → `Content-Type: application/x-www-form-urlencoded`
- any other non-empty value is used as the Content-Type directly (e.g. `"application/xml"`)
- omitted / empty → no Content-Type header is set

## Live monitoring

While Hammer is running, hit the stats endpoint for a plain-text snapshot:

```shell
curl http://localhost:9001/stats
```

Disable the endpoint with `-stats-addr ""` if port 9001 conflicts with your test target.

## JSON report output

Pass `-json-out report.json` to write a structured report on exit, suitable for CI assertions or baseline comparison across runs:

```shell
./hammer -profile profiles/httpbin.json -rps 100 -duration 1m -json-out report.json
```

Shape:

```json
{
  "start_time": "2026-05-16T10:00:00Z",
  "end_time":   "2026-05-16T10:01:00Z",
  "duration_sec": 60.0,
  "target_rps": 100,
  "profile": "profiles/httpbin.json",
  "sent": 6000, "received": 5994, "errors": 6, "canceled": 0, "slow": 0,
  "slow_threshold_sec": 1,
  "status_codes":   [{"code": 200, "count": 5994}, {"code": 500, "count": 6}],
  "network_errors": [],
  "latency_ms": {
    "samples": 5994,
    "min":  140.12, "mean": 182.04,
    "p50":  180.34, "p90":  220.55, "p95": 240.07, "p99": 310.82,
    "max":  410.43
  },
  "per_call": [
    {"method": "GET",  "url": "https://httpbin.org/get",  "count": 4002, "avg_rt_sec": 0.183},
    {"method": "POST", "url": "https://httpbin.org/post", "count": 1992, "avg_rt_sec": 0.181}
  ]
}
```

In CI you can pipe this through `jq` to fail a build on regressions:

```shell
./hammer -profile p.json -rps 200 -duration 30s -json-out r.json
jq -e '.latency_ms.p99 < 500 and .errors == 0' r.json
```

## Behavior notes

- **Success vs. error**: 2xx and 3xx responses are successes. Anything else (including 4xx, 5xx, and network failures) is an error. Use `-ok "404,409"` to whitelist additional status codes as success.
- **Network-error categories** reported in the summary:
  - `timeout` – the per-request `-timeout` exceeded
  - `canceled` – request was in-flight when the run ended (Ctrl+C or `-duration`); not counted in `Errors`
  - `conn` – TCP dial / read / write failure (e.g. connection refused, reset)
  - `dns` – name resolution failure
  - `tls` – TLS handshake / certificate validation failure
  - `other` – anything else (e.g. malformed URL)
- **`Pending` in the live log** = sent − received − errors − canceled. Steady non-zero values mean the target can't keep up with the requested RPS.
- **`-timeout`** caps each individual request. When the target slows down, prefer a low timeout to keep goroutine count bounded.
- **`-rps`** is enforced by a fixed-interval ticker (`1s / rps`). Effective RPS is bounded by how fast the target can respond and how many file descriptors are available.

## Local test target

A minimal HTTP server for local benchmarking lives in `cmd/testserver`:

```shell
go run ./cmd/testserver       # listens on :9000
```

Endpoints: `/hello`, `/hello_in_json`.

## Development

```shell
go test ./...           # run all tests
go test -race ./...     # race detector
go test -cover ./...    # coverage report
go vet ./...
```

## Layout

```
hammer.go              # CLI + load generator
profile/               # traffic-profile parser
profiles/              # example profile files
cmd/testserver/        # tiny local HTTP target for development
```

## License

Apache 2.0 — see [LICENSE](LICENSE).

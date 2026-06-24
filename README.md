<p align="center">
  <img src="assets/hammer-logo.svg" alt="hammer — HTTP load generator" height="88">
</p>

[![ci](https://github.com/chenchaoyi/hammer/actions/workflows/ci.yml/badge.svg)](https://github.com/chenchaoyi/hammer/actions/workflows/ci.yml)
[![release](https://img.shields.io/github/v/release/chenchaoyi/hammer?logo=github&label=release)](https://github.com/chenchaoyi/hammer/releases/latest)
[![license](https://img.shields.io/github/license/chenchaoyi/hammer)](LICENSE)

Lightweight, single-binary HTTP(S) load generator written in Go — built to be
driven by humans **and AI agents** alike.

**Why it's agent-friendly**

- **Zero-config one-shot mode**: `hammer -url https://api/health -rps 50 -duration 10s` — no profile file to author first
- **Machine-readable output**: `-output json` writes the full report to **stdout** (logs stay on stderr), so it pipes straight into `jq`
- **SLO assertions + meaningful exit codes**: `-max-error-rate`, `-max-p95`, `-max-p99` turn a load test into a pass/fail check an agent can branch on (exit `1` = SLO violated)
- **Reads a profile from stdin**: `… | hammer -profile -` — generate traffic mixes on the fly without temp files
- **Quiet by default for automation**: `-quiet` silences the live monitor; the live HTTP `/stats` port is **opt-in** (no surprise port binds in CI/sandboxes)
- **Self-describing**: `hammer -h` documents every flag, the exit codes, and copy-pasteable examples

**Core engine**

- **Constant-rate**: drives a configurable RPS using a tick loop
- **Weighted traffic mix**: define multiple endpoints with relative weights in a JSON file
- **Per-call headers**: set `Authorization`, API keys, tracing headers, … per call
- **URL / body templating**: `{{ uuid }}`, `{{ randInt 1000000 }}`, `{{ pickOne "us" "eu" }}` to avoid cache-hit skew
- **Live monitor**: per-second `SendPS / ReceivePS / AvgRT / Pending / Err% / Slow%` log line
- **Final report**: latency percentiles + per–status-code histogram + network-error categories
- **Structured JSON report**: to stdout (`-output json`) or a file (`-json-out`) for CI / baseline diffing
- **TTY-aware color**: ANSI color in interactive terminals; byte-stable plain text for pipes, redirects, JSON, CI, and tests
- **Optional HTTP stats endpoint**: `GET /stats` while running (`-stats-addr`)
- **Graceful shutdown**: SIGINT/SIGTERM or `-duration`
- **Per-request timeout**: stops slow servers from piling up goroutines

## Install

### Install script (recommended)

Install the latest GitHub release as a `hammer` command:

```shell
curl -fsSL https://github.com/chenchaoyi/hammer/releases/latest/download/install.sh | sh
hammer -version
```

The installer detects macOS/Linux and `amd64`/`arm64`, verifies the downloaded
archive against `SHA256SUMS`, and installs to `/usr/local/bin` when writable
or `$HOME/.local/bin` otherwise. To choose a directory:

```shell
curl -fsSL https://github.com/chenchaoyi/hammer/releases/latest/download/install.sh | HAMMER_INSTALL_DIR="$HOME/bin" sh
```

If GitHub release downloads are unstable from your network, force the mirror
ladder and skip the direct GitHub attempt:

```shell
curl -fsSL https://github.com/chenchaoyi/hammer/releases/latest/download/install.sh | HAMMER_INSTALL_MIRROR=ghproxy sh
```

`HAMMER_INSTALL_MIRROR` accepts `auto` (default: GitHub first, then mirrors),
`github` (canonical GitHub only), `ghproxy` (mirror chain only), or a custom
`https://proxy.example/` prefix that serves `<proxy><github-url>`.
Each download attempt has a bounded timeout, and the installer rejects invalid
mirror responses by checking that archives are gzip streams and `SHA256SUMS`
contains the expected 64-hex entry before installing anything.

If downloading `install.sh` itself is blocked, bootstrap through the same mirror
style first:

```shell
curl -fsSL https://ghfast.top/https://github.com/chenchaoyi/hammer/releases/latest/download/install.sh | HAMMER_INSTALL_MIRROR=ghproxy sh
```

### Manual archive download

Grab the archive for your platform from the [latest GitHub release](https://github.com/chenchaoyi/hammer/releases/latest):

```shell
# macOS (Apple Silicon)
curl -L https://github.com/chenchaoyi/hammer/releases/latest/download/hammer-darwin-arm64.tar.gz | tar xz
./hammer -version

# Linux x86_64
curl -L https://github.com/chenchaoyi/hammer/releases/latest/download/hammer-linux-amd64.tar.gz | tar xz
./hammer -version
```

Binaries are statically linked (no libc dependency), ~7-8 MB, available for:

- `linux/amd64`, `linux/arm64`
- `darwin/amd64`, `darwin/arm64`
- `windows/amd64`

Each release also includes a `SHA256SUMS` file:

```shell
curl -LO https://github.com/chenchaoyi/hammer/releases/latest/download/SHA256SUMS
sha256sum -c SHA256SUMS --ignore-missing
```

### From source

Requires Go 1.24+.

```shell
git clone https://github.com/chenchaoyi/hammer
cd hammer
go build -o hammer .
```

Cross-compile, e.g. for Linux:

```shell
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o hammer.linux .
```

### Updating

Once installed, hammer can update itself in place:

```shell
hammer update            # download the latest release and replace this binary
hammer update -check     # only report whether a newer version is available
hammer update -version v1.2.0   # install a specific release
hammer update -y         # skip the confirmation prompt (for scripts/CI)
```

`update` resolves the target release from the GitHub API, downloads the archive
for your platform, verifies it against `SHA256SUMS`, and atomically swaps the
running binary. It honors the same mirror controls as the installer — pass
`-mirror ghproxy` (or set `HAMMER_INSTALL_MIRROR`) when GitHub is unreachable,
and `HAMMER_REPO` to update from a fork. If the binary lives in a directory you
can't write to (e.g. `/usr/local/bin`), re-run with `sudo`.

### Self-test

Verify a freshly installed binary works end-to-end with no external target:

```shell
hammer selftest          # human-readable PASS/FAIL list
hammer selftest -json    # machine-readable results for CI/agents
```

`selftest` spins up an in-process HTTP server and drives the **real** load
engine against it, asserting request accounting, status-code classification,
the `-ok` override, SLO threshold evaluation, header/Content-Type propagation,
and profile parsing all behave. It exits `0` when every check passes and `1`
otherwise — a quick smoke test for an agent to confirm the tool is healthy
before relying on it.

## Quick start

Zero-config — hammer a single URL for 10 seconds at 50 rps (no profile file needed):

```shell
./hammer -url https://httpbin.org/get -rps 50 -duration 10s
```

Or drive a weighted traffic mix from a profile file:

```shell
./hammer -profile profiles/httpbin.json -rps 50 -duration 10s
```

Expected output:

```
2026/05/16 11:00:00 Hammering @ 50 rps for 10s (timeout=30s, target=profiles/httpbin.json)
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

Latency (ms):  min=140.12  mean=182.04  p50=180.34  p90=220.55  p95=240.07  p99=310.82  max=410.43

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

## For agents

Hammer is designed so an autonomous agent can run a load test and act on the
result with **no files and no scraping of human text**. The recommended recipe:

```shell
hammer -url https://api.example.com/health \
       -rps 100 -duration 30s \
       -output json -quiet \
       -max-error-rate 0.01 -max-p99 500ms
```

- `-output json` prints the entire report to **stdout** as one JSON object; all
  progress/diagnostic logging goes to **stderr**, so stdout is always clean to parse.
- `-quiet` removes the per-second monitor noise.
- `-max-*` flags assert SLOs. If any is violated the process exits non-zero, so
  the agent can branch on the exit code alone — or read `.checks` in the JSON:

```shell
hammer -url https://api.example.com/ -duration 30s -output json -quiet \
       -max-p99 500ms -max-error-rate 0.01 | jq '.checks'
```

```json
{
  "passed": false,
  "results": [
    { "name": "error_rate", "limit": 0.01, "actual": 0.0, "unit": "", "ok": true },
    { "name": "p99_ms", "limit": 500, "actual": 612.3, "unit": "ms", "ok": false }
  ]
}
```

**Exit codes** (stable; safe to branch on):

| Code | Meaning                                                        |
|------|---------------------------------------------------------------|
| `0`  | Run completed; all configured SLO checks passed (or none set) |
| `1`  | Run completed but one or more SLO thresholds were violated    |
| `2`  | Usage / argument error (bad flags, missing target, …)         |
| `3`  | Setup error (couldn't load the profile, build the client, …)  |

Without any `-max-*` flag, hammer never exits non-zero just because the target
returned errors — it only fails on usage/setup problems. Opt into pass/fail with
the SLO flags.

## Flags

### Target (choose one)

| Flag           | Default | Description                                                              |
|----------------|---------|--------------------------------------------------------------------------|
| `-url`         | `""`    | Single target URL (zero-config mode). Templating works in the URL.        |
| `-profile`     | `""`    | Path to a traffic profile JSON file, or `-` to read the profile from stdin |

`-url` mode also accepts these (ignored in `-profile` mode):

| Flag             | Default | Description                                                            |
|------------------|---------|------------------------------------------------------------------------|
| `-method`        | `GET`   | HTTP method for `-url` mode                                            |
| `-body`          | `""`    | Request body for `-url` mode (templating supported)                   |
| `-content-type`  | `""`    | Content-Type for `-url` mode: `REST`, `WWW`, or a raw value           |
| `-header`        | _(none)_| Header as `'Key: Value'`; repeatable                                  |

### Load & reporting

| Flag           | Default | Description                                                              |
|----------------|---------|--------------------------------------------------------------------------|
| `-rps`         | `100`   | Target requests per second                                               |
| `-duration`    | `0`     | Total run time (e.g. `30s`, `5m`); `0` runs until `Ctrl+C`               |
| `-timeout`     | `30s`   | Per-request HTTP timeout                                                 |
| `-slow`        | `1s`    | Log + count responses slower than this threshold                         |
| `-output`      | `text`  | Final report format: `text` (human) or `json` (machine-readable, to stdout) |
| `-quiet`       | `false` | Suppress the per-second progress monitor and info logs                   |
| `-json-out`    | `""`    | Also write the structured JSON report to this file path                  |
| `-ok`          | `""`    | Extra status codes treated as success (e.g. `-ok "404,409"`). 2xx and 3xx are always OK. |
| `-stats-addr`  | `""`    | Address for the live `/stats` HTTP endpoint; empty (default) disables it  |
| `-proxy`       | `""`    | HTTP proxy URL, e.g. `http://127.0.0.1:8888`                             |
| `-insecure`    | `false` | Skip TLS certificate verification                                        |
| `-debug`       | `false` | Verbose request/response logging (one log line per request)              |
| `-version`     | `false` | Print version (set via `-ldflags="-X main.version=..."`) and exit           |

### SLO thresholds (drive the exit code)

| Flag              | Default | Description                                                          |
|-------------------|---------|----------------------------------------------------------------------|
| `-max-error-rate` | `-1`    | Fail (exit `1`) if the error rate exceeds this fraction `[0,1]`; `-1` disables. `0` means "no errors allowed". |
| `-max-p50`        | `0`     | Fail if p50 latency exceeds this duration (e.g. `100ms`); `0` disables |
| `-max-p95`        | `0`     | Fail if p95 latency exceeds this duration; `0` disables               |
| `-max-p99`        | `0`     | Fail if p99 latency exceeds this duration; `0` disables               |

Run `./hammer -h` for the live list.

> **Note:** the live `/stats` endpoint is now **off by default** (was `:9001`).
> Enable it with `-stats-addr :9001` when you want it. This keeps automated runs
> from binding a port they didn't ask for.

## Profile format

For anything more than a single endpoint, use a profile: a list of weighted
`Call` objects. Pass it with `-profile FILE`, or stream it on stdin with
`-profile -`. JSON field names are case-insensitive, so `"weight"` and
`"Weight"` are equivalent.

The natural form is a **JSON array**:

```json
[
  {
    "Weight": 40,
    "Method": "GET",
    "URL": "https://httpbin.org/get",
    "Headers": {
      "Authorization": "Bearer your-token",
      "X-Trace-Id": "hammer-load-test"
    }
  },
  {
    "Weight": 20,
    "Method": "POST",
    "URL": "https://httpbin.org/post",
    "Body": "{\"test\":\"hammer\"}",
    "Type": "REST"
  }
]
```

A bare **stream of objects** (concatenated, no enclosing array — whitespace
between them optional) is also accepted, which is handy for appending calls
to a file or generating them line by line:

```json
{"Weight": 40, "Method": "GET",  "URL": "https://httpbin.org/get"}
{"Weight": 20, "Method": "POST", "URL": "https://httpbin.org/post", "Body": "{\"test\":\"hammer\"}", "Type": "REST"}
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

The live `/stats` endpoint is **opt-in**. Start it by passing an address:

```shell
./hammer -url https://httpbin.org/get -duration 1m -stats-addr :9001 &
curl http://localhost:9001/stats
```

It stays off by default so automated/CI runs never bind a port they didn't ask for.

## JSON report output

Two independent ways to get the structured report:

- `-output json` writes it to **stdout** (ideal for agents/pipelines; logs go to stderr)
- `-json-out report.json` writes it to a file (handy for archiving a CI artifact)

```shell
# stdout, straight into jq
./hammer -url https://httpbin.org/get -rps 100 -duration 1m -output json -quiet | jq .

# or to a file
./hammer -profile profiles/httpbin.json -rps 100 -duration 1m -json-out report.json
```

Shape:

```json
{
  "start_time": "2026-05-16T10:00:00Z",
  "end_time":   "2026-05-16T10:01:00Z",
  "duration_sec": 60.0,
  "target_rps": 100,
  "achieved_rps": 99.9,
  "profile": "https://httpbin.org/get",
  "sent": 6000, "received": 5994, "errors": 6, "canceled": 0, "slow": 0,
  "error_rate": 0.001,
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
  ],
  "checks": {
    "passed": true,
    "results": [
      {"name": "error_rate", "limit": 0.01, "actual": 0.001, "unit": "", "ok": true},
      {"name": "p99_ms", "limit": 500, "actual": 310.82, "unit": "ms", "ok": true}
    ]
  }
}
```

`achieved_rps`, `error_rate`, and `checks` are computed for you so consumers don't
have to. `checks` is present only when at least one `-max-*` flag is set.

The cleanest way to gate CI/an agent on a regression is to let hammer do the
asserting and check the exit code — no `jq` math required:

```shell
./hammer -url https://api.example.com/ -rps 200 -duration 30s \
         -max-p99 500ms -max-error-rate 0 -quiet || echo "SLO breach!"
```

Prefer to assert yourself? Pipe the JSON through `jq`:

```shell
./hammer -url https://api.example.com/ -rps 200 -duration 30s -output json -quiet \
  | jq -e '.latency_ms.p99 < 500 and .errors == 0'
```

## Behavior notes

- **Success vs. error**: 2xx and 3xx responses are successes. Anything else (including 4xx, 5xx, and network failures) is an error. Use `-ok "404,409"` to whitelist additional status codes as success.
- **Terminal color**: live progress and text reports use ANSI color only when the relevant stream is an interactive TTY. `NO_COLOR`, `TERM=dumb`, pipes, redirects, `-output json`, and captured test output stay plain.
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

## Cutting a release

The `.github/workflows/release.yml` workflow builds cross-platform binaries and uploads them to a GitHub release whenever a `v*` tag is pushed:

```shell
git tag -a v1.0.0 -m "v1.0.0"
git push origin v1.0.0
```

The workflow runs the test suite first, then builds for `linux/{amd64,arm64}`,
`darwin/{amd64,arm64}`, and `windows/amd64`. Unix archives contain a binary
named `hammer`; the Windows zip contains `hammer.exe`. The release also includes
`install.sh` and `SHA256SUMS`, so the one-line curl installer works from the
GitHub release page without reading files from the branch tip.

## Layout

```
hammer.go              # CLI + load generator
color.go               # TTY-gated ANSI color helpers
install.sh             # GitHub release installer with mirror fallback
profile/               # traffic-profile parser
profiles/              # example profile files
assets/                # README logo and brand assets
cmd/testserver/        # tiny local HTTP target for development
```

## License

Apache 2.0 — see [LICENSE](LICENSE).

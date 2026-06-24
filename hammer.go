package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/chenchaoyi/hammer/profile"
)

// netErrCategory names every kind of transport-layer failure we recognize.
type netErrCategory int

const (
	netErrNone netErrCategory = iota
	netErrCanceled
	netErrTimeout
	netErrDNS
	netErrConn
	netErrTLS
	netErrOther
)

var netErrCategoryNames = [...]string{
	netErrNone:     "none",
	netErrCanceled: "canceled",
	netErrTimeout:  "timeout",
	netErrDNS:      "dns",
	netErrConn:     "conn",
	netErrTLS:      "tls",
	netErrOther:    "other",
}

func classifyErr(err error) netErrCategory {
	if err == nil {
		return netErrNone
	}
	if errors.Is(err, context.Canceled) {
		return netErrCanceled
	}
	var ue *url.Error
	if errors.As(err, &ue) && ue.Timeout() {
		return netErrTimeout
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return netErrTimeout
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return netErrDNS
	}
	msg := err.Error()
	if strings.Contains(msg, "x509") || strings.Contains(msg, "tls:") {
		return netErrTLS
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return netErrConn
	}
	return netErrOther
}

type Counter struct {
	sentCount     atomic.Int64
	recvCount     atomic.Int64 // 2xx/3xx or extra-OK responses
	errCount      atomic.Int64 // network errors + non-OK status codes
	canceledCount atomic.Int64 // requests canceled (usually shutdown); not in errCount
	slowCount     atomic.Int64
	totalTimeNs   atomic.Int64

	lastSent int64
	lastRecv int64

	slowThreshold time.Duration
	extraOKCodes  map[int]bool

	// Per–HTTP-status-code histogram (indices 100-599).
	statusCounts [600]atomic.Int64
	// Per-category network-error histogram.
	netErrCounts [len(netErrCategoryNames)]atomic.Int64

	latMu sync.Mutex
	latMs []float64 // milliseconds, successful responses only

	client  *http.Client
	profile *profile.Profile
	debug   bool
	quiet   bool // suppress per-response "Slow response" lines
}

func newCounter(p *profile.Profile, timeout, slow time.Duration, proxyURL string, insecureTLS, debug bool, extraOK map[int]bool) (*Counter, error) {
	tr := &http.Transport{
		DisableKeepAlives:   false,
		MaxIdleConns:        4096,
		MaxIdleConnsPerHost: 1024,
		IdleConnTimeout:     90 * time.Second,
		TLSClientConfig:     &tls.Config{InsecureSkipVerify: insecureTLS},
	}
	if proxyURL != "" {
		u, err := url.Parse(proxyURL)
		if err != nil {
			return nil, fmt.Errorf("parse proxy: %w", err)
		}
		tr.Proxy = http.ProxyURL(u)
	}
	return &Counter{
		client:        &http.Client{Transport: tr, Timeout: timeout},
		profile:       p,
		slowThreshold: slow,
		extraOKCodes:  extraOK,
		latMs:         make([]float64, 0, 4096),
		debug:         debug,
	}, nil
}

func (c *Counter) isOK(statusCode int) bool {
	if statusCode >= 200 && statusCode < 400 {
		return true
	}
	return c.extraOKCodes[statusCode]
}

func (c *Counter) hammer(ctx context.Context) {
	c.sentCount.Add(1)

	call := c.profile.NextCall()
	method, ctype := call.Method, call.Type

	u, body, err := call.Render()
	if err != nil {
		c.errCount.Add(1)
		c.netErrCounts[netErrOther].Add(1)
		if c.debug {
			log.Printf("render template for %s %s: %v", method, call.URL, err)
		}
		return
	}

	var reqBody io.Reader
	if body != "" {
		reqBody = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, reqBody)
	if err != nil {
		c.errCount.Add(1)
		c.netErrCounts[netErrOther].Add(1)
		if c.debug {
			log.Printf("build request: %v", err)
		}
		return
	}

	if method == "PATCH" || method == "PUT" || method == "POST" {
		switch ctype {
		case "REST":
			req.Header.Set("Content-Type", "application/json; charset=utf-8")
		case "WWW":
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		case "":
			// no default
		default:
			req.Header.Set("Content-Type", ctype)
		}
	}
	// Per-call headers override the auto-inferred Content-Type.
	for k, v := range call.Headers {
		req.Header.Set(k, v)
	}

	t0 := time.Now()
	res, err := c.client.Do(req)
	elapsed := time.Since(t0)

	if err != nil {
		cat := classifyErr(err)
		c.netErrCounts[cat].Add(1)
		if cat == netErrCanceled {
			c.canceledCount.Add(1)
		} else {
			c.errCount.Add(1)
		}
		if c.debug {
			log.Printf("response time %s %s err on %s %s: %v",
				elapsed, netErrCategoryNames[cat], method, u, err)
		}
		return
	}

	// Drain & close body so keep-alive can reuse the connection.
	if c.debug {
		data, _ := io.ReadAll(res.Body)
		log.Printf("Req: %s %s\nReq Body: %s\nRes: %s\nRes Body: %s",
			method, u, body, res.Status, string(data))
	} else {
		_, _ = io.Copy(io.Discard, res.Body)
	}
	res.Body.Close()

	if sc := res.StatusCode; sc >= 100 && sc < len(c.statusCounts) {
		c.statusCounts[sc].Add(1)
	}

	if !c.isOK(res.StatusCode) {
		if c.debug {
			log.Printf("status %s for %s %s", res.Status, method, u)
		}
		c.errCount.Add(1)
		return
	}

	c.recvCount.Add(1)
	c.totalTimeNs.Add(elapsed.Nanoseconds())
	if elapsed > c.slowThreshold {
		c.slowCount.Add(1)
		if !c.quiet {
			log.Printf("Slow response %s -> %s %s", elapsed, method, u)
		}
	}

	call.Record(elapsed.Nanoseconds())

	ms := float64(elapsed) / float64(time.Millisecond)
	c.latMu.Lock()
	c.latMs = append(c.latMs, ms)
	c.latMu.Unlock()
}

func (c *Counter) tick() {
	sent := c.sentCount.Load()
	recv := c.recvCount.Load()
	errs := c.errCount.Load()
	slow := c.slowCount.Load()
	total := c.totalTimeNs.Load()

	sps := sent - c.lastSent
	pps := recv - c.lastRecv
	backlog := sent - recv - errs - c.canceledCount.Load()
	c.lastSent = sent
	c.lastRecv = recv

	avg := 0.0
	if recv > 0 {
		avg = float64(total) / float64(recv) / 1e9
	}
	denom := errs + recv
	if denom == 0 {
		denom = 1
	}

	errPct := float64(errs) * 100 / float64(denom)
	errStr := fmt.Sprintf("%d|%.2f%%", errs, errPct)
	errCol := cDim
	if errs > 0 {
		errCol = cRed
	}
	slowPct := float64(slow) * 100 / float64(denom)
	slowStr := fmt.Sprintf("%.2f%%", slowPct)
	slowCol := cDim
	if slowPct > 0 {
		slowCol = cYellow
	}

	log.Printf(" %s %s  %s %s  %s %s  %s %s  %s %s  %s %s",
		pe("SendPS:", cDim), pe(fmt.Sprintf("%4d", sps), cFg, cBold),
		pe("ReceivePS:", cDim), pe(fmt.Sprintf("%4d", pps), cFg, cBold),
		pe("AvgRT:", cDim), pe(fmt.Sprintf("%.4fs", avg), cCyan, cBold),
		pe("Pending:", cDim), pe(fmt.Sprintf("%d", backlog), cFg),
		pe("Err:", cDim), pe(errStr, errCol),
		pe("Slow:", cDim), pe(slowStr, slowCol))
}

// percentile returns the q-th percentile (q in [0, 1]) from an already-sorted
// slice using the nearest-rank method. Returns 0 if the slice is empty.
func percentile(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	if q < 0 {
		q = 0
	}
	if q > 1 {
		q = 1
	}
	idx := int(q * float64(len(sorted)-1))
	return sorted[idx]
}

func (c *Counter) statusBreakdown() string {
	return c.statusBreakdownWith(po)
}

func (c *Counter) statusBreakdownPlain() string {
	return c.statusBreakdownWith(noColor)
}

type colorFunc func(string, ...string) string

func noColor(s string, _ ...string) string { return s }

func statusColor(sc int) string {
	switch {
	case sc >= 200 && sc < 300:
		return cGreen
	case sc >= 300 && sc < 400:
		return cCyan
	case sc >= 400 && sc < 500:
		return cYellow
	case sc >= 500:
		return cRed
	default:
		return cDim
	}
}

func (c *Counter) statusBreakdownWith(colorize colorFunc) string {
	var b strings.Builder
	any := false
	for sc := 100; sc < len(c.statusCounts); sc++ {
		if n := c.statusCounts[sc].Load(); n > 0 {
			fmt.Fprintf(&b, "  %s: %s\n",
				colorize(strconv.Itoa(sc), statusColor(sc)),
				colorize(strconv.FormatInt(n, 10), cFg))
			any = true
		}
	}
	if !any {
		fmt.Fprintf(&b, "  %s\n", colorize("(none)", cDim))
	}
	return b.String()
}

func (c *Counter) netErrBreakdown() string {
	return c.netErrBreakdownWith(po)
}

func (c *Counter) netErrBreakdownPlain() string {
	return c.netErrBreakdownWith(noColor)
}

func (c *Counter) netErrBreakdownWith(colorize colorFunc) string {
	var b strings.Builder
	any := false
	for cat := netErrCanceled; cat < netErrCategory(len(c.netErrCounts)); cat++ {
		if n := c.netErrCounts[cat].Load(); n > 0 {
			fmt.Fprintf(&b, "  %s\n",
				colorize(fmt.Sprintf("%s: %d", netErrCategoryNames[cat], n), cRed))
			any = true
		}
	}
	if !any {
		return ""
	}
	return b.String()
}

// report writes the human-readable summary to w. checks may be nil when no
// SLO thresholds were configured.
func (c *Counter) report(w io.Writer, checks *ChecksReport) {
	c.latMu.Lock()
	samples := append([]float64(nil), c.latMs...)
	c.latMu.Unlock()
	sort.Float64s(samples)

	fmt.Fprintln(w)
	fmt.Fprintln(w, po("=== Summary ===", cOrange, cBold))
	fmt.Fprintf(w, "%s     %s\n", po("Sent:", cDim), po(strconv.FormatInt(c.sentCount.Load(), 10), cFg))
	fmt.Fprintf(w, "%s %s\n", po("Received:", cDim), po(strconv.FormatInt(c.recvCount.Load(), 10), cFg))
	errs := c.errCount.Load()
	errColor := cGreen
	if errs > 0 {
		errColor = cRed
	}
	fmt.Fprintf(w, "%s   %s\n", po("Errors:", cDim), po(strconv.FormatInt(errs, 10), errColor))
	if canceled := c.canceledCount.Load(); canceled > 0 {
		fmt.Fprintf(w, "%s %s  (in-flight at shutdown)\n",
			po("Canceled:", cDim), po(strconv.FormatInt(canceled, 10), cFg))
	}
	slow := c.slowCount.Load()
	slowColor := cGreen
	if slow > 0 {
		slowColor = cYellow
	}
	fmt.Fprintf(w, "%s     %s  (> %s)\n",
		po("Slow:", cDim), po(strconv.FormatInt(slow, 10), slowColor), c.slowThreshold)

	fmt.Fprintln(w)
	fmt.Fprintln(w, po("Status codes:", cYelHi, cBold))
	fmt.Fprint(w, c.statusBreakdown())

	if netErrs := c.netErrBreakdown(); netErrs != "" {
		fmt.Fprintln(w)
		fmt.Fprintln(w, po("Network errors:", cYelHi, cBold))
		fmt.Fprint(w, netErrs)
	}

	if len(samples) == 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "No successful samples; latency stats unavailable.")
		printChecks(w, checks)
		return
	}
	var sum float64
	for _, v := range samples {
		sum += v
	}
	mean := sum / float64(len(samples))
	fmt.Fprintln(w)
	fmt.Fprintf(w, "%s  min=%s  mean=%s  p50=%s  p90=%s  p95=%s  p99=%s  max=%s\n",
		po("Latency (ms):", cCyan),
		po(fmt.Sprintf("%.2f", samples[0]), cFg, cBold),
		po(fmt.Sprintf("%.2f", mean), cFg, cBold),
		po(fmt.Sprintf("%.2f", percentile(samples, 0.50)), cFg, cBold),
		po(fmt.Sprintf("%.2f", percentile(samples, 0.90)), cFg, cBold),
		po(fmt.Sprintf("%.2f", percentile(samples, 0.95)), cFg, cBold),
		po(fmt.Sprintf("%.2f", percentile(samples, 0.99)), cFg, cBold),
		po(fmt.Sprintf("%.2f", samples[len(samples)-1]), cFg, cBold))

	fmt.Fprintln(w)
	fmt.Fprintln(w, po("--- Per call ---", cDim))
	fmt.Fprint(w, c.profile.Print())

	printChecks(w, checks)
}

// printChecks renders the SLO threshold results, if any were configured.
func printChecks(w io.Writer, checks *ChecksReport) {
	if checks == nil || len(checks.Results) == 0 {
		return
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, po("=== Checks ===", cOrange, cBold))
	for _, r := range checks.Results {
		status := "PASS"
		statusColor := cGreen
		cmp := "<="
		if !r.OK {
			status = "FAIL"
			statusColor = cRed
			cmp = ">"
		}
		fmt.Fprintf(w, "  %-12s %.4g %s %.4g %s  %s\n",
			r.Name, r.Actual, cmp, r.Limit, r.Unit, po(status, statusColor))
	}
	verdict := "PASS"
	verdictColor := cGreen
	if !checks.Passed {
		verdict = "FAIL"
		verdictColor = cRed
	}
	fmt.Fprintf(w, "RESULT: %s\n", po(verdict, verdictColor, cBold))
}

// --- JSON report --------------------------------------------------------

type LatencyReport struct {
	Samples int     `json:"samples"`
	Min     float64 `json:"min"`
	Mean    float64 `json:"mean"`
	P50     float64 `json:"p50"`
	P90     float64 `json:"p90"`
	P95     float64 `json:"p95"`
	P99     float64 `json:"p99"`
	Max     float64 `json:"max"`
}

type StatusBucket struct {
	Code  int   `json:"code"`
	Count int64 `json:"count"`
}

type NetErrBucket struct {
	Category string `json:"category"`
	Count    int64  `json:"count"`
}

type CallReport struct {
	Method   string  `json:"method"`
	URL      string  `json:"url"`
	Count    int64   `json:"count"`
	AvgRTSec float64 `json:"avg_rt_sec"`
}

// CheckResult is the outcome of evaluating one SLO threshold.
type CheckResult struct {
	Name   string  `json:"name"`
	Limit  float64 `json:"limit"`
	Actual float64 `json:"actual"`
	Unit   string  `json:"unit"`
	OK     bool    `json:"ok"`
}

// ChecksReport aggregates every configured SLO threshold and the overall
// pass/fail verdict that drives hammer's exit code.
type ChecksReport struct {
	Passed  bool          `json:"passed"`
	Results []CheckResult `json:"results"`
}

type JSONReport struct {
	StartTime        time.Time      `json:"start_time"`
	EndTime          time.Time      `json:"end_time"`
	DurationSec      float64        `json:"duration_sec"`
	TargetRPS        int            `json:"target_rps"`
	AchievedRPS      float64        `json:"achieved_rps"`
	Profile          string         `json:"profile"`
	Sent             int64          `json:"sent"`
	Received         int64          `json:"received"`
	Errors           int64          `json:"errors"`
	Canceled         int64          `json:"canceled"`
	Slow             int64          `json:"slow"`
	ErrorRate        float64        `json:"error_rate"`
	SlowThresholdSec float64        `json:"slow_threshold_sec"`
	StatusCodes      []StatusBucket `json:"status_codes"`
	NetworkErrors    []NetErrBucket `json:"network_errors"`
	LatencyMs        LatencyReport  `json:"latency_ms"`
	PerCall          []CallReport   `json:"per_call"`
	Checks           *ChecksReport  `json:"checks,omitempty"`
}

func (c *Counter) buildJSONReport(start, end time.Time, rps int, profilePath string) *JSONReport {
	c.latMu.Lock()
	samples := append([]float64(nil), c.latMs...)
	c.latMu.Unlock()
	sort.Float64s(samples)

	var lat LatencyReport
	if n := len(samples); n > 0 {
		var sum float64
		for _, v := range samples {
			sum += v
		}
		lat = LatencyReport{
			Samples: n,
			Min:     samples[0],
			Mean:    sum / float64(n),
			P50:     percentile(samples, 0.50),
			P90:     percentile(samples, 0.90),
			P95:     percentile(samples, 0.95),
			P99:     percentile(samples, 0.99),
			Max:     samples[n-1],
		}
	}

	statuses := make([]StatusBucket, 0)
	for sc := 100; sc < len(c.statusCounts); sc++ {
		if n := c.statusCounts[sc].Load(); n > 0 {
			statuses = append(statuses, StatusBucket{Code: sc, Count: n})
		}
	}

	netErrs := make([]NetErrBucket, 0)
	for cat := netErrCanceled; cat < netErrCategory(len(c.netErrCounts)); cat++ {
		if n := c.netErrCounts[cat].Load(); n > 0 {
			netErrs = append(netErrs, NetErrBucket{Category: netErrCategoryNames[cat], Count: n})
		}
	}

	calls := make([]CallReport, 0, len(c.profile.Calls()))
	for _, pc := range c.profile.Calls() {
		n := pc.Count()
		var avg float64
		if n > 0 {
			avg = float64(pc.TotalTimeNs()) / float64(n) / 1e9
		}
		calls = append(calls, CallReport{
			Method:   pc.Method,
			URL:      pc.URL,
			Count:    n,
			AvgRTSec: avg,
		})
	}

	received := c.recvCount.Load()
	errs := c.errCount.Load()
	durSec := end.Sub(start).Seconds()
	achieved := 0.0
	if durSec > 0 {
		achieved = float64(received) / durSec
	}

	return &JSONReport{
		StartTime:        start,
		EndTime:          end,
		DurationSec:      durSec,
		TargetRPS:        rps,
		AchievedRPS:      achieved,
		Profile:          profilePath,
		Sent:             c.sentCount.Load(),
		Received:         received,
		Errors:           errs,
		Canceled:         c.canceledCount.Load(),
		Slow:             c.slowCount.Load(),
		ErrorRate:        errorRate(received, errs),
		SlowThresholdSec: c.slowThreshold.Seconds(),
		StatusCodes:      statuses,
		NetworkErrors:    netErrs,
		LatencyMs:        lat,
		PerCall:          calls,
	}
}

// errorRate is errors / (errors + received). It returns 0 when nothing was
// completed, so an idle run never looks like a failure.
func errorRate(received, errors int64) float64 {
	denom := received + errors
	if denom == 0 {
		return 0
	}
	return float64(errors) / float64(denom)
}

// thresholds holds the optional SLO limits an agent can assert on. A zero
// duration (or negative rate) means "not configured".
type thresholds struct {
	maxErrorRate float64       // <0 disables
	maxP50       time.Duration // 0 disables
	maxP95       time.Duration
	maxP99       time.Duration
}

func (t thresholds) configured() bool {
	return t.maxErrorRate >= 0 || t.maxP50 > 0 || t.maxP95 > 0 || t.maxP99 > 0
}

// evaluate compares a finished run against the configured thresholds and
// returns a ChecksReport, or nil when no thresholds were set.
func (t thresholds) evaluate(r *JSONReport) *ChecksReport {
	if !t.configured() {
		return nil
	}
	cr := &ChecksReport{Passed: true}
	add := func(name string, limit, actual float64, unit string) {
		ok := actual <= limit
		if !ok {
			cr.Passed = false
		}
		cr.Results = append(cr.Results, CheckResult{
			Name: name, Limit: limit, Actual: actual, Unit: unit, OK: ok,
		})
	}
	if t.maxErrorRate >= 0 {
		add("error_rate", t.maxErrorRate, r.ErrorRate, "")
	}
	if t.maxP50 > 0 {
		add("p50_ms", float64(t.maxP50)/float64(time.Millisecond), r.LatencyMs.P50, "ms")
	}
	if t.maxP95 > 0 {
		add("p95_ms", float64(t.maxP95)/float64(time.Millisecond), r.LatencyMs.P95, "ms")
	}
	if t.maxP99 > 0 {
		add("p99_ms", float64(t.maxP99)/float64(time.Millisecond), r.LatencyMs.P99, "ms")
	}
	return cr
}

func encodeJSONReport(w io.Writer, r *JSONReport) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

func writeJSONReport(path string, r *JSONReport) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return encodeJSONReport(f, r)
}

// headerFlag collects repeatable -header "Key: Value" flags into a map.
type headerFlag map[string]string

func (h headerFlag) String() string { return "" }

func (h headerFlag) Set(v string) error {
	k, val, ok := strings.Cut(v, ":")
	if !ok {
		return fmt.Errorf("header must be in 'Key: Value' form, got %q", v)
	}
	k = strings.TrimSpace(k)
	if k == "" {
		return fmt.Errorf("header has empty name: %q", v)
	}
	h[k] = strings.TrimSpace(val)
	return nil
}

// parseExtraOKCodes parses a comma-separated list of status codes
// (e.g. "404,409") into a set, validating each code is in [100, 599].
func parseExtraOKCodes(s string) (map[int]bool, error) {
	out := map[int]bool{}
	s = strings.TrimSpace(s)
	if s == "" {
		return out, nil
	}
	for _, tok := range strings.Split(s, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		n, err := strconv.Atoi(tok)
		if err != nil {
			return nil, fmt.Errorf("invalid status code %q", tok)
		}
		if n < 100 || n >= 600 {
			return nil, fmt.Errorf("status code %d out of range [100, 599]", n)
		}
		out[n] = true
	}
	return out, nil
}

// version is overridden via -ldflags="-X main.version=..." in release builds.
var version = "dev"

// Exit codes. Stable and documented so agents can branch on them.
const (
	exitOK     = 0 // run completed; all SLO checks (if any) passed
	exitChecks = 1 // run completed but one or more SLO thresholds were violated
	exitUsage  = 2 // bad flags / arguments
	exitSetup  = 3 // could not load the profile, build the client, or write output
)

func usage() {
	out := flag.CommandLine.Output()
	fmt.Fprintf(out, `hammer %s — lightweight, agent-friendly HTTP load generator

Usage:
  hammer -url URL [options]                 # zero-config: hammer a single URL
  hammer -profile FILE [options]            # weighted traffic mix from a file
  hammer -profile - [options]               # read the profile from stdin
  hammer update [options]                    # self-update to the latest release
  hammer selftest [-json]                    # smoke-test this binary (no target needed)

Agent-friendly options:
  -output json    emit the structured report to stdout (logs stay on stderr)
  -quiet          suppress the per-second progress monitor
  -max-error-rate, -max-p50, -max-p95, -max-p99   assert SLOs; a violation exits 1

Exit codes: 0 ok · 1 SLO violated · 2 usage error · 3 setup error

Examples:
  hammer -url https://api.example.com/health -rps 50 -duration 10s
  hammer -url https://api.example.com/users -method POST \
         -header 'Authorization: Bearer t' -body '{"x":1}' -content-type REST
  hammer -url https://api.example.com/ -duration 30s \
         -output json -quiet -max-error-rate 0.01 -max-p99 500ms | jq .checks
  cat profile.json | hammer -profile - -rps 200 -duration 1m -output json

Options:
`, version)
	flag.PrintDefaults()
}

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "update":
			os.Exit(runUpdate(os.Args[2:]))
		case "selftest":
			os.Exit(runSelftest(os.Args[2:]))
		}
	}
	os.Exit(run())
}

func run() int {
	var (
		rps           int
		profileFile   string
		targetURL     string
		method        string
		body          string
		contentType   string
		duration      time.Duration
		timeout       time.Duration
		slowThreshold time.Duration
		proxy         string
		insecureTLS   bool
		debug         bool
		quiet         bool
		output        string
		statsAddr     string
		extraOKList   string
		jsonOut       string
		showVersion   bool
		maxErrorRate  float64
		maxP50        time.Duration
		maxP95        time.Duration
		maxP99        time.Duration
	)
	headers := headerFlag{}

	flag.Usage = usage
	flag.IntVar(&rps, "rps", 100, "target requests per second")
	flag.StringVar(&profileFile, "profile", "", "path to a traffic profile JSON file, or \"-\" to read from stdin")
	flag.StringVar(&targetURL, "url", "", "single target URL (zero-config mode; alternative to -profile)")
	flag.StringVar(&method, "method", "GET", "HTTP method for -url mode")
	flag.StringVar(&body, "body", "", "request body for -url mode (templating supported)")
	flag.StringVar(&contentType, "content-type", "", "Content-Type for -url mode: REST, WWW, or a raw value")
	flag.Var(headers, "header", "header for -url mode as 'Key: Value' (repeatable)")
	flag.DurationVar(&duration, "duration", 0, "total duration to run; 0 means run until interrupted")
	flag.DurationVar(&timeout, "timeout", 30*time.Second, "per-request HTTP timeout")
	flag.DurationVar(&slowThreshold, "slow", time.Second, "log + count responses slower than this threshold")
	flag.StringVar(&proxy, "proxy", "", "HTTP proxy URL (e.g. http://127.0.0.1:8888)")
	flag.BoolVar(&insecureTLS, "insecure", false, "skip TLS certificate verification")
	flag.BoolVar(&debug, "debug", false, "verbose request/response logging")
	flag.BoolVar(&quiet, "quiet", false, "suppress the per-second progress monitor and info logs")
	flag.StringVar(&output, "output", "text", "final report format: text (human) or json (machine-readable, to stdout)")
	flag.StringVar(&statsAddr, "stats-addr", "", "address to serve a live /stats endpoint on (empty disables it)")
	flag.StringVar(&extraOKList, "ok", "", "extra status codes treated as success (comma-separated, e.g. \"404,409\")")
	flag.StringVar(&jsonOut, "json-out", "", "also write the structured JSON report to this file path")
	flag.BoolVar(&showVersion, "version", false, "print version and exit")
	flag.Float64Var(&maxErrorRate, "max-error-rate", -1, "fail (exit 1) if the error rate exceeds this fraction [0,1]; -1 disables")
	flag.DurationVar(&maxP50, "max-p50", 0, "fail (exit 1) if p50 latency exceeds this duration; 0 disables")
	flag.DurationVar(&maxP95, "max-p95", 0, "fail (exit 1) if p95 latency exceeds this duration; 0 disables")
	flag.DurationVar(&maxP99, "max-p99", 0, "fail (exit 1) if p99 latency exceeds this duration; 0 disables")
	flag.Parse()

	if showVersion {
		fmt.Printf("hammer %s\n", version)
		return exitOK
	}

	// --- Validate flags ------------------------------------------------
	switch output {
	case "text", "json":
	default:
		fmt.Fprintf(os.Stderr, "Error: -output must be \"text\" or \"json\", got %q\n", output)
		return exitUsage
	}
	if rps <= 0 {
		fmt.Fprintln(os.Stderr, "Error: -rps must be positive")
		return exitUsage
	}
	if maxErrorRate > 1 {
		fmt.Fprintln(os.Stderr, "Error: -max-error-rate must be in [0,1]")
		return exitUsage
	}
	if targetURL != "" && profileFile != "" {
		fmt.Fprintln(os.Stderr, "Error: pass either -url or -profile, not both")
		return exitUsage
	}
	if targetURL == "" && profileFile == "" {
		fmt.Fprintln(os.Stderr, "Error: one of -url or -profile is required")
		flag.Usage()
		return exitUsage
	}

	colorErr = colorEnabled(os.Stderr)
	colorOut = colorEnabled(os.Stdout) && output != "json"

	extraOK, err := parseExtraOKCodes(extraOKList)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: parse -ok: %v\n", err)
		return exitUsage
	}

	// --- Load the traffic profile --------------------------------------
	var (
		prof         *profile.Profile
		profileLabel string
	)
	switch {
	case targetURL != "":
		prof, err = profile.SingleCall(method, targetURL, body, contentType, headers)
		profileLabel = targetURL
	case profileFile == "-":
		prof, err = profile.LoadFromReader(os.Stdin)
		profileLabel = "stdin"
	default:
		prof, err = profile.LoadFromFile(profileFile)
		profileLabel = profileFile
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: load profile: %v\n", err)
		return exitSetup
	}

	c, err := newCounter(prof, timeout, slowThreshold, proxy, insecureTLS, debug, extraOK)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: init: %v\n", err)
		return exitSetup
	}
	c.quiet = quiet

	thr := thresholds{maxErrorRate: maxErrorRate, maxP50: maxP50, maxP95: maxP95, maxP99: maxP99}

	parentCtx, signalCancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer signalCancel()

	// Drive shutdown through a single cancel() so in-flight requests
	// always end with context.Canceled — whether the run ends via a
	// signal or via -duration. That keeps the "timeout" bucket meaning
	// "per-request HTTP timeout exceeded" only.
	ctx, runCancel := context.WithCancel(parentCtx)
	defer runCancel()
	if duration > 0 {
		t := time.AfterFunc(duration, runCancel)
		defer t.Stop()
	}

	if statsAddr != "" {
		mux := http.NewServeMux()
		mux.HandleFunc("/stats", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			fmt.Fprintf(w, "Sent: %d\nReceived: %d\nErrors: %d\nCanceled: %d\n",
				c.sentCount.Load(), c.recvCount.Load(), c.errCount.Load(), c.canceledCount.Load())
			fmt.Fprintf(w, "\nStatus codes:\n%s", c.statusBreakdownPlain())
			if netErrs := c.netErrBreakdownPlain(); netErrs != "" {
				fmt.Fprintf(w, "\nNetwork errors:\n%s", netErrs)
			}
			fmt.Fprintf(w, "\n==========\n%s", prof.Print())
		})
		srv := &http.Server{Addr: statsAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
		go func() {
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Printf("stats server: %v", err)
			}
		}()
		defer func() {
			shutdownCtx, cc := context.WithTimeout(context.Background(), 2*time.Second)
			defer cc()
			_ = srv.Shutdown(shutdownCtx)
		}()
		if !quiet {
			log.Printf("Stats endpoint: http://%s/stats", statsAddr)
		}
	}

	interval := time.Second / time.Duration(rps)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	monitor := time.NewTicker(time.Second)
	defer monitor.Stop()

	durLabel := "until interrupted"
	if duration > 0 {
		durLabel = "for " + duration.String()
	}
	if !quiet {
		log.Printf("Hammering @ %s %s (timeout=%s, target=%s)",
			pe(fmt.Sprintf("%d rps", rps), cOrange, cBold),
			durLabel,
			pe(timeout.String(), cDim),
			pe(profileLabel, cDim))
	}
	startTime := time.Now()

LOOP:
	for {
		select {
		case <-ctx.Done():
			break LOOP
		case <-monitor.C:
			if !quiet {
				c.tick()
			}
		case <-ticker.C:
			go c.hammer(ctx)
		}
	}
	endTime := time.Now()

	if !quiet {
		log.Println(pe("Stopping...", cDim))
	}

	// Build the report once; render it as text or JSON, and reuse it for
	// both the optional -json-out file and the SLO checks / exit code.
	r := c.buildJSONReport(startTime, endTime, rps, profileLabel)
	checks := thr.evaluate(r)
	r.Checks = checks

	if output == "json" {
		if err := encodeJSONReport(os.Stdout, r); err != nil {
			fmt.Fprintf(os.Stderr, "Error: encode report: %v\n", err)
			return exitSetup
		}
	} else {
		c.report(os.Stdout, checks)
	}

	if jsonOut != "" {
		if err := writeJSONReport(jsonOut, r); err != nil {
			fmt.Fprintf(os.Stderr, "Error: write json report: %v\n", err)
			return exitSetup
		}
		if !quiet {
			log.Printf("JSON report written to %s", jsonOut)
		}
	}

	if checks != nil && !checks.Passed {
		return exitChecks
	}
	return exitOK
}

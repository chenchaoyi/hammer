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
		log.Printf("Slow response %s -> %s %s", elapsed, method, u)
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

	log.Printf(" SendPS: %4d  ReceivePS: %4d  AvgRT: %.4fs  Pending: %d  Err: %d|%.2f%%  Slow: %.2f%%",
		sps, pps, avg, backlog, errs,
		float64(errs)*100/float64(denom),
		float64(slow)*100/float64(denom))
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
	var b strings.Builder
	any := false
	for sc := 100; sc < len(c.statusCounts); sc++ {
		if n := c.statusCounts[sc].Load(); n > 0 {
			fmt.Fprintf(&b, "  %d: %d\n", sc, n)
			any = true
		}
	}
	if !any {
		b.WriteString("  (none)\n")
	}
	return b.String()
}

func (c *Counter) netErrBreakdown() string {
	var b strings.Builder
	any := false
	for cat := netErrCanceled; cat < netErrCategory(len(c.netErrCounts)); cat++ {
		if n := c.netErrCounts[cat].Load(); n > 0 {
			fmt.Fprintf(&b, "  %s: %d\n", netErrCategoryNames[cat], n)
			any = true
		}
	}
	if !any {
		return ""
	}
	return b.String()
}

func (c *Counter) report() {
	c.latMu.Lock()
	samples := append([]float64(nil), c.latMs...)
	c.latMu.Unlock()
	sort.Float64s(samples)

	fmt.Println()
	fmt.Println("=== Summary ===")
	fmt.Printf("Sent:     %d\n", c.sentCount.Load())
	fmt.Printf("Received: %d\n", c.recvCount.Load())
	fmt.Printf("Errors:   %d\n", c.errCount.Load())
	if canceled := c.canceledCount.Load(); canceled > 0 {
		fmt.Printf("Canceled: %d  (in-flight at shutdown)\n", canceled)
	}
	fmt.Printf("Slow:     %d  (> %s)\n", c.slowCount.Load(), c.slowThreshold)

	fmt.Println()
	fmt.Println("Status codes:")
	fmt.Print(c.statusBreakdown())

	if netErrs := c.netErrBreakdown(); netErrs != "" {
		fmt.Println()
		fmt.Println("Network errors:")
		fmt.Print(netErrs)
	}

	if len(samples) == 0 {
		fmt.Println()
		fmt.Println("No successful samples; latency stats unavailable.")
		return
	}
	var sum float64
	for _, v := range samples {
		sum += v
	}
	mean := sum / float64(len(samples))
	fmt.Println()
	fmt.Printf("Latency (ms):  min=%.2f  mean=%.2f  p50=%.2f  p90=%.2f  p95=%.2f  p99=%.2f  max=%.2f\n",
		samples[0],
		mean,
		percentile(samples, 0.50),
		percentile(samples, 0.90),
		percentile(samples, 0.95),
		percentile(samples, 0.99),
		samples[len(samples)-1])

	fmt.Println()
	fmt.Println("--- Per call ---")
	fmt.Print(c.profile.Print())
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

type JSONReport struct {
	StartTime        time.Time      `json:"start_time"`
	EndTime          time.Time      `json:"end_time"`
	DurationSec      float64        `json:"duration_sec"`
	TargetRPS        int            `json:"target_rps"`
	Profile          string         `json:"profile"`
	Sent             int64          `json:"sent"`
	Received         int64          `json:"received"`
	Errors           int64          `json:"errors"`
	Canceled         int64          `json:"canceled"`
	Slow             int64          `json:"slow"`
	SlowThresholdSec float64        `json:"slow_threshold_sec"`
	StatusCodes      []StatusBucket `json:"status_codes"`
	NetworkErrors    []NetErrBucket `json:"network_errors"`
	LatencyMs        LatencyReport  `json:"latency_ms"`
	PerCall          []CallReport   `json:"per_call"`
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

	return &JSONReport{
		StartTime:        start,
		EndTime:          end,
		DurationSec:      end.Sub(start).Seconds(),
		TargetRPS:        rps,
		Profile:          profilePath,
		Sent:             c.sentCount.Load(),
		Received:         c.recvCount.Load(),
		Errors:           c.errCount.Load(),
		Canceled:         c.canceledCount.Load(),
		Slow:             c.slowCount.Load(),
		SlowThresholdSec: c.slowThreshold.Seconds(),
		StatusCodes:      statuses,
		NetworkErrors:    netErrs,
		LatencyMs:        lat,
		PerCall:          calls,
	}
}

func writeJSONReport(path string, r *JSONReport) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
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

func main() {
	var (
		rps           int
		profileFile   string
		duration      time.Duration
		timeout       time.Duration
		slowThreshold time.Duration
		proxy         string
		insecureTLS   bool
		debug         bool
		statsAddr     string
		extraOKList   string
		jsonOut       string
	)

	flag.IntVar(&rps, "rps", 100, "requests per second")
	flag.StringVar(&profileFile, "profile", "", "path to traffic profile JSON file (required)")
	flag.DurationVar(&duration, "duration", 0, "total duration to run; 0 means run until interrupted")
	flag.DurationVar(&timeout, "timeout", 30*time.Second, "per-request HTTP timeout")
	flag.DurationVar(&slowThreshold, "slow", time.Second, "log responses slower than this threshold")
	flag.StringVar(&proxy, "proxy", "", "HTTP proxy URL (e.g. http://127.0.0.1:8888)")
	flag.BoolVar(&insecureTLS, "insecure", false, "skip TLS certificate verification")
	flag.BoolVar(&debug, "debug", false, "verbose request/response logging")
	flag.StringVar(&statsAddr, "stats-addr", ":9001", "address to serve /stats endpoint on (empty to disable)")
	flag.StringVar(&extraOKList, "ok", "", "extra status codes treated as success (comma-separated, e.g. \"404,409\")")
	flag.StringVar(&jsonOut, "json-out", "", "path to write a structured JSON report on exit (empty to skip)")
	flag.Parse()

	if profileFile == "" {
		fmt.Fprintln(os.Stderr, "Error: -profile is required")
		flag.Usage()
		os.Exit(2)
	}
	if rps <= 0 {
		fmt.Fprintln(os.Stderr, "Error: -rps must be positive")
		os.Exit(2)
	}

	extraOK, err := parseExtraOKCodes(extraOKList)
	if err != nil {
		log.Fatalf("parse -ok: %v", err)
	}

	prof, err := profile.LoadFromFile(profileFile)
	if err != nil {
		log.Fatalf("load profile: %v", err)
	}

	c, err := newCounter(prof, timeout, slowThreshold, proxy, insecureTLS, debug, extraOK)
	if err != nil {
		log.Fatalf("init: %v", err)
	}

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
			fmt.Fprintf(w, "\nStatus codes:\n%s", c.statusBreakdown())
			if netErrs := c.netErrBreakdown(); netErrs != "" {
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
		log.Printf("Stats endpoint: http://%s/stats", statsAddr)
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
	log.Printf("Hammering @ %d rps %s (timeout=%s, profile=%s)", rps, durLabel, timeout, profileFile)
	startTime := time.Now()

LOOP:
	for {
		select {
		case <-ctx.Done():
			break LOOP
		case <-monitor.C:
			c.tick()
		case <-ticker.C:
			go c.hammer(ctx)
		}
	}
	endTime := time.Now()

	log.Println("Stopping...")
	c.report()

	if jsonOut != "" {
		r := c.buildJSONReport(startTime, endTime, rps, profileFile)
		if err := writeJSONReport(jsonOut, r); err != nil {
			log.Printf("write json report: %v", err)
		} else {
			log.Printf("JSON report written to %s", jsonOut)
		}
	}
}

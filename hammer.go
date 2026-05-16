package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/chenchaoyi/hammer/profile"
)

type Counter struct {
	sentCount   atomic.Int64
	recvCount   atomic.Int64
	errCount    atomic.Int64
	slowCount   atomic.Int64
	totalTimeNs atomic.Int64

	lastSent int64
	lastRecv int64

	slowThreshold time.Duration

	latMu sync.Mutex
	latMs []float64 // milliseconds, successful responses only

	client  *http.Client
	profile *profile.Profile
	debug   bool
}

func newCounter(p *profile.Profile, timeout, slow time.Duration, proxyURL string, insecureTLS, debug bool) (*Counter, error) {
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
		latMs:         make([]float64, 0, 4096),
		debug:         debug,
	}, nil
}

func (c *Counter) hammer(ctx context.Context) {
	c.sentCount.Add(1)

	method, u, body, ctype, call := c.profile.NextCall()

	var reqBody io.Reader
	if body != "" {
		reqBody = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, reqBody)
	if err != nil {
		c.errCount.Add(1)
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

	t0 := time.Now()
	res, err := c.client.Do(req)
	elapsed := time.Since(t0)

	if err != nil {
		c.errCount.Add(1)
		if c.debug {
			log.Printf("response time %s err on %s %s: %v", elapsed, method, u, err)
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

	// 409 Conflict is tolerated to preserve original behavior for idempotent PATCH/PUT flows.
	if res.StatusCode >= 400 && res.StatusCode != 409 {
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
	backlog := sent - recv - errs
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
	fmt.Printf("Slow:     %d  (> %s)\n", c.slowCount.Load(), c.slowThreshold)

	if len(samples) == 0 {
		fmt.Println("No successful samples; latency stats unavailable.")
		return
	}
	pct := func(q float64) float64 {
		idx := int(q * float64(len(samples)-1))
		return samples[idx]
	}
	fmt.Printf("Latency (ms):  min=%.2f  p50=%.2f  p90=%.2f  p95=%.2f  p99=%.2f  max=%.2f\n",
		samples[0], pct(0.50), pct(0.90), pct(0.95), pct(0.99), samples[len(samples)-1])

	fmt.Println()
	fmt.Println("--- Per call ---")
	fmt.Print(c.profile.Print())
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

	prof, err := profile.LoadFromFile(profileFile)
	if err != nil {
		log.Fatalf("load profile: %v", err)
	}

	c, err := newCounter(prof, timeout, slowThreshold, proxy, insecureTLS, debug)
	if err != nil {
		log.Fatalf("init: %v", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if duration > 0 {
		var cancelDur context.CancelFunc
		ctx, cancelDur = context.WithTimeout(ctx, duration)
		defer cancelDur()
	}

	if statsAddr != "" {
		mux := http.NewServeMux()
		mux.HandleFunc("/stats", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			fmt.Fprintf(w, "Sent: %d\nReceived: %d\nErrors: %d\n==========\n%s",
				c.sentCount.Load(), c.recvCount.Load(), c.errCount.Load(), prof.Print())
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

	log.Println("Stopping...")
	c.report()
}

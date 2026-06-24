package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chenchaoyi/hammer/profile"
)

// selftestResult is the outcome of one self-test assertion.
type selftestResult struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// runSelftest drives the real load engine against an in-process HTTP server and
// verifies the request path, request accounting, status classification, SLO
// evaluation, and profile parsing all behave. It needs no network and no
// external target, so an agent (or CI) can smoke-test a freshly installed
// binary with `hammer selftest`. It exits 0 when every check passes, 1
// otherwise.
func runSelftest(args []string) int {
	fs := flag.NewFlagSet("selftest", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "emit the results as JSON to stdout")
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), `hammer selftest — smoke-test this binary with no external target

Spins up an in-process HTTP server and drives the real load engine against it,
verifying request accounting, status classification, SLO checks, and profile
parsing. Exits 0 when every check passes, 1 otherwise.

Usage:
  hammer selftest [-json]

Options:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return exitOK
		}
		return exitUsage
	}

	colorOut = colorEnabled(os.Stdout) && !*asJSON

	srv, captured := newSelftestServer()
	defer srv.Close()

	var results []selftestResult
	check := func(name string, ok bool, detail string) {
		results = append(results, selftestResult{Name: name, OK: ok, Detail: detail})
	}

	// 1. Happy path: every 200 is received, nothing is counted as an error,
	//    and a latency sample is recorded per successful response.
	{
		c := mustCounter(srv.URL+"/ok", nil)
		fireN(c, 50)
		r := c.buildJSONReport(time.Time{}, time.Time{}, 0, "selftest")
		ok := r.Sent == 50 && r.Received == 50 && r.Errors == 0 &&
			r.ErrorRate == 0 && r.LatencyMs.Samples == 50 && statusCount(r, 200) == 50
		check("load: 200 OK accounting", ok,
			fmt.Sprintf("sent=%d received=%d errors=%d rate=%.2f samples=%d code200=%d",
				r.Sent, r.Received, r.Errors, r.ErrorRate, r.LatencyMs.Samples, statusCount(r, 200)))
	}

	// 2. A 5xx response is a failure: counted as an error, never received.
	{
		c := mustCounter(srv.URL+"/status?code=500", nil)
		fireN(c, 20)
		r := c.buildJSONReport(time.Time{}, time.Time{}, 0, "selftest")
		ok := r.Errors == 20 && r.Received == 0 && r.ErrorRate == 1 && statusCount(r, 500) == 20
		check("errors: 5xx counted as failures", ok,
			fmt.Sprintf("errors=%d received=%d rate=%.2f code500=%d",
				r.Errors, r.Received, r.ErrorRate, statusCount(r, 500)))
	}

	// 3. -ok turns an otherwise-failing status into a success.
	{
		c := mustCounter(srv.URL+"/status?code=404", map[int]bool{404: true})
		fireN(c, 10)
		r := c.buildJSONReport(time.Time{}, time.Time{}, 0, "selftest")
		ok := r.Received == 10 && r.Errors == 0 && r.ErrorRate == 0 && statusCount(r, 404) == 10
		check("extra-ok: -ok code suppresses error", ok,
			fmt.Sprintf("received=%d errors=%d code404=%d", r.Received, r.Errors, statusCount(r, 404)))
	}

	// 4. SLO evaluation fires both ways on a single run: a tiny p95 limit
	//    fails, a generous one passes.
	{
		c := mustCounter(srv.URL+"/slow", nil)
		fireN(c, 10)
		r := c.buildJSONReport(time.Time{}, time.Time{}, 0, "selftest")
		strict := thresholds{maxErrorRate: -1, maxP95: time.Millisecond}.evaluate(r)
		loose := thresholds{maxErrorRate: -1, maxP95: 10 * time.Second}.evaluate(r)
		ok := strict != nil && !strict.Passed && loose != nil && loose.Passed
		detail := "checks not evaluated"
		if strict != nil && loose != nil {
			detail = fmt.Sprintf("p95=%.2fms strictPass=%v loosePass=%v",
				r.LatencyMs.P95, strict.Passed, loose.Passed)
		}
		check("slo: p95 threshold pass/fail", ok, detail)
	}

	// 5. Per-call headers and the inferred Content-Type reach the server.
	{
		captured.reset()
		c := mustCounterFull("POST", srv.URL+"/echo", `{"x":1}`, "REST",
			map[string]string{"Authorization": "Bearer selftest"}, nil)
		fireN(c, 3)
		ok := captured.contentType() == "application/json; charset=utf-8" &&
			captured.auth() == "Bearer selftest" && captured.bodyLen() > 0
		check("request: headers + content-type propagate", ok,
			fmt.Sprintf("ct=%q auth=%q bodyLen=%d",
				captured.contentType(), captured.auth(), captured.bodyLen()))
	}

	// 6. The array and stream profile forms parse to the same set of calls.
	{
		arr, errArr := profile.LoadFromReader(strings.NewReader(
			`[{"weight":1,"method":"GET","url":"http://x/a"},{"weight":1,"method":"GET","url":"http://x/b"}]`))
		stream, errStream := profile.LoadFromReader(strings.NewReader(
			`{"weight":1,"method":"GET","url":"http://x/a"} {"weight":1,"method":"GET","url":"http://x/b"}`))
		ok := errArr == nil && errStream == nil && arr != nil && stream != nil &&
			len(arr.Calls()) == 2 && len(stream.Calls()) == 2
		check("profile: array and stream forms agree", ok,
			fmt.Sprintf("arrErr=%v streamErr=%v", errArr, errStream))
	}

	return reportSelftest(os.Stdout, results, *asJSON)
}

// fireN drives n requests synchronously through the real request path so the
// self-test is deterministic (no rate/timing flakiness).
func fireN(c *Counter, n int) {
	ctx := context.Background()
	for i := 0; i < n; i++ {
		c.hammer(ctx)
	}
}

// mustCounter builds a single-call GET counter for url. It panics only on a
// genuine internal bug (the inputs are static and valid), which surfaces as a
// non-zero exit rather than a silently-passing self-test.
func mustCounter(url string, extraOK map[int]bool) *Counter {
	return mustCounterFull("GET", url, "", "", nil, extraOK)
}

func mustCounterFull(method, url, body, ctype string, headers map[string]string, extraOK map[int]bool) *Counter {
	prof, err := profile.SingleCall(method, url, body, ctype, headers)
	if err != nil {
		panic("selftest: build profile: " + err.Error())
	}
	if extraOK == nil {
		extraOK = map[int]bool{}
	}
	c, err := newCounter(prof, 30*time.Second, time.Second, "", false, false, extraOK)
	if err != nil {
		panic("selftest: build counter: " + err.Error())
	}
	c.quiet = true
	return c
}

// statusCount returns the recorded count for an HTTP status code, or 0.
func statusCount(r *JSONReport, code int) int64 {
	for _, b := range r.StatusCodes {
		if b.Code == code {
			return b.Count
		}
	}
	return 0
}

// capturedReq records what the /echo handler last saw, so the self-test can
// assert headers and body propagate. Guarded by a mutex purely for safety.
type capturedReq struct {
	mu      sync.Mutex
	ct      string
	authHdr string
	bodyN   int
}

func (c *capturedReq) reset() {
	c.mu.Lock()
	c.ct, c.authHdr, c.bodyN = "", "", 0
	c.mu.Unlock()
}

func (c *capturedReq) set(ct, auth string, n int) {
	c.mu.Lock()
	c.ct, c.authHdr, c.bodyN = ct, auth, n
	c.mu.Unlock()
}

func (c *capturedReq) contentType() string { c.mu.Lock(); defer c.mu.Unlock(); return c.ct }
func (c *capturedReq) auth() string        { c.mu.Lock(); defer c.mu.Unlock(); return c.authHdr }
func (c *capturedReq) bodyLen() int        { c.mu.Lock(); defer c.mu.Unlock(); return c.bodyN }

// newSelftestServer returns an in-process server with the handful of endpoints
// the self-test drives, plus the capture sink wired into /echo.
func newSelftestServer() (*httptest.Server, *capturedReq) {
	captured := &capturedReq{}
	mux := http.NewServeMux()
	mux.HandleFunc("/ok", func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "ok")
	})
	mux.HandleFunc("/slow", func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(15 * time.Millisecond)
		io.WriteString(w, "slow")
	})
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		code := 200
		if v := r.URL.Query().Get("code"); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				code = n
			}
		}
		w.WriteHeader(code)
	})
	mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		captured.set(r.Header.Get("Content-Type"), r.Header.Get("Authorization"), len(b))
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	})
	return httptest.NewServer(mux), captured
}

// reportSelftest renders the results (text or JSON) and returns the exit code.
func reportSelftest(w io.Writer, results []selftestResult, asJSON bool) int {
	passed := 0
	for _, r := range results {
		if r.OK {
			passed++
		}
	}
	failed := len(results) - passed

	if asJSON {
		out := struct {
			Passed  bool             `json:"passed"`
			Total   int              `json:"total"`
			Failed  int              `json:"failed"`
			Results []selftestResult `json:"results"`
		}{Passed: failed == 0, Total: len(results), Failed: failed, Results: results}
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(out)
	} else {
		fmt.Fprintln(w, po("hammer selftest", cOrange, cBold))
		fmt.Fprintln(w)
		for _, r := range results {
			label := po("PASS", cGreen)
			if !r.OK {
				label = po("FAIL", cRed)
			}
			fmt.Fprintf(w, "  %s  %s\n", label, r.Name)
			if !r.OK && r.Detail != "" {
				fmt.Fprintf(w, "        %s\n", po(r.Detail, cDim))
			}
		}
		fmt.Fprintln(w)
		summary := fmt.Sprintf("%d passed, %d failed", passed, failed)
		color := cGreen
		if failed > 0 {
			color = cRed
		}
		fmt.Fprintln(w, po(summary, color, cBold))
	}

	if failed > 0 {
		return exitChecks
	}
	return exitOK
}

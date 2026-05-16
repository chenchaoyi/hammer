package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenchaoyi/hammer/profile"
)

func loadOneCallProfile(t *testing.T, url, method, body, ctype string) *profile.Profile {
	t.Helper()
	path := filepath.Join(t.TempDir(), "profile.json")
	contents := `{"Weight":1,"Method":"` + method + `","URL":"` + url +
		`","Body":` + jsonString(body) + `,"Type":"` + ctype + `"}`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write profile: %v", err)
	}
	prof, err := profile.LoadFromFile(path)
	if err != nil {
		t.Fatalf("load profile: %v", err)
	}
	return prof
}

func jsonString(s string) string {
	// Tiny JSON-string escaper sufficient for test bodies.
	out := []byte{'"'}
	for _, r := range s {
		switch r {
		case '\\', '"':
			out = append(out, '\\', byte(r))
		case '\n':
			out = append(out, '\\', 'n')
		default:
			out = append(out, []byte(string(r))...)
		}
	}
	out = append(out, '"')
	return string(out)
}

func newTestCounter(t *testing.T, prof *profile.Profile, timeout, slow time.Duration) *Counter {
	t.Helper()
	c, err := newCounter(prof, timeout, slow, "", false, false, nil)
	if err != nil {
		t.Fatalf("newCounter: %v", err)
	}
	return c
}

func TestHammer_successCountsAndRecordsLatency(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	prof := loadOneCallProfile(t, srv.URL+"/x", "GET", "", "")
	c := newTestCounter(t, prof, time.Second, time.Second)
	c.hammer(context.Background())

	if got := c.sentCount.Load(); got != 1 {
		t.Errorf("sent=%d, want 1", got)
	}
	if got := c.recvCount.Load(); got != 1 {
		t.Errorf("recv=%d, want 1", got)
	}
	if got := c.errCount.Load(); got != 0 {
		t.Errorf("err=%d, want 0", got)
	}
	if hits.Load() != 1 {
		t.Errorf("server hits=%d, want 1", hits.Load())
	}
	if len(c.latMs) != 1 {
		t.Errorf("latency samples=%d, want 1", len(c.latMs))
	}
}

func TestHammer_status500CountsAsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	prof := loadOneCallProfile(t, srv.URL, "GET", "", "")
	c := newTestCounter(t, prof, time.Second, time.Second)
	c.hammer(context.Background())

	if got := c.errCount.Load(); got != 1 {
		t.Errorf("err=%d, want 1", got)
	}
	if got := c.recvCount.Load(); got != 0 {
		t.Errorf("recv=%d, want 0", got)
	}
}

// Without -ok, 409 is now an error (the previous hard-coded exception was
// dropped). With -ok 409, it counts as success.
func TestHammer_409IsErrorByDefault(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
	}))
	defer srv.Close()

	prof := loadOneCallProfile(t, srv.URL, "GET", "", "")
	c := newTestCounter(t, prof, time.Second, time.Second)
	c.hammer(context.Background())

	if got := c.errCount.Load(); got != 1 {
		t.Errorf("err=%d, want 1", got)
	}
	if got := c.recvCount.Load(); got != 0 {
		t.Errorf("recv=%d, want 0", got)
	}
	if got := c.statusCounts[409].Load(); got != 1 {
		t.Errorf("statusCounts[409]=%d, want 1", got)
	}
}

func TestHammer_extraOKCodeTreatsStatusAsSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
	}))
	defer srv.Close()

	prof := loadOneCallProfile(t, srv.URL, "GET", "", "")
	c, err := newCounter(prof, time.Second, time.Second, "", false, false, map[int]bool{409: true})
	if err != nil {
		t.Fatalf("newCounter: %v", err)
	}
	c.hammer(context.Background())

	if got := c.errCount.Load(); got != 0 {
		t.Errorf("err=%d, want 0", got)
	}
	if got := c.recvCount.Load(); got != 1 {
		t.Errorf("recv=%d, want 1", got)
	}
}

func TestHammer_statusHistogramBuckets(t *testing.T) {
	codes := []int{200, 204, 301, 404, 500}
	var idx atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		i := int(idx.Add(1)-1) % len(codes)
		w.WriteHeader(codes[i])
	}))
	defer srv.Close()

	prof := loadOneCallProfile(t, srv.URL, "GET", "", "")
	c := newTestCounter(t, prof, time.Second, time.Second)
	for i := 0; i < len(codes); i++ {
		c.hammer(context.Background())
	}
	for _, code := range codes {
		if got := c.statusCounts[code].Load(); got != 1 {
			t.Errorf("statusCounts[%d]=%d, want 1", code, got)
		}
	}
}

func TestHammer_slowResponseCounted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(80 * time.Millisecond)
	}))
	defer srv.Close()

	prof := loadOneCallProfile(t, srv.URL, "GET", "", "")
	c := newTestCounter(t, prof, time.Second, 30*time.Millisecond)
	c.hammer(context.Background())

	if got := c.slowCount.Load(); got != 1 {
		t.Errorf("slow=%d, want 1", got)
	}
}

func TestHammer_clientTimeoutCountsAsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(300 * time.Millisecond)
	}))
	defer srv.Close()

	prof := loadOneCallProfile(t, srv.URL, "GET", "", "")
	c := newTestCounter(t, prof, 40*time.Millisecond, time.Second)
	c.hammer(context.Background())

	if got := c.errCount.Load(); got != 1 {
		t.Errorf("err=%d, want 1 on timeout", got)
	}
	if got := c.netErrCounts[netErrTimeout].Load(); got != 1 {
		t.Errorf("netErrCounts[timeout]=%d, want 1", got)
	}
}

// Distinguishes the "client timeout" bucket from the "canceled" bucket:
// per-request -timeout firing must be a timeout, never a cancel.
func TestHammer_clientTimeoutIsTimeoutNotCanceled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(300 * time.Millisecond)
	}))
	defer srv.Close()

	prof := loadOneCallProfile(t, srv.URL, "GET", "", "")
	c := newTestCounter(t, prof, 30*time.Millisecond, time.Second)
	c.hammer(context.Background())

	if got := c.netErrCounts[netErrTimeout].Load(); got != 1 {
		t.Errorf("netErrCounts[timeout]=%d, want 1", got)
	}
	if got := c.canceledCount.Load(); got != 0 {
		t.Errorf("canceledCount=%d, want 0 (client timeout != run cancel)", got)
	}
}

func TestHammer_contextCancelGoesToCanceledBucket(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	prof := loadOneCallProfile(t, srv.URL, "GET", "", "")
	c := newTestCounter(t, prof, time.Second, time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		c.hammer(ctx)
		close(done)
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	<-done

	if got := c.canceledCount.Load(); got != 1 {
		t.Errorf("canceled=%d, want 1", got)
	}
	if got := c.errCount.Load(); got != 0 {
		t.Errorf("err=%d, want 0 (cancel should not count as an error)", got)
	}
}

func TestHammer_connRefusedClassified(t *testing.T) {
	// Pick a port the kernel will refuse (no listener bound).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	prof := loadOneCallProfile(t, "http://"+addr+"/", "GET", "", "")
	c := newTestCounter(t, prof, time.Second, time.Second)
	c.hammer(context.Background())

	if got := c.errCount.Load(); got != 1 {
		t.Errorf("err=%d, want 1", got)
	}
	if got := c.netErrCounts[netErrConn].Load(); got != 1 {
		t.Errorf("netErrCounts[conn]=%d, want 1", got)
	}
}

func TestHammer_setsContentTypeForPOST(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	prof := loadOneCallProfile(t, srv.URL, "POST", `{"a":1}`, "REST")
	c := newTestCounter(t, prof, time.Second, time.Second)
	c.hammer(context.Background())

	if got != "application/json; charset=utf-8" {
		t.Errorf("Content-Type=%q, want REST mapping", got)
	}
}

func TestHammer_noContentTypeForGET(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// "REST" must not be applied to GET.
	prof := loadOneCallProfile(t, srv.URL, "GET", "", "REST")
	c := newTestCounter(t, prof, time.Second, time.Second)
	c.hammer(context.Background())

	if got != "" {
		t.Errorf("Content-Type=%q, want empty on GET", got)
	}
}

func TestNewCounter_withProxyConfiguresTransport(t *testing.T) {
	prof := loadOneCallProfile(t, "http://example.com/", "GET", "", "")
	c, err := newCounter(prof, time.Second, time.Second, "http://127.0.0.1:8888", false, false, nil)
	if err != nil {
		t.Fatalf("newCounter: %v", err)
	}
	tr, ok := c.client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport type = %T", c.client.Transport)
	}
	if tr.Proxy == nil {
		t.Error("Transport.Proxy not set")
	}
}

func TestNewCounter_invalidProxy(t *testing.T) {
	prof := loadOneCallProfile(t, "http://example.com/", "GET", "", "")
	_, err := newCounter(prof, time.Second, time.Second, ":bad-proxy", false, false, nil)
	if err == nil {
		t.Fatal("expected error on malformed proxy URL")
	}
}

func TestHammer_perCallHeadersAreSentAndOverrideContentType(t *testing.T) {
	gotHeaders := make(chan http.Header, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Clone is safe to ship across the goroutine boundary.
		gotHeaders <- r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "profile.json")
	contents := `{
		"Weight": 1,
		"Method": "POST",
		"URL": "` + srv.URL + `",
		"Body": "{}",
		"Type": "REST",
		"Headers": {
			"Authorization": "Bearer xyz",
			"X-Trace-Id": "abc",
			"Content-Type": "application/vnd.custom+json"
		}
	}`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	prof, err := profile.LoadFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	c := newTestCounter(t, prof, time.Second, time.Second)
	c.hammer(context.Background())

	h := <-gotHeaders
	if got := h.Get("Authorization"); got != "Bearer xyz" {
		t.Errorf("Authorization=%q, want %q", got, "Bearer xyz")
	}
	if got := h.Get("X-Trace-Id"); got != "abc" {
		t.Errorf("X-Trace-Id=%q, want %q", got, "abc")
	}
	if got := h.Get("Content-Type"); got != "application/vnd.custom+json" {
		t.Errorf("Content-Type=%q, want per-call header to override REST default", got)
	}
}

func TestHammer_templatedURLIsRenderedPerRequest(t *testing.T) {
	paths := make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case paths <- r.URL.RequestURI():
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "profile.json")
	contents := `{
		"Weight": 1,
		"Method": "GET",
		"URL": "` + srv.URL + `/users/{{ randInt 1000000 }}",
		"Body": ""
	}`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	prof, err := profile.LoadFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	c := newTestCounter(t, prof, time.Second, time.Second)

	// Fire several requests and verify URLs differ (random IDs).
	for i := 0; i < 4; i++ {
		c.hammer(context.Background())
	}
	close(paths)
	seen := map[string]bool{}
	for p := range paths {
		if !strings.HasPrefix(p, "/users/") {
			t.Errorf("path doesn't have expected prefix: %q", p)
		}
		seen[p] = true
	}
	if len(seen) < 2 {
		t.Errorf("expected multiple distinct paths from {{ randInt }}, got %d (%v)", len(seen), seen)
	}
}

func TestBuildJSONReport_writesValidStructuredFile(t *testing.T) {
	codes := []int{200, 200, 200, 500}
	var idx atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		i := int(idx.Add(1)-1) % len(codes)
		w.WriteHeader(codes[i])
	}))
	defer srv.Close()

	prof := loadOneCallProfile(t, srv.URL, "GET", "", "")
	c := newTestCounter(t, prof, time.Second, time.Second)
	for i := 0; i < len(codes); i++ {
		c.hammer(context.Background())
	}

	start := time.Now().Add(-1 * time.Second)
	end := time.Now()
	r := c.buildJSONReport(start, end, 100, "fake.json")

	out := filepath.Join(t.TempDir(), "report.json")
	if err := writeJSONReport(out, r); err != nil {
		t.Fatalf("writeJSONReport: %v", err)
	}

	// Read back and verify structure round-trips.
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var got JSONReport
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v\ndata=%s", err, data)
	}

	if got.Sent != 4 {
		t.Errorf("Sent=%d, want 4", got.Sent)
	}
	if got.Received != 3 {
		t.Errorf("Received=%d, want 3", got.Received)
	}
	if got.Errors != 1 {
		t.Errorf("Errors=%d, want 1", got.Errors)
	}
	if got.TargetRPS != 100 || got.Profile != "fake.json" {
		t.Errorf("TargetRPS=%d Profile=%q", got.TargetRPS, got.Profile)
	}
	if got.DurationSec <= 0 {
		t.Errorf("DurationSec=%v, want > 0", got.DurationSec)
	}

	// Status codes should include 200:3 and 500:1.
	found200, found500 := false, false
	for _, s := range got.StatusCodes {
		if s.Code == 200 && s.Count == 3 {
			found200 = true
		}
		if s.Code == 500 && s.Count == 1 {
			found500 = true
		}
	}
	if !found200 || !found500 {
		t.Errorf("status buckets missing: %+v", got.StatusCodes)
	}

	if got.LatencyMs.Samples != 3 {
		t.Errorf("LatencyMs.Samples=%d, want 3", got.LatencyMs.Samples)
	}
	if got.LatencyMs.Mean <= 0 {
		t.Errorf("LatencyMs.Mean=%v, want > 0", got.LatencyMs.Mean)
	}

	if len(got.PerCall) != 1 || got.PerCall[0].Count != 3 {
		t.Errorf("PerCall=%+v", got.PerCall)
	}
}

func TestParseExtraOKCodes(t *testing.T) {
	tests := []struct {
		in     string
		want   map[int]bool
		hasErr bool
	}{
		{"", map[int]bool{}, false},
		{"404", map[int]bool{404: true}, false},
		{" 404 , 409 ,422 ", map[int]bool{404: true, 409: true, 422: true}, false},
		{"abc", nil, true},
		{"99", nil, true},
		{"600", nil, true},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := parseExtraOKCodes(tc.in)
			if (err != nil) != tc.hasErr {
				t.Fatalf("err=%v, hasErr=%v", err, tc.hasErr)
			}
			if tc.hasErr {
				return
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("got[%d]=%v, want %v", k, got[k], v)
				}
			}
		})
	}
}

func make100() []float64 {
	v := make([]float64, 100)
	for i := range v {
		v[i] = float64(i + 1)
	}
	return v
}

func TestPercentile(t *testing.T) {
	tests := []struct {
		name   string
		input  []float64
		q      float64
		want   float64
	}{
		{"empty", nil, 0.5, 0},
		{"single", []float64{42}, 0.5, 42},
		{"p50", []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, 0.50, 5},
		{"p90", []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, 0.90, 9},
		{"p99-of-10", []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, 0.99, 9},  // int(0.99*9)=8 → 9
		{"p99-of-100", make100(), 0.99, 99},                                  // int(0.99*99)=98 → values[98]=99
		{"min", []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, 0, 1},
		{"q-clamped-low", []float64{1, 2, 3}, -1, 1},
		{"q-clamped-high", []float64{1, 2, 3}, 2, 3},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := percentile(tc.input, tc.q); got != tc.want {
				t.Errorf("percentile(%v, %v)=%v, want %v", tc.input, tc.q, got, tc.want)
			}
		})
	}
}

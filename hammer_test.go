package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	c, err := newCounter(prof, timeout, slow, "", false, false)
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

func TestHammer_status409Tolerated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
	}))
	defer srv.Close()

	prof := loadOneCallProfile(t, srv.URL, "GET", "", "")
	c := newTestCounter(t, prof, time.Second, time.Second)
	c.hammer(context.Background())

	if got := c.errCount.Load(); got != 0 {
		t.Errorf("err=%d, want 0", got)
	}
	if got := c.recvCount.Load(); got != 1 {
		t.Errorf("recv=%d, want 1", got)
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
	c, err := newCounter(prof, time.Second, time.Second, "http://127.0.0.1:8888", false, false)
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
	_, err := newCounter(prof, time.Second, time.Second, ":bad-proxy", false, false)
	if err == nil {
		t.Fatal("expected error on malformed proxy URL")
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

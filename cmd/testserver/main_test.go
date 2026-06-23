package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHelloHandler(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/hello", nil)
	rec := httptest.NewRecorder()
	hello(rec, req)

	res := rec.Result()
	if res.StatusCode != http.StatusOK {
		t.Errorf("status = %d; want 200", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content-type = %q; want text/html", ct)
	}
	if !strings.Contains(rec.Body.String(), "Hello World") {
		t.Errorf("body = %q; want it to contain 'Hello World'", rec.Body.String())
	}
}

func TestHelloJSONHandler(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/hello_in_json", nil)
	rec := httptest.NewRecorder()
	helloJSON(rec, req)

	res := rec.Result()
	if ct := res.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q; want application/json", ct)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `{"msg":"hello world"}` {
		t.Errorf("body = %q", got)
	}
}

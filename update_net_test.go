package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureFile redirects *target (os.Stdout or os.Stderr) to a pipe for the
// duration of fn and returns everything written to it.
func captureFile(t *testing.T, target **os.File, fn func()) string {
	t.Helper()
	orig := *target
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	*target = w
	outCh := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		outCh <- buf.String()
	}()
	defer func() { *target = orig }()
	fn()
	_ = w.Close()
	s := <-outCh
	_ = r.Close()
	return s
}

func setStringVar(t *testing.T, p *string, v string) {
	t.Helper()
	orig := *p
	*p = v
	t.Cleanup(func() { *p = orig })
}

func TestGetRelease(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if got := r.Header.Get("Accept"); got != "application/vnd.github+json" {
				t.Errorf("Accept header = %q", got)
			}
			io.WriteString(w, `{"tag_name":"v9.9.9"}`)
		}))
		defer srv.Close()
		rel, err := getRelease(context.Background(), srv.Client(), srv.URL)
		if err != nil {
			t.Fatalf("getRelease: %v", err)
		}
		if rel.TagName != "v9.9.9" {
			t.Errorf("tag = %q; want v9.9.9", rel.TagName)
		}
	})

	t.Run("not found", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		defer srv.Close()
		_, err := getRelease(context.Background(), srv.Client(), srv.URL)
		if err == nil || !strings.Contains(err.Error(), "not found") {
			t.Errorf("err = %v; want 'not found'", err)
		}
	})

	t.Run("server error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, "boom")
		}))
		defer srv.Close()
		_, err := getRelease(context.Background(), srv.Client(), srv.URL)
		if err == nil || !strings.Contains(err.Error(), "500") {
			t.Errorf("err = %v; want status 500", err)
		}
	})

	t.Run("invalid JSON", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			io.WriteString(w, "not json")
		}))
		defer srv.Close()
		_, err := getRelease(context.Background(), srv.Client(), srv.URL)
		if err == nil || !strings.Contains(err.Error(), "decode") {
			t.Errorf("err = %v; want decode error", err)
		}
	})

	t.Run("empty tag", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			io.WriteString(w, `{"tag_name":""}`)
		}))
		defer srv.Close()
		_, err := getRelease(context.Background(), srv.Client(), srv.URL)
		if err == nil || !strings.Contains(err.Error(), "tag_name") {
			t.Errorf("err = %v; want tag_name error", err)
		}
	})
}

func TestFetchLatestAndByTag(t *testing.T) {
	var gotPaths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPaths = append(gotPaths, r.URL.Path)
		io.WriteString(w, `{"tag_name":"v2.3.4"}`)
	}))
	defer srv.Close()
	setStringVar(t, &apiBaseURL, srv.URL)

	rel, err := fetchLatestRelease(context.Background(), srv.Client(), "owner/name")
	if err != nil || rel.TagName != "v2.3.4" {
		t.Fatalf("fetchLatestRelease = %v, %v", rel, err)
	}
	rel, err = fetchReleaseByTag(context.Background(), srv.Client(), "owner/name", "v2.3.4")
	if err != nil || rel.TagName != "v2.3.4" {
		t.Fatalf("fetchReleaseByTag = %v, %v", rel, err)
	}
	want := []string{"/repos/owner/name/releases/latest", "/repos/owner/name/releases/tags/v2.3.4"}
	if strings.Join(gotPaths, ",") != strings.Join(want, ",") {
		t.Errorf("request paths = %v; want %v", gotPaths, want)
	}
}

func TestDownloadTo(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			io.WriteString(w, "the-bytes")
		}))
		defer srv.Close()
		path, err := downloadTo(context.Background(), srv.Client(), srv.URL)
		if err != nil {
			t.Fatalf("downloadTo: %v", err)
		}
		defer os.Remove(path)
		got, _ := os.ReadFile(path)
		if string(got) != "the-bytes" {
			t.Errorf("downloaded = %q", got)
		}
	})

	t.Run("non-200", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusForbidden)
		}))
		defer srv.Close()
		if _, err := downloadTo(context.Background(), srv.Client(), srv.URL); err == nil {
			t.Error("expected error for 403 response")
		}
	})
}

func TestDownloadWithFallback(t *testing.T) {
	t.Run("github first succeeds", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			io.WriteString(w, "primary")
		}))
		defer srv.Close()
		path, err := downloadWithFallback(context.Background(), srv.Client(), srv.URL, "github")
		if err != nil {
			t.Fatalf("downloadWithFallback: %v", err)
		}
		defer os.Remove(path)
		got, _ := os.ReadFile(path)
		if string(got) != "primary" {
			t.Errorf("content = %q; want primary", got)
		}
	})

	t.Run("falls back to mirror when primary fails", func(t *testing.T) {
		// Primary always 500s; the mirror serves any path with 200.
		primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer primary.Close()
		mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			io.WriteString(w, "from-mirror")
		}))
		defer mirror.Close()

		// Custom mirror mode: candidates = [primaryURL, mirrorURL/+primaryURL].
		path, err := downloadWithFallback(context.Background(), mirror.Client(),
			primary.URL+"/asset", mirror.URL+"/")
		if err != nil {
			t.Fatalf("downloadWithFallback: %v", err)
		}
		defer os.Remove(path)
		got, _ := os.ReadFile(path)
		if string(got) != "from-mirror" {
			t.Errorf("content = %q; want from-mirror", got)
		}
	})

	t.Run("all candidates fail", func(t *testing.T) {
		primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer primary.Close()
		if _, err := downloadWithFallback(context.Background(), primary.Client(), primary.URL, "github"); err == nil {
			t.Error("expected error when every candidate fails")
		}
	})
}

// errReader fails partway through a read to exercise the io.Copy error path.
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("boom") }

func TestWriteTempBinaryReadError(t *testing.T) {
	if _, err := writeTempBinary(errReader{}); err == nil {
		t.Error("expected error when the source reader fails")
	}
}

func TestMoveFile(t *testing.T) {
	t.Run("rename within same dir", func(t *testing.T) {
		dir := t.TempDir()
		src := dir + "/src"
		dst := dir + "/dst"
		if err := os.WriteFile(src, []byte("data"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := moveFile(src, dst); err != nil {
			t.Fatalf("moveFile: %v", err)
		}
		if _, err := os.Stat(src); !os.IsNotExist(err) {
			t.Error("source should be gone after move")
		}
		got, _ := os.ReadFile(dst)
		if string(got) != "data" {
			t.Errorf("dst content = %q", got)
		}
	})

	t.Run("missing source errors", func(t *testing.T) {
		dir := t.TempDir()
		if err := moveFile(dir+"/nope", dir+"/dst"); err == nil {
			t.Error("expected error for missing source")
		}
	})
}

func TestRunUpdateCheck(t *testing.T) {
	newServer := func(tag string, status int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if status != 0 {
				w.WriteHeader(status)
				return
			}
			io.WriteString(w, `{"tag_name":"`+tag+`"}`)
		}))
	}

	t.Run("update available", func(t *testing.T) {
		srv := newServer("v9.9.9", 0)
		defer srv.Close()
		setStringVar(t, &apiBaseURL, srv.URL)
		setStringVar(t, &version, "v1.0.0")

		var code int
		out := captureFile(t, &os.Stderr, func() { code = runUpdate([]string{"-check"}) })
		if code != exitOK {
			t.Errorf("exit = %d; want %d", code, exitOK)
		}
		if !strings.Contains(stripANSI(out), "Update available") {
			t.Errorf("output missing 'Update available':\n%s", out)
		}
	})

	t.Run("already up to date", func(t *testing.T) {
		srv := newServer("v1.0.0", 0)
		defer srv.Close()
		setStringVar(t, &apiBaseURL, srv.URL)
		setStringVar(t, &version, "v1.0.0")

		var code int
		out := captureFile(t, &os.Stderr, func() { code = runUpdate([]string{"-check"}) })
		if code != exitOK {
			t.Errorf("exit = %d; want %d", code, exitOK)
		}
		if !strings.Contains(stripANSI(out), "already up to date") {
			t.Errorf("output missing 'already up to date':\n%s", out)
		}
	})

	t.Run("pinned version via -version uses tags endpoint", func(t *testing.T) {
		var gotPath string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			io.WriteString(w, `{"tag_name":"v3.0.0"}`)
		}))
		defer srv.Close()
		setStringVar(t, &apiBaseURL, srv.URL)
		setStringVar(t, &version, "v1.0.0")

		var code int
		_ = captureFile(t, &os.Stderr, func() { code = runUpdate([]string{"-check", "-version", "v3.0.0"}) })
		if code != exitOK {
			t.Errorf("exit = %d; want %d", code, exitOK)
		}
		if !strings.Contains(gotPath, "/releases/tags/v3.0.0") {
			t.Errorf("request path = %q; want tags endpoint", gotPath)
		}
	})

	t.Run("API error exits setup", func(t *testing.T) {
		srv := newServer("", http.StatusInternalServerError)
		defer srv.Close()
		setStringVar(t, &apiBaseURL, srv.URL)
		setStringVar(t, &version, "v1.0.0")

		var code int
		_ = captureFile(t, &os.Stderr, func() { code = runUpdate([]string{"-check"}) })
		if code != exitSetup {
			t.Errorf("exit = %d; want %d", code, exitSetup)
		}
	})

	t.Run("help exits ok", func(t *testing.T) {
		var code int
		_ = captureFile(t, &os.Stderr, func() { code = runUpdate([]string{"-h"}) })
		if code != exitOK {
			t.Errorf("exit = %d; want %d for -h", code, exitOK)
		}
	})

	t.Run("bad flag exits usage", func(t *testing.T) {
		var code int
		_ = captureFile(t, &os.Stderr, func() { code = runUpdate([]string{"-nonexistent-flag"}) })
		if code != exitUsage {
			t.Errorf("exit = %d; want %d for bad flag", code, exitUsage)
		}
	})

	t.Run("declining the prompt cancels", func(t *testing.T) {
		srv := newServer("v9.9.9", 0)
		defer srv.Close()
		setStringVar(t, &apiBaseURL, srv.URL)
		setStringVar(t, &version, "v1.0.0")

		// Feed "n\n" to the confirmation prompt via os.Stdin.
		stdinFile := filepath.Join(t.TempDir(), "stdin")
		if err := os.WriteFile(stdinFile, []byte("n\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		f, err := os.Open(stdinFile)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		origStdin := os.Stdin
		os.Stdin = f
		t.Cleanup(func() { os.Stdin = origStdin })

		var code int
		out := captureFile(t, &os.Stderr, func() { code = runUpdate(nil) })
		if code != exitOK {
			t.Errorf("exit = %d; want %d", code, exitOK)
		}
		if !strings.Contains(stripANSI(out), "canceled") {
			t.Errorf("output missing 'canceled':\n%s", out)
		}
	})
}

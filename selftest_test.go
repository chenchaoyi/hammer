package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
)

// captureStdout runs fn with os.Stdout redirected to a pipe and returns what it
// wrote, restoring os.Stdout afterward.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = orig })

	ch := make(chan string, 1)
	go func() { var b bytes.Buffer; _, _ = io.Copy(&b, r); ch <- b.String() }()

	fn()
	_ = w.Close()
	out := <-ch
	_ = r.Close()
	os.Stdout = orig
	return out
}

func TestRunSelftest_allPass(t *testing.T) {
	var code int
	out := captureStdout(t, func() { code = runSelftest(nil) })
	if code != exitOK {
		t.Fatalf("exit=%d, want %d\noutput:\n%s", code, exitOK, out)
	}
	if !strings.Contains(out, "0 failed") {
		t.Errorf("expected '0 failed' in output, got:\n%s", out)
	}
	if strings.Contains(out, "FAIL") {
		t.Errorf("unexpected FAIL line in passing run:\n%s", out)
	}
}

func TestRunSelftest_json(t *testing.T) {
	var code int
	out := captureStdout(t, func() { code = runSelftest([]string{"-json"}) })
	if code != exitOK {
		t.Fatalf("exit=%d, want %d", code, exitOK)
	}
	var res struct {
		Passed  bool             `json:"passed"`
		Total   int              `json:"total"`
		Failed  int              `json:"failed"`
		Results []selftestResult `json:"results"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out)
	}
	if !res.Passed || res.Failed != 0 || res.Total != len(res.Results) || res.Total == 0 {
		t.Errorf("unexpected summary: %+v", res)
	}
	for _, r := range res.Results {
		if !r.OK {
			t.Errorf("check %q failed: %s", r.Name, r.Detail)
		}
	}
}

func TestRunSelftest_help(t *testing.T) {
	var code int
	_ = captureStdout(t, func() { code = runSelftest([]string{"-h"}) })
	if code != exitOK {
		t.Errorf("-h exit=%d, want %d", code, exitOK)
	}
}

func TestRunSelftest_badFlag(t *testing.T) {
	var code int
	_ = captureStdout(t, func() { code = runSelftest([]string{"-no-such-flag"}) })
	if code != exitUsage {
		t.Errorf("bad flag exit=%d, want %d", code, exitUsage)
	}
}

// reportSelftest must surface failures with the FAIL exit code and detail line.
func TestReportSelftest_failExitAndDetail(t *testing.T) {
	results := []selftestResult{
		{Name: "good", OK: true},
		{Name: "bad", OK: false, Detail: "expected 1 got 0"},
	}
	var code int
	out := captureStdout(t, func() { code = reportSelftest(os.Stdout, results, false) })
	if code != exitChecks {
		t.Errorf("exit=%d, want %d", code, exitChecks)
	}
	if !strings.Contains(out, "FAIL") || !strings.Contains(out, "1 passed, 1 failed") {
		t.Errorf("unexpected output:\n%s", out)
	}
	if !strings.Contains(out, "expected 1 got 0") {
		t.Errorf("detail line missing from failing check:\n%s", out)
	}
}

func TestStatusCount(t *testing.T) {
	r := &JSONReport{StatusCodes: []StatusBucket{{Code: 200, Count: 3}, {Code: 500, Count: 1}}}
	if got := statusCount(r, 200); got != 3 {
		t.Errorf("statusCount(200)=%d, want 3", got)
	}
	if got := statusCount(r, 404); got != 0 {
		t.Errorf("statusCount(404)=%d, want 0", got)
	}
}

// The in-process server the self-test drives must answer its endpoints and
// capture what /echo received.
func TestSelftestServer_endpointsAndCapture(t *testing.T) {
	srv, captured := newSelftestServer()
	defer srv.Close()

	cc := mustCounterFull("POST", srv.URL+"/echo", `{"a":1}`, "WWW",
		map[string]string{"Authorization": "Bearer abc"}, nil)
	fireN(cc, 1)
	if captured.auth() != "Bearer abc" {
		t.Errorf("auth=%q, want %q", captured.auth(), "Bearer abc")
	}
	if captured.contentType() != "application/x-www-form-urlencoded" {
		t.Errorf("ct=%q", captured.contentType())
	}
	if captured.bodyLen() == 0 {
		t.Error("bodyLen=0, want >0")
	}
	captured.reset()
	if captured.auth() != "" || captured.bodyLen() != 0 {
		t.Error("reset did not clear capture")
	}
}

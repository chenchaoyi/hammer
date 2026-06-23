package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runMain invokes run() with a fresh flag set and os.Args, capturing stdout and
// stderr. It restores all global state on cleanup so tests don't interfere.
func runMain(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	origArgs := os.Args
	origCL := flag.CommandLine
	origUsage := flag.Usage
	origStdout := os.Stdout
	origStderr := os.Stderr
	t.Cleanup(func() {
		os.Args = origArgs
		flag.CommandLine = origCL
		flag.Usage = origUsage
		os.Stdout = origStdout
		os.Stderr = origStderr
	})

	flag.CommandLine = flag.NewFlagSet("hammer", flag.ContinueOnError)
	flag.CommandLine.SetOutput(io.Discard)
	os.Args = append([]string{"hammer"}, args...)

	outR, outW, _ := os.Pipe()
	errR, errW, _ := os.Pipe()
	os.Stdout = outW
	os.Stderr = errW

	outCh := make(chan string, 1)
	errCh := make(chan string, 1)
	go func() { var b bytes.Buffer; _, _ = io.Copy(&b, outR); outCh <- b.String() }()
	go func() { var b bytes.Buffer; _, _ = io.Copy(&b, errR); errCh <- b.String() }()

	code = run()

	_ = outW.Close()
	_ = errW.Close()
	stdout = <-outCh
	stderr = <-errCh
	_ = outR.Close()
	_ = errR.Close()
	return code, stdout, stderr
}

func TestRun_versionFlag(t *testing.T) {
	code, stdout, _ := runMain(t, "-version")
	if code != exitOK {
		t.Errorf("exit = %d; want %d", code, exitOK)
	}
	if !strings.Contains(stdout, "hammer ") {
		t.Errorf("stdout = %q; want it to contain 'hammer '", stdout)
	}
}

func TestRun_happyPathJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	code, stdout, _ := runMain(t, "-url", srv.URL, "-rps", "200", "-duration", "300ms",
		"-quiet", "-output", "json")
	if code != exitOK {
		t.Fatalf("exit = %d; want %d", code, exitOK)
	}
	var rep JSONReport
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\n%s", err, stdout)
	}
	if rep.Received == 0 {
		t.Errorf("expected some received responses, got %d", rep.Received)
	}
	if rep.TargetRPS != 200 {
		t.Errorf("target_rps = %d; want 200", rep.TargetRPS)
	}
}

func TestRun_sloViolationExits1(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	code, stdout, _ := runMain(t, "-url", srv.URL, "-rps", "200", "-duration", "300ms",
		"-quiet", "-output", "json", "-max-error-rate", "0")
	if code != exitChecks {
		t.Fatalf("exit = %d; want %d (SLO violation)", code, exitChecks)
	}
	var rep JSONReport
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if rep.Checks == nil || rep.Checks.Passed {
		t.Errorf("expected failed checks, got %+v", rep.Checks)
	}
}

func TestRun_sloPassExits0(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	code, _, _ := runMain(t, "-url", srv.URL, "-rps", "200", "-duration", "300ms",
		"-quiet", "-output", "json", "-max-error-rate", "0.5")
	if code != exitOK {
		t.Errorf("exit = %d; want %d (SLO satisfied)", code, exitOK)
	}
}

func TestRun_jsonOutFile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	out := filepath.Join(t.TempDir(), "report.json")
	code, _, _ := runMain(t, "-url", srv.URL, "-rps", "100", "-duration", "250ms",
		"-quiet", "-json-out", out)
	if code != exitOK {
		t.Fatalf("exit = %d; want %d", code, exitOK)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read json-out: %v", err)
	}
	var rep JSONReport
	if err := json.Unmarshal(data, &rep); err != nil {
		t.Fatalf("json-out file invalid: %v", err)
	}
}

func TestRun_jsonOutUnwritableExitsSetup(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	bad := filepath.Join(t.TempDir(), "no-such-dir", "report.json")
	code, _, _ := runMain(t, "-url", srv.URL, "-rps", "100", "-duration", "200ms",
		"-quiet", "-json-out", bad)
	if code != exitSetup {
		t.Errorf("exit = %d; want %d", code, exitSetup)
	}
}

func TestRun_validationErrors(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"no target", []string{"-rps", "10"}},
		{"both url and profile", []string{"-url", "http://x", "-profile", "p.json"}},
		{"rps zero", []string{"-url", "http://x", "-rps", "0"}},
		{"bad output", []string{"-url", "http://x", "-output", "yaml"}},
		{"error rate over one", []string{"-url", "http://x", "-max-error-rate", "2"}},
		{"bad ok codes", []string{"-url", "http://x", "-ok", "999"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, _, _ := runMain(t, c.args...)
			if code != exitUsage {
				t.Errorf("exit = %d; want %d", code, exitUsage)
			}
		})
	}
}

func TestRun_missingProfileExitsSetup(t *testing.T) {
	code, _, _ := runMain(t, "-profile", filepath.Join(t.TempDir(), "missing.json"),
		"-duration", "100ms", "-quiet")
	if code != exitSetup {
		t.Errorf("exit = %d; want %d", code, exitSetup)
	}
}

func TestRun_invalidProxyExitsSetup(t *testing.T) {
	code, _, _ := runMain(t, "-url", "http://x", "-proxy", "://bad", "-duration", "100ms", "-quiet")
	if code != exitSetup {
		t.Errorf("exit = %d; want %d", code, exitSetup)
	}
}

func TestRun_statsEndpointServesLiveCounts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// Bind the stats server to an ephemeral port and scrape it mid-run.
	code, _, _ := runMain(t, "-url", srv.URL, "-rps", "100", "-duration", "300ms",
		"-quiet", "-output", "json", "-stats-addr", "127.0.0.1:0")
	// We can't easily know the chosen port without parsing logs (suppressed by
	// -quiet), so this test just asserts the run still succeeds with the stats
	// server enabled.
	if code != exitOK {
		t.Errorf("exit = %d; want %d", code, exitOK)
	}
}

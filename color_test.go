package main

import (
	"os"
	"strings"
	"testing"
)

func TestColorEnabledHonorsNoColor(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	if colorEnabled(os.Stdout) {
		t.Error("colorEnabled should be false when NO_COLOR is set")
	}
}

func TestColorEnabledHonorsDumbTerm(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "dumb")
	if colorEnabled(os.Stdout) {
		t.Error("colorEnabled should be false when TERM=dumb")
	}
}

func TestIsTTYFalseForPipe(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if isTTY(w) {
		t.Error("a pipe should not be reported as a TTY")
	}
}

func TestNoColorIsIdentity(t *testing.T) {
	if got := noColor("hello", cRed, cBold); got != "hello" {
		t.Errorf("noColor = %q; want %q", got, "hello")
	}
}

func TestPlainBreakdownsHaveNoANSI(t *testing.T) {
	c := populatedReportCounter(t)

	status := c.statusBreakdownPlain()
	for _, code := range []string{"200", "404", "500"} {
		if !strings.Contains(status, code) {
			t.Errorf("plain status breakdown missing %s:\n%s", code, status)
		}
	}
	if strings.Contains(status, "\x1b[") {
		t.Errorf("plain status breakdown should have no ANSI codes:\n%q", status)
	}

	netErrs := c.netErrBreakdownPlain()
	if !strings.Contains(netErrs, "timeout") {
		t.Errorf("plain net-err breakdown missing 'timeout':\n%s", netErrs)
	}
	if strings.Contains(netErrs, "\x1b[") {
		t.Errorf("plain net-err breakdown should have no ANSI codes:\n%q", netErrs)
	}
}

func TestStatusColorRanges(t *testing.T) {
	cases := map[int]string{
		204: cGreen,
		301: cCyan,
		404: cYellow,
		503: cRed,
		100: cDim,
	}
	for code, want := range cases {
		if got := statusColor(code); got != want {
			t.Errorf("statusColor(%d) = %q; want %q", code, got, want)
		}
	}
}

func TestStatusBreakdownNoneWhenEmpty(t *testing.T) {
	prof := loadOneCallProfile(t, "https://example.test/", "GET", "", "")
	c := newTestCounter(t, prof, 0, 0)
	if got := c.statusBreakdownPlain(); !strings.Contains(got, "(none)") {
		t.Errorf("empty status breakdown = %q; want it to contain '(none)'", got)
	}
	if got := c.netErrBreakdownPlain(); got != "" {
		t.Errorf("empty net-err breakdown = %q; want empty string", got)
	}
}

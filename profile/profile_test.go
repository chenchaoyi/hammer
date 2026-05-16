package profile

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTempProfile(t *testing.T, contents string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "profile.json")
	if err := os.WriteFile(p, []byte(contents), 0o600); err != nil {
		t.Fatalf("write temp profile: %v", err)
	}
	return p
}

func TestLoadFromFile_normalizesAndAccumulatesWeights(t *testing.T) {
	p := writeTempProfile(t, `
		{"Weight": 1, "Method": "get",  "URL": "http://example.com/a", "Body": ""}
		{"Weight": 3, "Method": "post", "URL": "http://example.com/b", "Body": "x", "Type": "rest"}
	`)

	prof, err := LoadFromFile(p)
	if err != nil {
		t.Fatalf("LoadFromFile: %v", err)
	}
	if got, want := len(prof.calls), 2; got != want {
		t.Fatalf("calls=%d, want %d", got, want)
	}
	if prof.calls[0].Method != "GET" {
		t.Errorf("Method not upper-cased: %q", prof.calls[0].Method)
	}
	if prof.calls[1].Type != "REST" {
		t.Errorf("Type not upper-cased: %q", prof.calls[1].Type)
	}
	if prof.totalWeight != 4 {
		t.Errorf("totalWeight=%v, want 4", prof.totalWeight)
	}
	if prof.calls[0].randomWeight != 1 || prof.calls[1].randomWeight != 4 {
		t.Errorf("cumulative weights = %v, %v; want 1, 4",
			prof.calls[0].randomWeight, prof.calls[1].randomWeight)
	}
}

func TestLoadFromFile_missingFile(t *testing.T) {
	if _, err := LoadFromFile("/does/not/exist/profile.json"); err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestLoadFromFile_emptyFile(t *testing.T) {
	p := writeTempProfile(t, "")
	_, err := LoadFromFile(p)
	if err == nil || !strings.Contains(err.Error(), "no calls") {
		t.Fatalf("expected 'no calls' error, got %v", err)
	}
}

func TestLoadFromFile_invalidJSON(t *testing.T) {
	p := writeTempProfile(t, "{not json}")
	if _, err := LoadFromFile(p); err == nil {
		t.Fatal("expected error on invalid JSON")
	}
}

func TestLoadFromFile_nonPositiveWeight(t *testing.T) {
	p := writeTempProfile(t, `{"Weight": 0, "Method": "GET", "URL": "http://x/"}`)
	_, err := LoadFromFile(p)
	if err == nil || !strings.Contains(err.Error(), "weight") {
		t.Fatalf("expected weight error, got %v", err)
	}
}

// Guards against the historical 2KB read truncation bug.
func TestLoadFromFile_largeStreamNotTruncated(t *testing.T) {
	var b strings.Builder
	const n = 200
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, `{"Weight":1,"Method":"GET","URL":"http://example.com/path-%d","Body":""}`+"\n", i)
	}
	p := writeTempProfile(t, b.String())

	prof, err := LoadFromFile(p)
	if err != nil {
		t.Fatalf("LoadFromFile: %v", err)
	}
	if len(prof.calls) != n {
		t.Errorf("calls=%d, want %d (file likely truncated)", len(prof.calls), n)
	}
}

func TestNextCall_weightedDistribution(t *testing.T) {
	p := writeTempProfile(t, `
		{"Weight": 70, "Method": "GET", "URL": "http://a/", "Body": ""}
		{"Weight": 30, "Method": "GET", "URL": "http://b/", "Body": ""}
	`)
	prof, err := LoadFromFile(p)
	if err != nil {
		t.Fatal(err)
	}

	const total = 100_000
	counts := map[string]int{}
	for i := 0; i < total; i++ {
		counts[prof.NextCall().URL]++
	}
	ratio := float64(counts["http://a/"]) / float64(total)
	// Allow ±2pp drift on a 100k-sample run.
	if ratio < 0.68 || ratio > 0.72 {
		t.Errorf("a-url ratio=%.4f, want 0.70±0.02", ratio)
	}
}

func TestLoadFromFile_parsesHeaders(t *testing.T) {
	p := writeTempProfile(t, `{
		"Weight": 1,
		"Method": "GET",
		"URL": "http://example.com/",
		"Body": "",
		"Headers": {
			"Authorization": "Bearer abc123",
			"X-Trace-Id": "load-test"
		}
	}`)
	prof, err := LoadFromFile(p)
	if err != nil {
		t.Fatalf("LoadFromFile: %v", err)
	}
	got := prof.NextCall().Headers
	if got["Authorization"] != "Bearer abc123" {
		t.Errorf("Authorization=%q, want %q", got["Authorization"], "Bearer abc123")
	}
	if got["X-Trace-Id"] != "load-test" {
		t.Errorf("X-Trace-Id=%q, want %q", got["X-Trace-Id"], "load-test")
	}
}

func TestCall_RenderNoTemplate(t *testing.T) {
	p := writeTempProfile(t, `{"Weight":1,"Method":"GET","URL":"http://x/static","Body":"plain"}`)
	prof, err := LoadFromFile(p)
	if err != nil {
		t.Fatal(err)
	}
	c := prof.NextCall()
	if c.urlTmpl != nil || c.bodyTmpl != nil {
		t.Fatal("templates should not be compiled when no {{ }} present")
	}
	u, b, err := c.Render()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if u != "http://x/static" || b != "plain" {
		t.Errorf("Render returned %q, %q", u, b)
	}
}

func TestCall_RenderExpandsTemplates(t *testing.T) {
	p := writeTempProfile(t, `{
		"Weight": 1,
		"Method": "GET",
		"URL": "http://x/users/{{ randInt 1000 }}?trace={{ uuid }}",
		"Body": "{\"name\":\"{{ randString 8 }}\",\"region\":\"{{ pickOne \"us\" \"eu\" \"ap\" }}\"}"
	}`)
	prof, err := LoadFromFile(p)
	if err != nil {
		t.Fatal(err)
	}
	c := prof.NextCall()
	if c.urlTmpl == nil || c.bodyTmpl == nil {
		t.Fatal("templates should be compiled")
	}
	u, b, err := c.Render()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	// URL: numeric id + uuid; "{{" must be gone.
	if strings.Contains(u, "{{") {
		t.Errorf("URL still has template markers: %q", u)
	}
	if !strings.HasPrefix(u, "http://x/users/") || !strings.Contains(u, "?trace=") {
		t.Errorf("URL shape unexpected: %q", u)
	}
	// Body: random 8-char name + region from the allowlist.
	if !strings.Contains(b, `"name":"`) || strings.Contains(b, "{{") {
		t.Errorf("Body shape unexpected: %q", b)
	}
	region := ""
	for _, r := range []string{"us", "eu", "ap"} {
		if strings.Contains(b, `"region":"`+r+`"`) {
			region = r
			break
		}
	}
	if region == "" {
		t.Errorf("region not one of us/eu/ap: %q", b)
	}

	// Calling Render twice must yield different values for random pieces.
	u2, _, _ := c.Render()
	if u == u2 {
		// Could happen with astronomically low probability; retry once.
		u2, _, _ = c.Render()
		if u == u2 {
			t.Errorf("two consecutive renders produced identical URLs: %q", u)
		}
	}
}

func TestCall_RenderTemplateParseError(t *testing.T) {
	p := writeTempProfile(t, `{"Weight":1,"Method":"GET","URL":"http://x/{{ unknownFunc }}","Body":""}`)
	if _, err := LoadFromFile(p); err == nil {
		t.Fatal("expected template parse error")
	}
}

func TestNextCall_returnsCallPointerForRecording(t *testing.T) {
	p := writeTempProfile(t, `{"Weight":1,"Method":"GET","URL":"http://x/","Body":""}`)
	prof, err := LoadFromFile(p)
	if err != nil {
		t.Fatal(err)
	}
	call := prof.NextCall()
	call.Record(1_500_000_000) // 1.5s
	call.Record(2_500_000_000) // 2.5s
	if got := call.count.Load(); got != 2 {
		t.Errorf("count=%d, want 2", got)
	}
	if got := call.totalTimeNs.Load(); got != 4_000_000_000 {
		t.Errorf("totalTimeNs=%d, want 4e9", got)
	}
	if s := call.String(); !strings.Contains(s, "Total Call: 2") || !strings.Contains(s, "2.0000s") {
		t.Errorf("unexpected String: %q", s)
	}
}

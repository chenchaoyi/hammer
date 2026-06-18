package profile

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"strings"
	"sync/atomic"
	"text/template"
	"time"
)

// Call describes one weighted HTTP call in a traffic profile.
type Call struct {
	Weight float32 `json:"Weight"`
	Method string  `json:"Method"`
	URL    string  `json:"URL"`
	Body   string  `json:"Body"`
	// Type is an optional Content-Type hint for write methods:
	// "REST" → application/json; charset=utf-8
	// "WWW"  → application/x-www-form-urlencoded
	// any other non-empty value is used as-is.
	Type string `json:"Type,omitempty"`
	// Headers are sent with every request for this call. If a header
	// conflicts with the Content-Type implied by Type, Headers wins.
	Headers map[string]string `json:"Headers,omitempty"`

	randomWeight float32
	urlTmpl      *template.Template // nil when URL has no {{ }} placeholders
	bodyTmpl     *template.Template

	count       atomic.Int64
	totalTimeNs atomic.Int64
}

func (c *Call) Record(d int64) {
	c.count.Add(1)
	c.totalTimeNs.Add(d)
}

// Count returns the number of successful requests recorded for this call.
func (c *Call) Count() int64 { return c.count.Load() }

// TotalTimeNs returns the cumulative response time of successful requests.
func (c *Call) TotalTimeNs() int64 { return c.totalTimeNs.Load() }

// Render returns the URL and body to use for one request, expanding any
// {{ }} placeholders. When the call has no templates both fields are
// returned verbatim and no allocations are performed.
func (c *Call) Render() (url, body string, err error) {
	url, body = c.URL, c.Body
	if c.urlTmpl != nil {
		var b bytes.Buffer
		if err := c.urlTmpl.Execute(&b, nil); err != nil {
			return "", "", fmt.Errorf("render URL template: %w", err)
		}
		url = b.String()
	}
	if c.bodyTmpl != nil {
		var b bytes.Buffer
		if err := c.bodyTmpl.Execute(&b, nil); err != nil {
			return "", "", fmt.Errorf("render body template: %w", err)
		}
		body = b.String()
	}
	return url, body, nil
}

func (c *Call) String() string {
	n := c.count.Load()
	avg := 0.0
	if n > 0 {
		avg = float64(c.totalTimeNs.Load()) / float64(n) / 1e9
	}
	return fmt.Sprintf("API: %s %s\nTotal Call: %d\nAvg RT: %.4fs", c.Method, c.URL, n, avg)
}

// Profile is a weighted set of Calls.
type Profile struct {
	totalWeight float32
	calls       []*Call
}

// Calls returns the profile's calls in their declared order. The returned
// slice and its elements live for the lifetime of the Profile; callers
// must not mutate them.
func (p *Profile) Calls() []*Call { return p.calls }

// LoadFromFile parses a traffic profile from a file containing a stream of
// JSON-encoded Call objects (one after another, optionally separated by
// whitespace).
func LoadFromFile(path string) (*Profile, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	p, err := LoadFromReader(f)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return p, nil
}

// LoadFromReader parses a traffic profile from any reader containing a stream
// of JSON-encoded Call objects. It lets callers feed a profile from stdin or
// any other source without first materializing a file.
func LoadFromReader(r io.Reader) (*Profile, error) {
	p := &Profile{}
	dec := json.NewDecoder(r)
	for {
		c := &Call{}
		if err := dec.Decode(c); err == io.EOF {
			break
		} else if err != nil {
			return nil, err
		}
		if err := p.add(c); err != nil {
			return nil, err
		}
	}
	if len(p.calls) == 0 {
		return nil, fmt.Errorf("no calls found")
	}
	return p, nil
}

// SingleCall builds a one-call profile from explicit parameters, compiling
// URL/body templates exactly like a file-loaded profile. It powers hammer's
// zero-config mode, where an agent targets a single URL via flags instead of
// authoring a profile file.
func SingleCall(method, url, body, ctype string, headers map[string]string) (*Profile, error) {
	p := &Profile{}
	c := &Call{
		Weight:  1,
		Method:  method,
		URL:     url,
		Body:    body,
		Type:    ctype,
		Headers: headers,
	}
	if err := p.add(c); err != nil {
		return nil, err
	}
	return p, nil
}

// add normalizes, validates, compiles templates for, and appends a single
// Call, accumulating its weight. It is the shared core of every loader.
func (p *Profile) add(c *Call) error {
	c.Method = strings.ToUpper(c.Method)
	c.Type = strings.ToUpper(c.Type)
	if c.Weight <= 0 {
		return fmt.Errorf("call %s %s has non-positive weight", c.Method, c.URL)
	}
	if t, err := compileTemplate("url", c.URL); err != nil {
		return fmt.Errorf("URL template for %s %s: %w", c.Method, c.URL, err)
	} else {
		c.urlTmpl = t
	}
	if t, err := compileTemplate("body", c.Body); err != nil {
		return fmt.Errorf("body template for %s %s: %w", c.Method, c.URL, err)
	} else {
		c.bodyTmpl = t
	}
	p.totalWeight += c.Weight
	c.randomWeight = p.totalWeight
	p.calls = append(p.calls, c)
	return nil
}

// NextCall picks a call according to its weight.
func (p *Profile) NextCall() *Call {
	r := rand.Float32() * p.totalWeight
	for _, c := range p.calls {
		if r <= c.randomWeight {
			return c
		}
	}
	return p.calls[len(p.calls)-1]
}

func (p *Profile) Print() string {
	var b strings.Builder
	for _, c := range p.calls {
		b.WriteString(c.String())
		b.WriteString("\n+++++++\n")
	}
	return b.String()
}

// --- Templating ---------------------------------------------------------

// tmplFuncs are the helpers available inside URL / Body templates.
// math/rand/v2 is used throughout: its top-level functions are
// lock-free, so heavy templates do not serialize the load loop.
var tmplFuncs = template.FuncMap{
	"randInt":      func(n int) int { return rand.IntN(n) },
	"randIntRange": func(lo, hi int) int { return lo + rand.IntN(hi-lo) },
	"randString":   randString,
	"uuid":         newUUIDv4,
	"now":          func() int64 { return time.Now().Unix() },
	"nowNano":      func() int64 { return time.Now().UnixNano() },
	"pickOne":      pickOne,
}

func pickOne(args ...string) string {
	if len(args) == 0 {
		return ""
	}
	return args[rand.IntN(len(args))]
}

const alnum = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

func randString(n int) string {
	if n <= 0 {
		return ""
	}
	b := make([]byte, n)
	for i := range b {
		b[i] = alnum[rand.IntN(len(alnum))]
	}
	return string(b)
}

func newUUIDv4() string {
	var b [16]byte
	binary.LittleEndian.PutUint64(b[0:8], rand.Uint64())
	binary.LittleEndian.PutUint64(b[8:16], rand.Uint64())
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10xx
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// compileTemplate returns nil (no error) when text contains no template
// markers, so callers can skip Execute() and avoid allocations.
func compileTemplate(name, text string) (*template.Template, error) {
	if !strings.Contains(text, "{{") {
		return nil, nil
	}
	return template.New(name).Funcs(tmplFuncs).Parse(text)
}

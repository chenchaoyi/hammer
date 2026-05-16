package profile

import (
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"os"
	"strings"
	"sync/atomic"
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
	count        atomic.Int64
	totalTimeNs  atomic.Int64
}

func (c *Call) Record(d int64) {
	c.count.Add(1)
	c.totalTimeNs.Add(d)
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

// LoadFromFile parses a traffic profile from a file containing a stream of
// JSON-encoded Call objects (one after another, optionally separated by
// whitespace).
func LoadFromFile(path string) (*Profile, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	p := &Profile{}
	dec := json.NewDecoder(f)
	for {
		c := &Call{}
		if err := dec.Decode(c); err == io.EOF {
			break
		} else if err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		c.Method = strings.ToUpper(c.Method)
		c.Type = strings.ToUpper(c.Type)
		if c.Weight <= 0 {
			return nil, fmt.Errorf("call %s %s has non-positive weight", c.Method, c.URL)
		}
		p.totalWeight += c.Weight
		c.randomWeight = p.totalWeight
		p.calls = append(p.calls, c)
	}
	if len(p.calls) == 0 {
		return nil, fmt.Errorf("no calls found in %s", path)
	}
	return p, nil
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

package main

import (
	"os"
	"strings"
)

// Resolved once in main() after flags are parsed.
var (
	colorOut bool // colorize stdout (the final report)
	colorErr bool // colorize stderr (live monitor + banners)
)

const (
	cReset  = "\x1b[0m"
	cBold   = "\x1b[1m"
	cOrange = "\x1b[38;2;254;166;43m"
	cGreen  = "\x1b[38;2;152;224;36m"
	cYellow = "\x1b[38;2;253;151;31m"
	cYelHi  = "\x1b[38;2;253;172;74m"
	cRed    = "\x1b[38;2;244;0;95m"
	cCyan   = "\x1b[38;2;88;209;235m"
	cFg     = "\x1b[38;2;224;224;224m"
	cDim    = "\x1b[38;2;153;153;153m"
)

func isTTY(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func colorEnabled(f *os.File) bool {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	return isTTY(f)
}

// paint wraps s in SGR codes only when enabled is true; otherwise it returns s
// verbatim so plain-mode output stays byte-for-byte stable.
func paint(enabled bool, s string, codes ...string) string {
	if !enabled || len(codes) == 0 {
		return s
	}
	var b strings.Builder
	for _, c := range codes {
		b.WriteString(c)
	}
	b.WriteString(s)
	b.WriteString(cReset)
	return b.String()
}

func po(s string, codes ...string) string { return paint(colorOut, s, codes...) }
func pe(s string, codes ...string) string { return paint(colorErr, s, codes...) }

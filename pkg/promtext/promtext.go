// Package promtext writes the Prometheus text exposition format.
//
// ADR-0011 explains why this is here rather than client_golang: Corral's
// metrics are a projection of a snapshot computed all at once, not counters
// incremented at the point of work, so the library's registry-and-collector
// model would be a dependency taken on to do the easy part.
//
// The commitment that comes with that is escaping. Label values here carry
// instance names and doctor detail strings, which contain quotes, backslashes,
// and newlines; a metrics endpoint that emits an unescaped one produces a body
// Prometheus rejects wholesale, losing every series rather than the one. That
// is the part with tests.
package promtext

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Writer accumulates an exposition body.
//
// It enforces one rule the format requires and a hand-written exporter easily
// breaks: HELP and TYPE are emitted once per metric name, before its first
// sample, and all samples of a name must be contiguous. Callers therefore
// declare a metric and then write its samples, rather than interleaving.
type Writer struct {
	buf   strings.Builder
	named map[string]bool
	// current is the metric being written, so an out-of-order sample is caught
	// here rather than by a scraper.
	current string
}

func New() *Writer { return &Writer{named: map[string]bool{}} }

// Metric declares a metric and makes it current. Samples written after it
// belong to it until the next Metric call.
func (w *Writer) Metric(name, kind, help string) *Writer {
	if w.named[name] {
		// Re-declaring is a bug in the caller — the samples would be
		// non-contiguous and Prometheus would reject the body.
		w.current = name
		return w
	}
	w.named[name] = true
	w.current = name
	if w.buf.Len() > 0 {
		w.buf.WriteByte('\n')
	}
	fmt.Fprintf(&w.buf, "# HELP %s %s\n# TYPE %s %s\n", name, escapeHelp(help), name, kind)
	return w
}

// Sample writes one line of the current metric. Labels are sorted so a body is
// byte-stable across runs, which makes a diff of two scrapes readable and a
// golden test possible.
func (w *Writer) Sample(labels map[string]string, value float64) *Writer {
	if w.current == "" {
		return w
	}
	w.buf.WriteString(w.current)
	if len(labels) > 0 {
		keys := make([]string, 0, len(labels))
		for k := range labels {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		w.buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				w.buf.WriteByte(',')
			}
			fmt.Fprintf(&w.buf, `%s="%s"`, k, EscapeLabel(labels[k]))
		}
		w.buf.WriteByte('}')
	}
	w.buf.WriteByte(' ')
	w.buf.WriteString(formatValue(value))
	w.buf.WriteByte('\n')
	return w
}

// Bool is the common case: a gauge that is 1 or 0. Written as a helper because
// `b2f(vm.Running)` at every call site is where an inverted condition hides.
func (w *Writer) Bool(labels map[string]string, on bool) *Writer {
	if on {
		return w.Sample(labels, 1)
	}
	return w.Sample(labels, 0)
}

func (w *Writer) String() string { return w.buf.String() }

// ContentType is what a /metrics handler must send. Prometheus tolerates
// text/plain, but naming the version is what tells a scraper it is not being
// handed an HTML error page.
const ContentType = "text/plain; version=0.0.4; charset=utf-8"

// EscapeLabel escapes a label value per the exposition format: backslash,
// double quote, and newline. Nothing else — escaping more would corrupt values
// that are legal, and a tab in an instance name is legal.
func EscapeLabel(v string) string {
	if !strings.ContainsAny(v, "\\\"\n") {
		return v
	}
	var b strings.Builder
	b.Grow(len(v) + 8)
	for _, r := range v {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// escapeHelp escapes a HELP string, where the rules differ from label values:
// quotes are literal, and only backslash and newline need escaping.
func escapeHelp(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	return strings.ReplaceAll(v, "\n", `\n`)
}

// formatValue renders a float the way the format expects. Integers are written
// without a decimal point, because "3" reads better than "3.000000" in a body
// someone will curl.
func formatValue(f float64) string {
	if f == float64(int64(f)) {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

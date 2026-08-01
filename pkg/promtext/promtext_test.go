package promtext

import (
	"strings"
	"testing"
)

func TestWriterEmitsHelpAndTypeOncePerMetric(t *testing.T) {
	w := New()
	w.Metric("corral_instance_running", "gauge", "1 when the instance is running")
	w.Sample(map[string]string{"name": "a"}, 1)
	w.Sample(map[string]string{"name": "b"}, 0)

	out := w.String()
	if strings.Count(out, "# HELP corral_instance_running") != 1 {
		t.Fatalf("HELP should appear once:\n%s", out)
	}
	if strings.Count(out, "# TYPE corral_instance_running gauge") != 1 {
		t.Fatalf("TYPE should appear once:\n%s", out)
	}
	if strings.Count(out, "corral_instance_running{") != 2 {
		t.Fatalf("both samples should be written:\n%s", out)
	}
}

func TestWriterSortsLabelsForAStableBody(t *testing.T) {
	w := New()
	w.Metric("m", "gauge", "h")
	w.Sample(map[string]string{"zeta": "1", "alpha": "2", "mid": "3"}, 1)
	if !strings.Contains(w.String(), `m{alpha="2",mid="3",zeta="1"} 1`) {
		t.Fatalf("labels should be sorted:\n%s", w.String())
	}
}

func TestWriterIgnoresSamplesBeforeAnyMetric(t *testing.T) {
	w := New()
	w.Sample(map[string]string{"a": "b"}, 1)
	if w.String() != "" {
		t.Fatalf("a sample with no declared metric would be an unparseable body, got %q", w.String())
	}
}

func TestBoolWritesOneAndZero(t *testing.T) {
	w := New()
	w.Metric("m", "gauge", "h")
	w.Bool(nil, true)
	w.Bool(nil, false)
	if got := w.String(); !strings.Contains(got, "m 1\n") || !strings.Contains(got, "m 0\n") {
		t.Fatalf("Bool should write 1 and 0:\n%s", got)
	}
}

// TestEscapeLabel is the test this package exists for. An unescaped quote in an
// instance name produces a body Prometheus rejects wholesale — every series
// lost, not one.
func TestEscapeLabel(t *testing.T) {
	for in, want := range map[string]string{
		"plain":          "plain",
		`say "hi"`:       `say \"hi\"`,
		`back\slash`:     `back\\slash`,
		"two\nlines":     `two\nlines`,
		`all "\` + "\n":  `all \"\\\n`,
		"":               "",
		"unicode-ü-name": "unicode-ü-name",
		"tab\there":      "tab\there", // legal unescaped; escaping it would corrupt the value
	} {
		if got := EscapeLabel(in); got != want {
			t.Errorf("EscapeLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEscapedLabelSurvivesInABody(t *testing.T) {
	w := New()
	w.Metric("corral_check", "gauge", "doctor checks")
	w.Sample(map[string]string{"detail": `kubectl said "no" \ then stopped`}, 0)

	out := w.String()
	if strings.Contains(out, `"no"`) {
		t.Fatalf("the inner quotes must be escaped, or the line ends early:\n%s", out)
	}
	// Exactly one label block: an unescaped quote would have closed it early
	// and turned the rest of the detail into garbage tokens.
	line := strings.TrimSpace(strings.Split(out, "\n")[2])
	if !strings.HasPrefix(line, "corral_check{detail=\"") || !strings.HasSuffix(line, "} 0") {
		t.Fatalf("line is malformed: %q", line)
	}
}

func TestHelpEscapingKeepsQuotesButNotBackslashes(t *testing.T) {
	w := New()
	w.Metric("m", "gauge", `a "quoted" help with a \ backslash`)
	out := w.String()
	if !strings.Contains(out, `a "quoted" help`) {
		t.Errorf("quotes are literal in HELP:\n%s", out)
	}
	if !strings.Contains(out, `a \\ backslash`) {
		t.Errorf("backslashes must be escaped in HELP:\n%s", out)
	}
}

func TestFormatValueKeepsIntegersReadable(t *testing.T) {
	w := New()
	w.Metric("m", "gauge", "h")
	w.Sample(nil, 3)
	w.Sample(nil, 1.5)
	w.Sample(nil, 0)
	out := w.String()
	for _, want := range []string{"m 3\n", "m 1.5\n", "m 0\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "3.000000") {
		t.Errorf("an integer should not render with a decimal tail:\n%s", out)
	}
}

func TestBodyIsByteStableAcrossRuns(t *testing.T) {
	build := func() string {
		w := New()
		w.Metric("corral_instances", "gauge", "count")
		for _, b := range []string{"kubevirt", "qemu", "incus"} {
			w.Sample(map[string]string{"backend": b, "state": "running"}, 1)
		}
		return w.String()
	}
	if build() != build() {
		t.Fatal("two identical snapshots must produce identical bodies")
	}
}

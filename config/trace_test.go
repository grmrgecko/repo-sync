package config

import (
	"os"
	"strings"
	"testing"
)

// TestTraceValue verifies trace fields are reduced to a single line, so a
// configured value cannot forge additional trace fields.
func TestTraceValue(t *testing.T) {
	cases := []struct{ in, want string }{
		{"  Example Inc  ", "Example Inc"},
		{"a\nb", "a b"},
		{"a\r\nb", "a b"},
		{"a\tb", "a b"},
		{"name <mail@example.com>", "name <mail@example.com>"},
		{"Maintainer\nCountry: XX", "Maintainer Country: XX"},
		{"has\x00null", "hasnull"},
		{"   ", ""},
	}
	for _, c := range cases {
		if got := TraceValue(c.in); got != c.want {
			t.Errorf("TraceValue(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// Whatever the input, the result never spans lines.
	for _, in := range []string{"a\nb\nc", "x\r\ny", "p\tq"} {
		if strings.ContainsAny(TraceValue(in), "\r\n") {
			t.Errorf("TraceValue(%q) still contains a line break", in)
		}
	}
}

// TestTraceHostName verifies the host falls back to the system hostname
// when none is configured.
func TestTraceHostName(t *testing.T) {
	if got := (TraceConfig{Host: "mirror.example.com"}).HostName(); got != "mirror.example.com" {
		t.Errorf("configured host = %q", got)
	}
	want, err := os.Hostname()
	if err != nil || want == "" {
		t.Skip("system hostname unavailable")
	}
	if got := (TraceConfig{}).HostName(); got != want {
		t.Errorf("fallback host = %q, want %q", got, want)
	}
}

// TestTraceConfigLoads verifies the trace section is read and normalized,
// and that tracing stays off unless it is asked for.
func TestTraceConfigLoads(t *testing.T) {
	path := writeConfig(t, `
trace:
  enabled: true
  host: mirror.example.com
  maintainer: "Test  Maintainer <test@example.com>"
  sponsor: "Example Inc"
  country: US
`)
	if err := Init(path); err != nil {
		t.Fatal(err)
	}
	if !C.Trace.Enabled {
		t.Error("trace not enabled")
	}
	if C.Trace.Host != "mirror.example.com" {
		t.Errorf("host = %q", C.Trace.Host)
	}
	if C.Trace.Maintainer != "Test Maintainer <test@example.com>" {
		t.Errorf("maintainer = %q, want internal whitespace collapsed", C.Trace.Maintainer)
	}
	if C.Trace.Location != "" || C.Trace.Throughput != "" {
		t.Error("unset trace fields should stay empty")
	}

	if err := Init(writeConfig(t, "log:\n  level: info\n")); err != nil {
		t.Fatal(err)
	}
	if C.Trace.Enabled {
		t.Error("tracing must default to off")
	}
}

package main

import (
	"testing"

	cfg "github.com/grmrgecko/repo-sync/config"
)

// TestApplyTraceFlags verifies the trace flags override the configuration
// only when given, so a configured value stands on its own.
func TestApplyTraceFlags(t *testing.T) {
	configured := cfg.TraceConfig{
		Enabled:    true,
		Host:       "configured.example.com",
		Maintainer: "Configured <configured@example.com>",
		Country:    "US",
	}
	yes, no := true, false

	tests := []struct {
		name  string
		flags Flags
		want  cfg.TraceConfig
	}{
		{
			name:  "unset flags leave the configuration alone",
			flags: Flags{},
			want:  configured,
		},
		{
			name:  "--no-trace disables tracing",
			flags: Flags{Trace: &no},
			want:  func() cfg.TraceConfig { c := configured; c.Enabled = false; return c }(),
		},
		{
			name:  "--trace enables tracing",
			flags: Flags{Trace: &yes},
			want:  func() cfg.TraceConfig { c := configured; c.Enabled = true; return c }(),
		},
		{
			name:  "value flags override individually",
			flags: Flags{TraceHost: "flag.example.com", TraceSponsor: "Flag Sponsor"},
			want: func() cfg.TraceConfig {
				c := configured
				c.Host = "flag.example.com"
				c.Sponsor = "Flag Sponsor"
				return c
			}(),
		},
		{
			name:  "flag values are normalized like configured ones",
			flags: Flags{TraceMaintainer: "Flag\nMaintainer"},
			want: func() cfg.TraceConfig {
				c := configured
				c.Maintainer = "Flag Maintainer"
				return c
			}(),
		},
	}

	// applyTraceFlags reads the package level flags and configuration, so
	// each case installs its own and restores them afterwards.
	origFlags := flags
	origTrace := cfg.C.Trace
	t.Cleanup(func() {
		flags = origFlags
		cfg.C.Trace = origTrace
	})

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg.C.Trace = configured
			flags = tc.flags
			applyTraceFlags()
			if cfg.C.Trace != tc.want {
				t.Errorf("trace config = %+v, want %+v", cfg.C.Trace, tc.want)
			}
		})
	}
}

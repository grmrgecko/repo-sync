package main

import (
	cfg "github.com/grmrgecko/repo-sync/config"
	"github.com/grmrgecko/repo-sync/fetch"
	log "github.com/sirupsen/logrus"
)

// main parses the command line, installs the configuration every command
// shares, and dispatches to the selected command.
func main() {
	ctx := ParseFlags()
	if err := cfg.Init(flags.ConfigPath); err != nil {
		log.Fatal(err)
	}
	applyGlobalFlags()
	err := ctx.Run()
	ctx.FatalIfErrorf(err)
}

// applyGlobalFlags overrides the loaded configuration with the global
// command line flags, then reapplies the settings derived from it. It also
// runs after a SIGHUP reload so the flags keep winning over the file.
func applyGlobalFlags() {
	if flags.LogLevel != "" {
		cfg.C.Log.Level = flags.LogLevel
	}
	if flags.UserAgent != "" {
		cfg.C.Crawler.UserAgent = flags.UserAgent
	}
	if flags.RequestTimeout > 0 {
		cfg.C.Crawler.RequestTimeout = flags.RequestTimeout
	}
	applyTraceFlags()
	cfg.SetupLogging(cfg.C.Log)
	fetch.Reload()
}

// applyTraceFlags overlays the trace command line flags onto the loaded
// configuration. Each flag is empty when unset so the configured value
// stands, matching how the other global overrides behave.
func applyTraceFlags() {
	if flags.Trace != nil {
		cfg.C.Trace.Enabled = *flags.Trace
	}
	for _, o := range []struct {
		flag string
		dst  *string
	}{
		{flags.TraceHost, &cfg.C.Trace.Host},
		{flags.TraceMaintainer, &cfg.C.Trace.Maintainer},
		{flags.TraceSponsor, &cfg.C.Trace.Sponsor},
		{flags.TraceCountry, &cfg.C.Trace.Country},
		{flags.TraceLocation, &cfg.C.Trace.Location},
		{flags.TraceThroughput, &cfg.C.Trace.Throughput},
	} {
		if o.flag != "" {
			*o.dst = cfg.TraceValue(o.flag)
		}
	}
}

package main

import (
	"fmt"
	"runtime/debug"
	"strings"
	"time"

	"github.com/alecthomas/kong"
	cfg "github.com/grmrgecko/repo-sync/config"
)

// VersionFlag prints build information and exits.
type VersionFlag bool

// Decode satisfies kong.MapperValue. The flag is treated as a boolean toggle.
func (v VersionFlag) Decode(ctx *kong.DecodeContext) error { return nil }

// IsBool reports the flag as a boolean for kong's parser.
func (v VersionFlag) IsBool() bool { return true }

// BeforeApply emits version information then exits before the rest of the
// command is executed.
func (v VersionFlag) BeforeApply(app *kong.Kong, vars kong.Vars) error {
	fmt.Printf("%s: %s (%s)\n", cfg.Name, cfg.Version, cfg.Mode)
	if cfg.Commit != "" {
		fmt.Printf("  commit: %s\n", cfg.Commit)
	}
	if cfg.Date != "" {
		fmt.Printf("  built:  %s\n", cfg.Date)
	}
	if cfg.Commit == "" {
		if bi, ok := debug.ReadBuildInfo(); ok {
			for _, s := range bi.Settings {
				switch s.Key {
				case "vcs.revision":
					fmt.Printf("  commit: %s\n", s.Value)
				case "vcs.time":
					fmt.Printf("  built:  %s\n", s.Value)
				}
			}
		}
	}
	app.Exit(0)
	return nil
}

// Flags is the root command line configuration. The global flags override
// the configuration file for every command, so a sync run and the server
// honor the same settings; each is empty when unset so the configured
// value stands.
type Flags struct {
	ConfigPath     string        `help:"The path to the config file." optional:"" type:"existingfile"`
	LogLevel       string        `help:"Override the configured log level." enum:",debug,info,warn,error" default:""`
	UserAgent      string        `help:"Override the configured user agent sent to upstreams." optional:""`
	RequestTimeout time.Duration `help:"Override the configured upstream response timeout (e.g. 2m)." optional:""`
	Version        VersionFlag   `name:"version" help:"Print version information and quit"`

	// Trace flags override the configuration's trace section. Enabling is a
	// pointer so an unset flag leaves the configured value alone while
	// --trace and --no-trace each override it.
	Trace           *bool  `help:"Write a trace file into each synchronized repository." negatable:""`
	TraceHost       string `help:"Override the host name this mirror is traced under; defaults to the system hostname." optional:""`
	TraceMaintainer string `help:"Override the maintainer recorded in traces, in \"name <email>\" form." optional:""`
	TraceSponsor    string `help:"Override the sponsor recorded in traces." optional:""`
	TraceCountry    string `help:"Override the country recorded in traces." optional:""`
	TraceLocation   string `help:"Override the location recorded in traces." optional:""`
	TraceThroughput string `help:"Override the throughput recorded in traces." optional:""`

	Sync    SyncCmd    `cmd:"" name:"sync" help:"Synchronize package repositories of any supported type."`
	Server  ServerCmd  `cmd:"" name:"server" help:"Run the caching mirror server."`
	Service ServiceCmd `cmd:"" name:"service" help:"Manage the mirror server system service."`
}

// flags holds the parsed command line configuration.
var flags Flags

// ParseFlags parses the command line, exiting with usage information on
// error.
func ParseFlags() *kong.Context {
	return kong.Parse(&flags,
		kong.Name(cfg.Name),
		kong.Description(cfg.Description),
		kong.UsageOnError(),
		kong.ConfigureHelp(kong.HelpOptions{
			Compact: true,
		}),
		kong.Vars{
			"serviceActions": strings.Join(ServiceAction, ","),
		},
	)
}

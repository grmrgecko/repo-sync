package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	cfg "github.com/grmrgecko/repo-sync/config"
	"github.com/grmrgecko/repo-sync/fetch"
	"github.com/grmrgecko/repo-sync/mirror"
)

// SyncArgs holds the flags and positional arguments shared by every
// repository type.
type SyncArgs struct {
	Trim     int  `help:"Number of leading URL path components to drop when building the destination path." default:"0"`
	Flat     bool `help:"Place repositories directly in the destination directory without copying the URL path."`
	Discover bool `help:"Treat each URL as a directory index and crawl it for repositories."`
	Depth    int  `help:"Maximum directory depth crawled in discovery mode." default:"5"`

	DiscoverCache time.Duration `help:"Reuse a discovery crawl's results for this long before scanning the tree again (e.g. 72h); 0 scans every run, and unset defaults to the configured crawler value." default:"-1s"`

	Workers int  `help:"Number of concurrent download workers; defaults to the configured crawler workers."`
	Verify  bool `help:"Re-verify checksums of files that already exist locally."`
	Prune   bool `help:"Delete local files that are no longer part of the repository."`

	Exclude     []string `help:"Glob of directory or file names discovery never crawls; repeatable, and matched against the path below the crawled URL when it contains a slash, which a leading slash anchors to that URL (e.g. sles, 'yum/docker*', /docker)."`
	IncludeFile []string `help:"Glob of loose files found outside repositories that discovery mirrors as well; repeatable, and matched by path on the same rules as --exclude (e.g. '*.rpm', 'RPM-GPG-KEY-*', '/*.rpm')."`

	PruneGrace time.Duration `help:"Keep files that left the repository for this long before pruning them (e.g. 72h); defaults to the configured crawler prune grace."`
	DryRun     bool          `help:"Report what would be downloaded or pruned without changing the repository."`

	Missing        string `help:"How to handle a package the repository metadata lists but the upstream does not serve: fail the repository, retry it across runs before accepting it as gone, or ignore it; defaults to the configured crawler mode." enum:",fail,retry,ignore" default:""`
	MissingRetries int    `help:"Number of consecutive runs a file must be missing upstream before --missing=retry accepts it as gone; defaults to the configured crawler value." default:"-1"`

	Args []string `arg:"" name:"url" help:"Repository or mirrorlist URLs followed by the destination directory."`
}

// options validates the shared arguments and builds the mirror options.
// The types limit which repository formats the run accepts; an empty list
// accepts every supported format.
func (s *SyncArgs) options(types []mirror.RepoType) (*mirror.Options, error) {
	if len(s.Args) < 2 {
		return nil, errors.New("expected at least one repository URL and a destination directory")
	}
	urls := s.Args[:len(s.Args)-1]
	for _, raw := range urls {
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("invalid repository URL %q", raw)
		}
	}
	dest, err := filepath.Abs(s.Args[len(s.Args)-1])
	if err != nil {
		return nil, err
	}
	if s.Trim < 0 {
		return nil, errors.New("trim must not be negative")
	}
	if s.Depth < 0 {
		return nil, errors.New("depth must not be negative")
	}
	if s.PruneGrace < 0 {
		return nil, errors.New("prune grace must not be negative")
	}

	// Reject unusable globs up front: a malformed pattern would otherwise
	// silently match nothing, which reads as a repository that vanished.
	for _, pattern := range slices.Concat(s.Exclude, s.IncludeFile) {
		if err := mirror.ValidPattern(pattern); err != nil {
			return nil, fmt.Errorf("invalid pattern %q: %w", pattern, err)
		}
	}
	// The patterns describe a crawl, so without one they would do nothing.
	if !s.Discover && (len(s.Exclude) > 0 || len(s.IncludeFile) > 0) {
		return nil, errors.New("--exclude and --include-file apply to discovery; add --discover")
	}

	// Unset worker and grace flags fall back to the crawler configuration,
	// so a config file tunes the sync commands the same way it tunes the
	// server's background crawls.
	if s.Workers == 0 {
		s.Workers = cfg.C.Crawler.Workers
	}
	if s.PruneGrace == 0 {
		s.PruneGrace = cfg.C.Crawler.PruneGrace
	}
	if s.Missing == "" {
		s.Missing = cfg.C.Crawler.MissingMode
	}
	if s.MissingRetries < 0 {
		s.MissingRetries = cfg.C.Crawler.MissingRetries
	}
	if s.DiscoverCache < 0 {
		s.DiscoverCache = cfg.C.Crawler.DiscoverCache
	}
	missing, err := fetch.ParseMissingMode(s.Missing)
	if err != nil {
		return nil, err
	}
	opts := &mirror.Options{
		Types:          types,
		URLs:           urls,
		Destination:    dest,
		Trim:           s.Trim,
		Flat:           s.Flat,
		Discover:       s.Discover,
		DiscoverDepth:  s.Depth,
		DiscoverCache:  s.DiscoverCache,
		Exclude:        s.Exclude,
		IncludeFiles:   s.IncludeFile,
		Workers:        s.Workers,
		Verify:         s.Verify,
		Prune:          s.Prune,
		PruneGrace:     s.PruneGrace,
		DryRun:         s.DryRun,
		Missing:        missing,
		MissingRetries: s.MissingRetries,
		Trace:          cfg.C.Trace.Enabled,
		TraceInfo: mirror.TraceInfo{
			Host:       cfg.C.Trace.HostName(),
			Maintainer: cfg.C.Trace.Maintainer,
			Sponsor:    cfg.C.Trace.Sponsor,
			Country:    cfg.C.Trace.Country,
			Location:   cfg.C.Trace.Location,
			Throughput: cfg.C.Trace.Throughput,
		},
	}

	// A run limited to one format synchronizes every URL as that format,
	// rather than identifying each from its metadata.
	if len(types) == 1 {
		opts.Type = types[0]
	}
	return opts, nil
}

// run executes a synchronization with signal-aware cancellation, reporting
// what the run transferred once it ends.
func (s *SyncArgs) run(opts *mirror.Options) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	summary, err := mirror.Sync(ctx, opts)
	printSummary(os.Stdout, summary)
	return err
}

// printSummary reports the run totals as "Field: value" lines, in the style
// of rsync's own statistics, so a calling script can act on what changed by
// reading the output. It is written to standard output rather than through
// the logger, which a configuration is free to send elsewhere or format as
// JSON, and it is reported even for a failed run, as a run that failed part
// way through still changed the destination.
func printSummary(w io.Writer, summary *mirror.Summary) {
	if summary == nil {
		return
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "Number of repositories: %d\n", summary.Repositories)
	fmt.Fprintf(w, "Number of failed repositories: %d\n", summary.Failed)

	// A dry run transfers metadata to plan against, so reporting that as
	// files transferred would read as a change. It reports what a real run
	// would have moved instead.
	if summary.DryRun {
		fmt.Fprintf(w, "Dry run: true\n")
		fmt.Fprintf(w, "Number of files to transfer: %d\n", summary.Planned)
		fmt.Fprintf(w, "Total file size to transfer: %d bytes\n", summary.PlannedBytes)
	} else {
		fmt.Fprintf(w, "Number of files transferred: %d\n", summary.Fetched)
		fmt.Fprintf(w, "Number of files pruned: %d\n", summary.Pruned)
		fmt.Fprintf(w, "Total transferred file size: %d bytes\n", summary.FetchedBytes)
	}
	fmt.Fprintf(w, "Number of files unchanged: %d\n", summary.Unchanged)
	fmt.Fprintf(w, "Number of files missing upstream: %d\n", summary.Missing)
	fmt.Fprintf(w, "Repository changed: %t\n", summary.Changed())
}

// DebArgs holds the apt-specific filters, shared by the deb command and the
// type-agnostic sync command, which reaches apt repositories too.
type DebArgs struct {
	Component []string `help:"Limit apt synchronization to these components."`
	Arch      []string `help:"Limit apt synchronization to these architectures; include \"source\" to keep source indexes."`
}

// apply copies the apt filters onto the mirror options.
func (d *DebArgs) apply(opts *mirror.Options) {
	opts.Components = d.Component
	opts.Architectures = d.Arch
}

// parseTypes converts requested type names into repository types, rejecting
// unsupported ones. An empty request accepts every supported format.
func parseTypes(names []string) ([]mirror.RepoType, error) {
	var types []mirror.RepoType
	for _, name := range names {
		typ := mirror.RepoType(strings.ToLower(strings.TrimSpace(name)))
		if !slices.Contains(mirror.AllTypes, typ) {
			return nil, fmt.Errorf("unsupported repository type %q; expected one of %s",
				name, strings.Join(mirror.TypeNames(), ", "))
		}
		if !slices.Contains(types, typ) {
			types = append(types, typ)
		}
	}
	return types, nil
}

// SyncCmd synchronizes repositories of any supported type. Each repository
// is identified by its own metadata, so one run covers an upstream that
// publishes several formats; --type limits which formats are accepted.
type SyncCmd struct {
	Type []string `help:"Repository types to synchronize; repeatable, and defaults to every supported type."`

	DebArgs  `embed:""`
	SyncArgs `embed:""`
}

// Run performs the synchronization.
func (c *SyncCmd) Run() error {
	types, err := parseTypes(c.Type)
	if err != nil {
		return err
	}
	opts, err := c.options(types)
	if err != nil {
		return err
	}
	c.apply(opts)
	return c.run(opts)
}

package mirror

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	cfg "github.com/grmrgecko/repo-sync/config"
	"github.com/grmrgecko/repo-sync/fetch"
	log "github.com/sirupsen/logrus"
)

// TraceDir is the directory, relative to a repository, that trace files are
// published in. Debian archives established the location and downstream
// mirror checkers look for it there.
const TraceDir = "project/trace"

// TraceInfo describes the mirror a trace file is written for. It carries
// the same fields as the configuration's trace section, kept as a separate
// type so callers populate it from wherever they hold those details rather
// than this package taking a configuration struct as part of its API.
type TraceInfo struct {
	Host       string
	Maintainer string
	Sponsor    string
	Country    string
	Location   string
	Throughput string
}

// traceRun records when a repository's synchronization began, and the
// transfer it accounted for, so the trace can report both. A nil run writes
// nothing, which is how tracing stays off without a condition at every call
// site.
type traceRun struct {
	started time.Time
	stats   *fetch.Counters
}

// newTrace starts timing a repository sync when tracing is enabled.
func newTrace(opts *Options) *traceRun {
	if !opts.Trace {
		return nil
	}
	return &traceRun{started: time.Now(), stats: &fetch.Counters{}}
}

// track installs the run's own transfer counters on ctx so the trace
// reports what this synchronization moved rather than what the shared
// collector holds. Without it the mirror server's concurrent crawls would
// report each other's bytes.
func (t *traceRun) track(ctx context.Context) context.Context {
	if t == nil {
		return ctx
	}
	return fetch.WithStats(ctx, t.stats)
}

// publish brings a repository's trace directory up to date: upstream's own
// traces are mirrored first, then this run's trace is written, so a name
// collision resolves in favour of the file describing this mirror.
func (t *traceRun) publish(ctx context.Context, root string, src *fetch.Source, keep *fetch.KeepSet, opts *Options) {
	if t == nil {
		return
	}
	t.mirrorUpstream(ctx, root, src, keep, opts)
	t.write(root, src, keep, opts)
}

// mirrorUpstream copies the traces upstream publishes into this mirror's
// trace directory, so a reader can follow the chain back through every
// mirror to the archive the content came from. Upstream may publish no
// traces at all or may not serve directory indexes, and neither is a
// failure of the synchronization, so problems here are logged and the run
// continues.
func (t *traceRun) mirrorUpstream(ctx context.Context, root string, src *fetch.Source, keep *fetch.KeepSet, opts *Options) {
	base := src.Active()
	if base == "" {
		return
	}
	dirURL := base + "/" + TraceDir + "/"
	listing, err := listDir(ctx, dirURL)
	if err != nil {
		log.WithError(err).WithField("url", dirURL).Debug("Upstream serves no trace directory.")
		return
	}

	// Skip the name this mirror traces under; write publishes that file
	// itself, and upstream's copy of it describes a different mirror.
	own, err := traceName(opts.TraceInfo.Host)
	if err != nil {
		own = ""
	}
	names := make([]string, 0, len(listing.files))
	for name := range listing.files {
		if name == own || name == "." || name == ".." {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)

	dir := filepath.Join(root, filepath.FromSlash(TraceDir))
	var jobs []fetch.Job
	for _, name := range names {
		dst, err := fetch.LocalJoin(dir, name)
		if err != nil {
			log.WithError(err).WithField("name", name).Debug("Skipping upstream trace with an unusable name.")
			continue
		}
		// Traces carry no checksums, so they revalidate against their
		// modification time rather than being re-fetched every run, and a
		// trace withdrawn between the listing and the fetch is not an error.
		jobs = append(jobs, fetch.Job{ReqPath: TraceDir + "/" + name, Dst: dst, Optional: true})
	}
	if len(jobs) == 0 {
		return
	}
	if opts.DryRun {
		fetch.PlanJobs(jobs, opts.Verify, keep)
		return
	}
	if _, err := fetch.Many(ctx, src, jobs, opts.Workers, opts.Verify, keep, nil); err != nil {
		log.WithError(err).WithField("url", dirURL).Warn("Failed to mirror upstream trace files.")
		return
	}
	log.WithFields(log.Fields{"url": dirURL, "traces": len(jobs)}).Debug("Mirrored upstream trace files.")
}

// traceName returns the file name a mirror is traced under. The value
// becomes a file name, so it is reduced to a single path element and
// rejected if nothing usable is left.
func traceName(host string) (string, error) {
	name := filepath.Base(strings.TrimSpace(strings.ReplaceAll(host, "\\", "/")))
	if name == "." || name == ".." || name == "/" || name == "" {
		return "", fmt.Errorf("invalid trace host %q", host)
	}
	return name, nil
}

// write publishes the trace for a finished repository synchronization into
// root, and records it in keep so pruning does not treat the file as stale
// and remove it. On a dry run the file is left untouched but still kept,
// so the run does not report the existing trace as about to be pruned.
//
// A trace describes the mirror rather than the packages, so a failure to
// write one is logged and does not fail the synchronization that produced
// it.
func (t *traceRun) write(root string, src *fetch.Source, keep *fetch.KeepSet, opts *Options) {
	if t == nil {
		return
	}
	name, err := traceName(opts.TraceInfo.Host)
	if err != nil {
		log.WithError(err).Warn("Skipping trace file.")
		return
	}
	dir := filepath.Join(root, filepath.FromSlash(TraceDir))
	file := filepath.Join(dir, name)

	// Keep the trace regardless of whether it is written now, so a dry run
	// does not report an existing trace as stale.
	keep.Add(file)
	if opts.DryRun {
		return
	}

	if err := os.MkdirAll(dir, 0755); err != nil {
		log.WithError(err).WithField("path", dir).Warn("Failed to create trace directory.")
		return
	}
	content := t.render(src, opts)
	// Write through a temporary file so a reader never sees a partial
	// trace, and so an interrupted write cannot truncate the previous one.
	tmp, err := os.CreateTemp(dir, ".trace-*")
	if err != nil {
		log.WithError(err).WithField("path", dir).Warn("Failed to create trace file.")
		return
	}
	tmpName := tmp.Name()
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		log.WithError(err).WithField("path", tmpName).Warn("Failed to write trace file.")
		return
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		log.WithError(err).WithField("path", tmpName).Warn("Failed to write trace file.")
		return
	}
	if err := os.Chmod(tmpName, 0644); err != nil {
		log.WithError(err).WithField("path", tmpName).Debug("Failed to set trace file mode.")
	}
	if err := os.Rename(tmpName, file); err != nil {
		os.Remove(tmpName)
		log.WithError(err).WithField("path", file).Warn("Failed to publish trace file.")
		return
	}
	log.WithField("path", file).Debug("Wrote trace file.")
}

// render builds the trace file's contents. The first line is the plain
// completion date, as the convention expects, followed by one field per
// line. Fields with no configured value are omitted rather than written
// empty.
func (t *traceRun) render(src *fetch.Source, opts *Options) string {
	done := time.Now().UTC()
	var b strings.Builder

	// The opening line matches date(1)'s default UTC output, which is what
	// the convention's readers expect; _2 space-pads the day as it does.
	fmt.Fprintf(&b, "%s\n", done.Format("Mon Jan _2 15:04:05 UTC 2006"))
	fmt.Fprintf(&b, "Date: %s\n", done.Format(time.RFC1123Z))
	fmt.Fprintf(&b, "Date-Started: %s\n", t.started.UTC().Format(time.RFC1123Z))
	fmt.Fprintf(&b, "Creator: %s %s\n", cfg.Name, cfg.Version)
	fmt.Fprintf(&b, "Running on host: %s\n", opts.TraceInfo.Host)

	for _, f := range []struct{ key, value string }{
		{"Maintainer", opts.TraceInfo.Maintainer},
		{"Sponsor", opts.TraceInfo.Sponsor},
		{"Country", opts.TraceInfo.Country},
		{"Location", opts.TraceInfo.Location},
		{"Throughput", opts.TraceInfo.Throughput},
	} {
		if f.value != "" {
			fmt.Fprintf(&b, "%s: %s\n", f.key, f.value)
		}
	}

	fmt.Fprintf(&b, "Repository type: %s\n", opts.Type)
	if arch := traceArchitectures(opts); arch != "" {
		fmt.Fprintf(&b, "Architectures: %s\n", arch)
	}

	// Report the mirror actually fetched from. For a mirrorlist or metalink
	// that is the mirror that answered, which is the useful fact and is not
	// derivable from the configured URL.
	if src != nil {
		if active := src.Active(); active != "" {
			fmt.Fprintf(&b, "Upstream-mirror: %s\n", active)
		}
	}

	elapsed := done.Sub(t.started.UTC())
	if elapsed < 0 {
		elapsed = 0
	}
	seconds := int64(elapsed.Round(time.Second) / time.Second)
	// The run's own counters, not the shared collector, so a trace written
	// by one of the server's concurrent crawls reports that crawl alone.
	bytes := t.stats.FetchedBytes.Load()
	fmt.Fprintf(&b, "Total bytes received: %d\n", bytes)
	fmt.Fprintf(&b, "Total time spent syncing: %d\n", seconds)
	if seconds > 0 {
		fmt.Fprintf(&b, "Average rate: %d B/s\n", bytes/seconds)
	}
	return b.String()
}

// traceArchitectures reports the architectures a run was restricted to,
// which only apt repositories can express. An unrestricted run mirrors
// whatever upstream publishes, so no claim is made.
func traceArchitectures(opts *Options) string {
	if opts.Type != RepoDeb || len(opts.Architectures) == 0 {
		return ""
	}
	arch := append([]string(nil), opts.Architectures...)
	sort.Strings(arch)
	return strings.Join(arch, " ")
}

package mirror

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/grmrgecko/repo-sync/fetch"
	"github.com/grmrgecko/repo-sync/internal/testrepos"
)

// traceOptions builds sync options with tracing enabled and a fixed set of
// mirror details, so tests assert on known values.
func traceOptions(typ RepoType, dest string) *Options {
	return &Options{
		Type:        typ,
		Destination: dest,
		Workers:     2,
		Trace:       true,
		TraceInfo: TraceInfo{
			Host:       "mirror.example.com",
			Maintainer: "Test Maintainer <test@example.com>",
			Sponsor:    "Example Inc",
			Country:    "US",
			Location:   "Texas",
			Throughput: "10Gb",
		},
	}
}

// traceField returns the value following a field prefix in a trace.
func traceField(t *testing.T, trace, prefix string) string {
	t.Helper()
	for _, line := range strings.Split(trace, "\n") {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimPrefix(line, prefix)
		}
	}
	t.Fatalf("trace has no %q field\ngot:\n%s", prefix, trace)
	return ""
}

// readTrace returns the trace file published under root.
func readTrace(t *testing.T, root string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(TraceDir), "mirror.example.com"))
	if err != nil {
		t.Fatal("reading trace:", err)
	}
	return string(data)
}

// TestTraceName verifies a trace host is reduced to a single path element,
// so a configured value cannot direct the file outside the repository.
func TestTraceName(t *testing.T) {
	cases := []struct {
		host string
		want string
		ok   bool
	}{
		{"mirror.example.com", "mirror.example.com", true},
		{"  mirror.example.com  ", "mirror.example.com", true},
		{"../../etc/cron.d/evil", "evil", true},
		{"/etc/passwd", "passwd", true},
		{`..\..\windows`, "windows", true},
		{"sub/dir/host", "host", true},
		{"", "", false},
		{"..", "", false},
		{"/", "", false},
	}
	for _, c := range cases {
		got, err := traceName(c.host)
		if c.ok && err != nil {
			t.Errorf("traceName(%q) unexpected error: %v", c.host, err)
			continue
		}
		if !c.ok {
			if err == nil {
				t.Errorf("traceName(%q) = %q, want error", c.host, got)
			}
			continue
		}
		if got != c.want {
			t.Errorf("traceName(%q) = %q, want %q", c.host, got, c.want)
		}
	}
}

// TestSyncTraceWritten verifies a trace lands in the repository directory
// carrying the configured mirror details and the upstream actually used.
func TestSyncTraceWritten(t *testing.T) {
	www := t.TempDir()
	testrepos.BuildApkRepo(t, filepath.Join(www, "alpine", "v3.24", "community", "x86_64"))
	srv := testrepos.ServeDir(t, www)

	dest := t.TempDir()
	opts := traceOptions(RepoApk, dest)
	repoURL := srv.URL + "/alpine/v3.24/community/x86_64"
	if err := syncOne(context.Background(), repoURL, opts.Type, opts); err != nil {
		t.Fatal(err)
	}

	trace := readTrace(t, filepath.Join(dest, "alpine", "v3.24", "community", "x86_64"))
	for _, want := range []string{
		"Date: ",
		"Date-Started: ",
		"Creator: repo-sync ",
		"Running on host: mirror.example.com",
		"Maintainer: Test Maintainer <test@example.com>",
		"Sponsor: Example Inc",
		"Country: US",
		"Location: Texas",
		"Throughput: 10Gb",
		"Repository type: apk",
		"Upstream-mirror: " + repoURL,
		"Total bytes received: ",
		"Total time spent syncing: ",
	} {
		if !strings.Contains(trace, want) {
			t.Errorf("trace missing %q\ngot:\n%s", want, trace)
		}
	}

	// The first line is the plain completion date the convention expects,
	// carrying no field name of its own and matching date(1)'s UTC output.
	first, _, _ := strings.Cut(trace, "\n")
	if strings.HasPrefix(first, "Date:") {
		t.Errorf("first line should be the bare date, got %q", first)
	}
	const dateLayout = "Mon Jan _2 15:04:05 UTC 2006"
	parsed, err := time.Parse(dateLayout, first)
	if err != nil {
		t.Fatalf("first line %q is not a date(1)-style UTC date: %v", first, err)
	}
	// Parsing alone is too lenient about spacing to catch a malformed day
	// field, so require the line to survive a round trip unchanged.
	if canonical := parsed.Format(dateLayout); canonical != first {
		t.Errorf("first line %q is not canonically formatted, want %q", first, canonical)
	}
}

// TestSyncTraceOmitsUnsetFields verifies fields with no configured value
// are left out rather than written empty.
func TestSyncTraceOmitsUnsetFields(t *testing.T) {
	www := t.TempDir()
	testrepos.BuildApkRepo(t, filepath.Join(www, "repo"))
	srv := testrepos.ServeDir(t, www)

	dest := t.TempDir()
	opts := &Options{
		Type: RepoApk, Destination: dest, Workers: 2, Trace: true,
		TraceInfo: TraceInfo{Host: "mirror.example.com"},
	}
	if err := syncOne(context.Background(), srv.URL+"/repo", opts.Type, opts); err != nil {
		t.Fatal(err)
	}

	trace := readTrace(t, filepath.Join(dest, "repo"))
	for _, absent := range []string{"Maintainer:", "Sponsor:", "Country:", "Location:", "Throughput:"} {
		if strings.Contains(trace, absent) {
			t.Errorf("trace contains unset field %q\ngot:\n%s", absent, trace)
		}
	}
	if !strings.Contains(trace, "Running on host: mirror.example.com") {
		t.Errorf("trace missing host\ngot:\n%s", trace)
	}
}

// TestSyncTraceDisabled verifies no trace directory appears when tracing
// is off, which is the default.
func TestSyncTraceDisabled(t *testing.T) {
	www := t.TempDir()
	testrepos.BuildApkRepo(t, filepath.Join(www, "repo"))
	srv := testrepos.ServeDir(t, www)

	dest := t.TempDir()
	opts := &Options{Type: RepoApk, Destination: dest, Workers: 2}
	if err := syncOne(context.Background(), srv.URL+"/repo", opts.Type, opts); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dest, "repo", "project")); err == nil {
		t.Error("trace written while tracing is disabled")
	}
}

// TestSyncTraceSurvivesPrune verifies a trace is not treated as a stale
// file. A flat layout puts the trace inside the pruned tree itself, which
// is the case that would otherwise delete it on the following sync.
func TestSyncTraceSurvivesPrune(t *testing.T) {
	www := t.TempDir()
	testrepos.BuildApkRepo(t, filepath.Join(www, "repo"))
	srv := testrepos.ServeDir(t, www)

	dest := t.TempDir()
	opts := traceOptions(RepoApk, dest)
	opts.Flat = true
	opts.Prune = true

	// Two syncs: the first writes the trace, the second prunes with that
	// trace already on disk.
	for i := 0; i < 2; i++ {
		if err := syncOne(context.Background(), srv.URL+"/repo", opts.Type, opts); err != nil {
			t.Fatalf("sync %d: %v", i+1, err)
		}
		if _, err := os.Stat(filepath.Join(dest, filepath.FromSlash(TraceDir), "mirror.example.com")); err != nil {
			t.Fatalf("trace missing after sync %d: %v", i+1, err)
		}
	}

	// A stale file beside it is still pruned, confirming pruning ran.
	stale := filepath.Join(dest, "old-1.0-r0.apk")
	if err := os.WriteFile(stale, []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := syncOne(context.Background(), srv.URL+"/repo", opts.Type, opts); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); err == nil {
		t.Error("stale package survived pruning, so pruning did not run")
	}
	if _, err := os.Stat(filepath.Join(dest, filepath.FromSlash(TraceDir), "mirror.example.com")); err != nil {
		t.Error("trace pruned:", err)
	}
}

// TestSyncTraceDryRun verifies a dry run neither writes a trace nor
// reports an existing one as a file it would prune.
func TestSyncTraceDryRun(t *testing.T) {
	www := t.TempDir()
	testrepos.BuildApkRepo(t, filepath.Join(www, "repo"))
	srv := testrepos.ServeDir(t, www)

	dest := t.TempDir()
	opts := traceOptions(RepoApk, dest)
	opts.Flat = true
	opts.DryRun = true
	opts.Prune = true
	if err := syncOne(context.Background(), srv.URL+"/repo", opts.Type, opts); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dest, "project")); err == nil {
		t.Error("dry run wrote a trace")
	}

	// With a trace already on disk, a dry run must not count it as
	// prunable. The file is planted directly so this stands on its own
	// rather than depending on a preceding sync having written one.
	traceFile := filepath.Join(dest, filepath.FromSlash(TraceDir), "mirror.example.com")
	if err := os.MkdirAll(filepath.Dir(traceFile), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(traceFile, []byte("previous trace\n"), 0644); err != nil {
		t.Fatal(err)
	}
	fetch.Stats.Reset()
	if err := syncOne(context.Background(), srv.URL+"/repo", opts.Type, opts); err != nil {
		t.Fatal(err)
	}
	if n := fetch.Stats.Pruned.Load(); n != 0 {
		t.Errorf("dry run would prune %d files, want 0 (the trace must be kept)", n)
	}
	if _, err := os.Stat(traceFile); err != nil {
		t.Error("dry run removed the existing trace:", err)
	}
}

// TestSyncTraceDebAtArchiveRoot verifies an apt trace is published at the
// archive root, where the convention places it, rather than in the suite.
func TestSyncTraceDebAtArchiveRoot(t *testing.T) {
	www := t.TempDir()
	testrepos.BuildDebRepo(t, filepath.Join(www, "debian"))
	srv := testrepos.ServeDir(t, www)

	dest := t.TempDir()
	opts := traceOptions(RepoDeb, dest)
	opts.Architectures = []string{"amd64", "source"}
	if err := syncOne(context.Background(), srv.URL+"/debian/dists/test", opts.Type, opts); err != nil {
		t.Fatal(err)
	}

	root := filepath.Join(dest, "debian")
	trace := readTrace(t, root)
	if !strings.Contains(trace, "Repository type: deb") {
		t.Errorf("trace missing repository type\ngot:\n%s", trace)
	}
	if !strings.Contains(trace, "Architectures: amd64 source") {
		t.Errorf("trace missing architectures\ngot:\n%s", trace)
	}
	if _, err := os.Stat(filepath.Join(root, "dists", "test", "project")); err == nil {
		t.Error("trace written into the suite directory instead of the archive root")
	}
}

// TestSyncIntoWritesTrace verifies the mirror server's entry point
// publishes a trace like the sync commands do, and that it still leaves the
// caller's options untouched.
func TestSyncIntoWritesTrace(t *testing.T) {
	www := t.TempDir()
	testrepos.BuildApkRepo(t, filepath.Join(www, "repo"))
	srv := testrepos.ServeDir(t, www)

	dest := t.TempDir()
	opts := traceOptions(RepoApk, dest)
	src := fetch.NewSource([]string{srv.URL + "/repo"})
	if err := SyncInto(context.Background(), RepoApk, src, srv.URL+"/repo", dest, opts); err != nil {
		t.Fatal(err)
	}
	trace := readTrace(t, dest)
	if !strings.Contains(trace, "Running on host: mirror.example.com") {
		t.Errorf("server trace missing host\ngot:\n%s", trace)
	}
	if !opts.Trace {
		t.Error("SyncInto mutated the caller's options")
	}
}

// TestSyncIntoTraceDisabled verifies the server path honors tracing being
// off, which is the default.
func TestSyncIntoTraceDisabled(t *testing.T) {
	www := t.TempDir()
	testrepos.BuildApkRepo(t, filepath.Join(www, "repo"))
	srv := testrepos.ServeDir(t, www)

	dest := t.TempDir()
	opts := &Options{Type: RepoApk, Destination: dest, Workers: 2}
	src := fetch.NewSource([]string{srv.URL + "/repo"})
	if err := SyncInto(context.Background(), RepoApk, src, srv.URL+"/repo", dest, opts); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dest, "project")); err == nil {
		t.Error("trace written while tracing is disabled")
	}
}

// TestSyncTraceReportsOwnTransfer verifies a trace reports the bytes its own
// synchronization moved rather than whatever the shared collector holds.
// That isolation is what makes a trace meaningful on the server's path,
// where crawls run concurrently and all report into the same collector.
func TestSyncTraceReportsOwnTransfer(t *testing.T) {
	www := t.TempDir()
	testrepos.BuildApkRepo(t, filepath.Join(www, "repo"))
	srv := testrepos.ServeDir(t, www)

	// Stand in for a concurrent crawl's transfer, which must not be
	// attributed to this run.
	const foreign = int64(1) << 40
	fetch.Stats.Reset()
	fetch.Stats.FetchedBytes.Add(foreign)

	dest := t.TempDir()
	opts := traceOptions(RepoApk, dest)
	if err := syncOne(context.Background(), srv.URL+"/repo", opts.Type, opts); err != nil {
		t.Fatal(err)
	}

	got := traceField(t, readTrace(t, filepath.Join(dest, "repo")), "Total bytes received: ")
	bytes, err := strconv.ParseInt(got, 10, 64)
	if err != nil {
		t.Fatalf("byte count %q: %v", got, err)
	}
	if bytes <= 0 {
		t.Errorf("trace reports %d bytes received, want the run's own transfer", bytes)
	}
	if bytes >= foreign {
		t.Errorf("trace reports %d bytes received, which includes another run's transfer", bytes)
	}
}

// TestSyncTraceMirrorsUpstream verifies upstream's own traces are copied
// into this mirror's trace directory, so the chain back to the origin
// archive is preserved, while the file naming this mirror stays the one
// this run wrote.
func TestSyncTraceMirrorsUpstream(t *testing.T) {
	www := t.TempDir()
	repo := filepath.Join(www, "repo")
	testrepos.BuildApkRepo(t, repo)
	upstreamTrace := filepath.Join(repo, filepath.FromSlash(TraceDir))
	testrepos.WriteFile(t, filepath.Join(upstreamTrace, "origin.example.net"), []byte("origin trace\n"))
	testrepos.WriteFile(t, filepath.Join(upstreamTrace, "middle.example.net"), []byte("middle trace\n"))
	// Upstream also carries a trace under this mirror's own name, which
	// describes a different mirror and must not replace ours.
	testrepos.WriteFile(t, filepath.Join(upstreamTrace, "mirror.example.com"), []byte("someone else\n"))
	srv := testrepos.ServeDir(t, www)

	dest := t.TempDir()
	opts := traceOptions(RepoApk, dest)
	opts.Prune = true
	if err := syncOne(context.Background(), srv.URL+"/repo", opts.Type, opts); err != nil {
		t.Fatal(err)
	}

	traceDir := filepath.Join(dest, "repo", filepath.FromSlash(TraceDir))
	for name, want := range map[string]string{
		"origin.example.net": "origin trace\n",
		"middle.example.net": "middle trace\n",
	} {
		data, err := os.ReadFile(filepath.Join(traceDir, name))
		if err != nil {
			t.Errorf("upstream trace %s not mirrored: %v", name, err)
			continue
		}
		if string(data) != want {
			t.Errorf("upstream trace %s = %q, want %q", name, data, want)
		}
	}
	if trace := readTrace(t, filepath.Join(dest, "repo")); !strings.Contains(trace, "Running on host: mirror.example.com") {
		t.Errorf("upstream trace replaced this mirror's own\ngot:\n%s", trace)
	}

	// A second sync prunes with the mirrored traces already on disk; they
	// belong to the repository and must survive.
	if err := syncOne(context.Background(), srv.URL+"/repo", opts.Type, opts); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(traceDir, "origin.example.net")); err != nil {
		t.Error("mirrored upstream trace pruned:", err)
	}
}

// TestSyncTraceUpstreamAbsent verifies an upstream that publishes no traces
// is not a synchronization failure.
func TestSyncTraceUpstreamAbsent(t *testing.T) {
	www := t.TempDir()
	testrepos.BuildApkRepo(t, filepath.Join(www, "repo"))
	srv := testrepos.ServeDir(t, www)

	dest := t.TempDir()
	opts := traceOptions(RepoApk, dest)
	if err := syncOne(context.Background(), srv.URL+"/repo", opts.Type, opts); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dest, "repo", filepath.FromSlash(TraceDir), "mirror.example.com")); err != nil {
		t.Error("trace missing:", err)
	}
}

// TestTraceArchitectures verifies architectures are only claimed for apt
// runs that were actually restricted to them.
func TestTraceArchitectures(t *testing.T) {
	if got := traceArchitectures(&Options{Type: RepoDeb, Architectures: []string{"source", "amd64"}}); got != "amd64 source" {
		t.Errorf("deb architectures = %q, want %q", got, "amd64 source")
	}
	if got := traceArchitectures(&Options{Type: RepoDeb}); got != "" {
		t.Errorf("unrestricted deb architectures = %q, want empty", got)
	}
	if got := traceArchitectures(&Options{Type: RepoRPM, Architectures: []string{"x86_64"}}); got != "" {
		t.Errorf("rpm architectures = %q, want empty", got)
	}
}

package mirror

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/grmrgecko/repo-sync/fetch"
	"github.com/grmrgecko/repo-sync/internal/testrepos"
)

// countFiles returns the number of regular files under root.
func countFiles(t *testing.T, root string) int {
	t.Helper()
	count := 0
	err := filepath.WalkDir(root, func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			count++
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return count
}

// TestDryRun verifies a dry run leaves the destination unchanged: no
// packages downloaded, no metadata published, and no files pruned.
func TestDryRun(t *testing.T) {
	www := t.TempDir()
	testrepos.BuildRPMRepo(t, filepath.Join(www, "el9"))
	srv := testrepos.ServeDir(t, www)

	// A dry run against an empty destination writes no files at all.
	dest := t.TempDir()
	opts := &Options{Type: RepoRPM, Destination: dest, Workers: 2, DryRun: true, Prune: true}
	if err := syncOne(context.Background(), srv.URL+"/el9", opts.Type, opts); err != nil {
		t.Fatal(err)
	}
	if n := countFiles(t, dest); n != 0 {
		t.Errorf("dry run left %d files in the destination", n)
	}

	// A real run works, and a later dry run with pruning keeps stale files.
	opts.DryRun = false
	if err := syncOne(context.Background(), srv.URL+"/el9", opts.Type, opts); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dest, "el9", "Packages", "stale.rpm")
	if err := os.WriteFile(stale, []byte("junk"), 0644); err != nil {
		t.Fatal(err)
	}
	opts.DryRun = true
	if err := syncOne(context.Background(), srv.URL+"/el9", opts.Type, opts); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); err != nil {
		t.Error("dry run pruned a file:", err)
	}
}

// TestPruneGrace verifies stale files wait out the grace period across
// runs before being removed.
func TestPruneGrace(t *testing.T) {
	www := t.TempDir()
	testrepos.BuildRPMRepo(t, filepath.Join(www, "el9"))
	srv := testrepos.ServeDir(t, www)

	dest := t.TempDir()
	opts := &Options{Type: RepoRPM, Destination: dest, Workers: 2, Prune: true, PruneGrace: time.Hour}
	if err := syncOne(context.Background(), srv.URL+"/el9", opts.Type, opts); err != nil {
		t.Fatal(err)
	}

	local := filepath.Join(dest, "el9")
	stale := filepath.Join(local, "Packages", "stale.rpm")
	if err := os.WriteFile(stale, []byte("junk"), 0644); err != nil {
		t.Fatal(err)
	}

	// The first run only starts the grace period.
	if err := syncOne(context.Background(), srv.URL+"/el9", opts.Type, opts); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); err != nil {
		t.Fatal("file pruned before its grace period expired:", err)
	}
	statePath := filepath.Join(local, fetch.PruneStateName)
	if _, err := os.Stat(statePath); err != nil {
		t.Fatal("prune state not recorded:", err)
	}

	// A second run within the grace period still keeps the file.
	if err := syncOne(context.Background(), srv.URL+"/el9", opts.Type, opts); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); err != nil {
		t.Fatal("file pruned within its grace period:", err)
	}

	// Age the recorded timestamp past the grace period; the next run
	// prunes the file and clears the bookkeeping.
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	state := map[string]time.Time{}
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	for rel := range state {
		state[rel] = time.Now().Add(-2 * time.Hour)
	}
	data, err = json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, data, 0644); err != nil {
		t.Fatal(err)
	}
	if err := syncOne(context.Background(), srv.URL+"/el9", opts.Type, opts); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); err == nil {
		t.Error("stale file survived past its grace period")
	}
	if _, err := os.Stat(statePath); err == nil {
		t.Error("prune state lingered after all grace periods resolved")
	}
}

// TestLockDestination verifies a held destination lock blocks a second run,
// is released on release, and leaves no lock file behind in the tree.
func TestLockDestination(t *testing.T) {
	dest := t.TempDir()
	lock, err := lockDestination(dest)
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(dest, fetch.LockFileName)
	if _, err := os.Stat(name); err != nil {
		t.Error("no lock file while the lock is held:", err)
	}
	if _, err := lockDestination(dest); err == nil {
		t.Error("second lock unexpectedly succeeded")
	}

	lock.release()
	if _, err := os.Stat(name); !os.IsNotExist(err) {
		t.Error("lock file survived the run that took it")
	}
	lock, err = lockDestination(dest)
	if err != nil {
		t.Error("lock not released:", err)
	} else {
		lock.release()
	}
}

// TestLockDestinationReplaced verifies a lock taken on a file that is no
// longer the one at the path is not accepted: a run that released between
// the open and the lock takes its file with it, and the lock left over
// guards nothing the next run would consult.
func TestLockDestinationReplaced(t *testing.T) {
	dest := t.TempDir()
	name := filepath.Join(dest, fetch.LockFileName)

	// Stand in for the released run by replacing the lock file with a new
	// one, which is the state a racing run would find.
	stale, err := os.OpenFile(name, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		t.Fatal(err)
	}
	defer stale.Close()
	if err := os.Remove(name); err != nil {
		t.Fatal(err)
	}
	replacement, err := os.OpenFile(name, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		t.Fatal(err)
	}
	replacement.Close()

	lock, err := lockDestination(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.release()
	held, err := lock.file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	current, err := os.Stat(name)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(held, current) {
		t.Error("locked a file that is no longer the destination's lock file")
	}
}

// TestSyncSummary verifies a run reports what it changed: the first run
// fetches, a repeat run finds everything unchanged, and a removed upstream
// file is counted as pruned.
func TestSyncSummary(t *testing.T) {
	www := t.TempDir()
	testrepos.BuildRPMRepo(t, filepath.Join(www, "el9"))
	srv := testrepos.ServeDir(t, www)

	dest := t.TempDir()
	opts := &Options{
		Type:        RepoRPM,
		URLs:        []string{srv.URL + "/el9"},
		Destination: dest,
		Workers:     2,
		Prune:       true,
	}

	// The first run populates the destination, so everything it fetched is a
	// change.
	summary, err := Sync(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Repositories != 1 || summary.Failed != 0 {
		t.Errorf("repositories = %d, failed = %d, want 1 and 0", summary.Repositories, summary.Failed)
	}
	if summary.Fetched == 0 {
		t.Error("first run reported nothing fetched")
	}
	if summary.FetchedBytes == 0 {
		t.Error("first run reported no bytes fetched")
	}
	if !summary.Changed() {
		t.Error("first run did not report a change")
	}

	// Repeating the run transfers nothing, which is what a hook keys off to
	// stay idle.
	summary, err = Sync(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Fetched != 0 || summary.Pruned != 0 {
		t.Errorf("repeat run fetched %d and pruned %d, want 0 and 0", summary.Fetched, summary.Pruned)
	}
	if summary.Unchanged == 0 {
		t.Error("repeat run reported nothing unchanged")
	}
	if summary.Changed() {
		t.Error("repeat run reported a change")
	}

	// A local file the upstream never served is pruned, which counts as a
	// change even though nothing was fetched.
	stale := filepath.Join(dest, "el9", "Packages", "stale.rpm")
	if err := os.WriteFile(stale, []byte("junk"), 0644); err != nil {
		t.Fatal(err)
	}
	summary, err = Sync(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Pruned != 1 {
		t.Errorf("pruned = %d, want 1", summary.Pruned)
	}
	if !summary.Changed() {
		t.Error("pruning run did not report a change")
	}
}

// TestSyncSummaryDryRun verifies a dry run reports its planned work without
// ever claiming the mirror changed.
func TestSyncSummaryDryRun(t *testing.T) {
	www := t.TempDir()
	testrepos.BuildRPMRepo(t, filepath.Join(www, "el9"))
	srv := testrepos.ServeDir(t, www)

	opts := &Options{
		Type:        RepoRPM,
		URLs:        []string{srv.URL + "/el9"},
		Destination: t.TempDir(),
		Workers:     2,
		DryRun:      true,
	}
	summary, err := Sync(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Planned == 0 {
		t.Error("dry run planned nothing")
	}
	if summary.PlannedBytes == 0 {
		t.Error("dry run planned no bytes")
	}
	// A dry run still fetches metadata to plan against, but none of it
	// reaches the destination, so the run must never read as a change.
	if summary.Changed() {
		t.Error("dry run reported a change")
	}
}

// TestSyncSummaryFailure verifies a run that fails still reports the totals
// it accumulated, so a caller can tell a failed run apart from an idle one.
func TestSyncSummaryFailure(t *testing.T) {
	www := t.TempDir()
	srv := testrepos.ServeDir(t, www)

	opts := &Options{
		Type:        RepoRPM,
		URLs:        []string{srv.URL + "/missing"},
		Destination: t.TempDir(),
		Workers:     2,
	}
	summary, err := Sync(context.Background(), opts)
	if err == nil {
		t.Fatal("expected a failure syncing a repository that does not exist")
	}
	if summary == nil {
		t.Fatal("no summary returned for a failed run")
	}
	if summary.Repositories != 1 || summary.Failed != 1 {
		t.Errorf("repositories = %d, failed = %d, want 1 and 1", summary.Repositories, summary.Failed)
	}
	if summary.Changed() {
		t.Error("a run that transferred nothing reported a change")
	}
}

// TestSyncDiscoverMixed verifies one run covers an upstream publishing more
// than one repository format, skips the trees it was told to exclude, and
// mirrors the loose files it was told to include beside the repositories.
func TestSyncDiscoverMixed(t *testing.T) {
	www := t.TempDir()
	testrepos.BuildRPMRepo(t, filepath.Join(www, "yum", "el9"))
	testrepos.BuildDebRepo(t, filepath.Join(www, "apt"))
	testrepos.BuildRPMRepo(t, filepath.Join(www, "yum", "sles15"))
	testrepos.WriteFile(t, filepath.Join(www, "RPM-GPG-KEY-test"), []byte("key"))
	testrepos.WriteFile(t, filepath.Join(www, "yum", "loose-1.0.rpm"), []byte("loose"))
	testrepos.WriteFile(t, filepath.Join(www, "yum", "notes.txt"), []byte("ignored"))
	srv := testrepos.ServeDir(t, www)

	dest := t.TempDir()
	opts := &Options{
		URLs:          []string{srv.URL},
		Destination:   dest,
		Discover:      true,
		DiscoverDepth: 4,
		Exclude:       []string{"sles*"},
		IncludeFiles:  []string{"*.rpm", "RPM-GPG-KEY-*"},
		Workers:       2,
	}
	summary, err := Sync(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Repositories != 2 || summary.Failed != 0 {
		t.Errorf("repositories = %d, failed = %d, want 2 and 0", summary.Repositories, summary.Failed)
	}

	// Both formats landed, each under its own upstream path.
	for _, rel := range []string{
		filepath.Join("yum", "el9", "repodata", "repomd.xml"),
		filepath.Join("apt", "dists", "test", "Release"),
		filepath.Join("apt", "pool", "main", "h", "hello", "hello_1.0_amd64.deb"),
		"RPM-GPG-KEY-test",
		filepath.Join("yum", "loose-1.0.rpm"),
	} {
		if _, err := os.Stat(filepath.Join(dest, rel)); err != nil {
			t.Errorf("missing %s: %v", rel, err)
		}
	}

	// The excluded tree was never crawled, and a file no pattern matched
	// was never fetched.
	for _, rel := range []string{
		filepath.Join("yum", "sles15"),
		filepath.Join("yum", "notes.txt"),
	} {
		if _, err := os.Stat(filepath.Join(dest, rel)); err == nil {
			t.Errorf("%s was synchronized despite not being asked for", rel)
		}
	}
}

// TestSyncDetectType verifies a repository URL given without a type is
// synchronized as whatever its metadata identifies it to be.
func TestSyncDetectType(t *testing.T) {
	www := t.TempDir()
	testrepos.BuildRPMRepo(t, filepath.Join(www, "el9"))
	srv := testrepos.ServeDir(t, www)

	dest := t.TempDir()
	summary, err := Sync(context.Background(), &Options{
		URLs:        []string{srv.URL + "/el9"},
		Destination: dest,
		Workers:     2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Repositories != 1 || summary.Failed != 0 {
		t.Errorf("repositories = %d, failed = %d, want 1 and 0", summary.Repositories, summary.Failed)
	}
	if _, err := os.Stat(filepath.Join(dest, "el9", "repodata", "repomd.xml")); err != nil {
		t.Error("repository not synchronized:", err)
	}
}

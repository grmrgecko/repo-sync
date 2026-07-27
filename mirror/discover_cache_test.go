package mirror

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/grmrgecko/repo-sync/fetch"
	"github.com/grmrgecko/repo-sync/internal/testrepos"
)

// countingArchive serves a fixture tree, counting the directory listings a
// crawl requests so a test can tell a fresh scan from a reused one.
func countingArchive(t *testing.T, www string) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var listings atomic.Int64
	files := http.FileServer(http.Dir(www))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/") {
			listings.Add(1)
		}
		files.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &listings
}

// readDiscoverState reads the discovery cache a run left at a destination.
func readDiscoverState(t *testing.T, dest string) discoverState {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dest, fetch.DiscoverStateName))
	if err != nil {
		t.Fatal(err)
	}
	var state discoverState
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	return state
}

// TestDiscoverCacheReuse verifies a crawl's results are reused until they
// age out, so a repeat run costs no directory listings, and that scanning
// again picks up what the upstream has published since.
func TestDiscoverCacheReuse(t *testing.T) {
	www := t.TempDir()
	testrepos.BuildRPMRepo(t, filepath.Join(www, "el9"))
	srv, listings := countingArchive(t, www)

	dest := t.TempDir()
	opts := &Options{
		URLs:          []string{srv.URL},
		Destination:   dest,
		Discover:      true,
		DiscoverDepth: 3,
		DiscoverCache: time.Hour,
		Workers:       2,
	}

	// The first run scans the tree and records what it found.
	summary, err := Sync(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Repositories != 1 {
		t.Fatalf("repositories = %d, want 1", summary.Repositories)
	}
	scanned := listings.Load()
	if scanned == 0 {
		t.Fatal("the first run listed no directories")
	}
	state := readDiscoverState(t, dest)
	rec := state.Crawls[crawlKey(srv.URL)]
	if rec == nil || len(rec.Repos) != 1 || rec.Repos[0].Type != string(RepoRPM) {
		t.Fatalf("recorded crawl = %+v", rec)
	}

	// A repository published after that scan is not picked up while the
	// cached results are still current, and the tree is not listed again.
	testrepos.BuildRPMRepo(t, filepath.Join(www, "el10"))
	summary, err = Sync(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Repositories != 1 {
		t.Errorf("repositories = %d on the cached run, want 1", summary.Repositories)
	}
	if got := listings.Load(); got != scanned {
		t.Errorf("the cached run listed %d directories", got-scanned)
	}
	if _, err := os.Stat(filepath.Join(dest, "el10")); err == nil {
		t.Error("a repository published after the scan was mirrored from the cache")
	}

	// Scanning on every run finds it.
	opts.DiscoverCache = 0
	summary, err = Sync(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Repositories != 2 {
		t.Errorf("repositories = %d after re-scanning, want 2", summary.Repositories)
	}
	if _, err := os.Stat(filepath.Join(dest, "el10", "repodata", "repomd.xml")); err != nil {
		t.Error("the newly published repository was not mirrored:", err)
	}
}

// TestDiscoverCacheOptionsChange verifies a run that changes what a crawl
// would find scans again rather than replaying the previous results.
func TestDiscoverCacheOptionsChange(t *testing.T) {
	www := t.TempDir()
	testrepos.BuildRPMRepo(t, filepath.Join(www, "el9"))
	testrepos.BuildRPMRepo(t, filepath.Join(www, "sles15"))
	srv, _ := countingArchive(t, www)

	dest := t.TempDir()
	opts := &Options{
		URLs:          []string{srv.URL},
		Destination:   dest,
		Discover:      true,
		DiscoverDepth: 3,
		DiscoverCache: time.Hour,
		Workers:       2,
	}
	summary, err := Sync(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Repositories != 2 {
		t.Fatalf("repositories = %d, want 2", summary.Repositories)
	}

	// The cache is still current, but the run no longer asks for the same
	// tree, so the crawl has to happen again.
	opts.Exclude = []string{"sles*"}
	summary, err = Sync(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Repositories != 1 {
		t.Errorf("repositories = %d after excluding one, want 1", summary.Repositories)
	}
}

// TestDiscoverCacheEviction verifies a repository the upstream withdrew is
// removed from the mirror when pruning is enabled, and kept otherwise.
func TestDiscoverCacheEviction(t *testing.T) {
	www := t.TempDir()
	testrepos.BuildRPMRepo(t, filepath.Join(www, "el9"))
	testrepos.BuildRPMRepo(t, filepath.Join(www, "el10"))
	srv, _ := countingArchive(t, www)

	dest := t.TempDir()
	opts := &Options{
		URLs:          []string{srv.URL},
		Destination:   dest,
		Discover:      true,
		DiscoverDepth: 3,
		Workers:       2,
	}
	if _, err := Sync(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dest, "el10", "repodata", "repomd.xml")); err != nil {
		t.Fatal("second repository not mirrored:", err)
	}

	// Withdraw it upstream. Without pruning the local copy stays, but the
	// crawl records that it is gone.
	if err := os.RemoveAll(filepath.Join(www, "el10")); err != nil {
		t.Fatal(err)
	}
	if _, err := Sync(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dest, "el10")); err != nil {
		t.Error("a withdrawn repository was removed without pruning:", err)
	}

	// With pruning it is evicted, and the record goes with it.
	opts.Prune = true
	if _, err := Sync(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dest, "el10")); !os.IsNotExist(err) {
		t.Error("a withdrawn repository survived a pruning run")
	}
	if _, err := os.Stat(filepath.Join(dest, "el9", "repodata", "repomd.xml")); err != nil {
		t.Error("the surviving repository was evicted too:", err)
	}
	for _, repo := range readDiscoverState(t, dest).Crawls[crawlKey(srv.URL)].Repos {
		if strings.HasSuffix(repo.URL, "/el10") {
			t.Error("the evicted repository is still recorded")
		}
	}
}

// TestDiscoverCacheEvictionGrace verifies a withdrawn repository waits out
// the prune grace before it is evicted, so one bad crawl does not take a
// tree with it.
func TestDiscoverCacheEvictionGrace(t *testing.T) {
	www := t.TempDir()
	testrepos.BuildRPMRepo(t, filepath.Join(www, "el9"))
	testrepos.BuildRPMRepo(t, filepath.Join(www, "el10"))
	srv, _ := countingArchive(t, www)

	dest := t.TempDir()
	opts := &Options{
		URLs:          []string{srv.URL},
		Destination:   dest,
		Discover:      true,
		DiscoverDepth: 3,
		Workers:       2,
		Prune:         true,
		PruneGrace:    time.Hour,
	}
	if _, err := Sync(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(www, "el10")); err != nil {
		t.Fatal(err)
	}
	if _, err := Sync(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dest, "el10")); err != nil {
		t.Error("a withdrawn repository was evicted inside its grace period:", err)
	}

	// Age the recorded withdrawal past the grace period; the next run
	// evicts it.
	state := readDiscoverState(t, dest)
	aged := time.Now().Add(-2 * time.Hour)
	for _, repo := range state.Crawls[crawlKey(srv.URL)].Repos {
		if strings.HasSuffix(repo.URL, "/el10") {
			if repo.Gone == nil {
				t.Fatal("the withdrawal was not recorded")
			}
			repo.Gone = &aged
		}
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest, fetch.DiscoverStateName), data, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Sync(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dest, "el10")); !os.IsNotExist(err) {
		t.Error("a withdrawn repository survived its grace period")
	}
}

// TestDiscoverCacheEvictionNeedsCleanCrawl verifies a crawl that could not
// list part of the tree withdraws nothing: an upstream that failed to
// answer is not an upstream that dropped a repository.
func TestDiscoverCacheEvictionNeedsCleanCrawl(t *testing.T) {
	www := t.TempDir()
	testrepos.BuildRPMRepo(t, filepath.Join(www, "el9"))
	testrepos.BuildRPMRepo(t, filepath.Join(www, "el10"))

	// The second run answers the el10 listing with an error, which is how a
	// repository that is still published can go missing from a crawl.
	var broken atomic.Bool
	files := http.FileServer(http.Dir(www))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if broken.Load() && strings.HasPrefix(r.URL.Path, "/el10") {
			http.Error(w, "upstream is having a bad day", http.StatusInternalServerError)
			return
		}
		files.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	dest := t.TempDir()
	opts := &Options{
		URLs:          []string{srv.URL},
		Destination:   dest,
		Discover:      true,
		DiscoverDepth: 3,
		Workers:       2,
		Prune:         true,
	}
	if _, err := Sync(context.Background(), opts); err != nil {
		t.Fatal(err)
	}

	broken.Store(true)
	if _, err := Sync(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dest, "el10", "repodata", "repomd.xml")); err != nil {
		t.Error("a failed listing evicted a repository that is still published:", err)
	}
	for _, repo := range readDiscoverState(t, dest).Crawls[crawlKey(srv.URL)].Repos {
		if strings.HasSuffix(repo.URL, "/el10") && repo.Gone != nil {
			t.Error("a failed listing recorded a repository as withdrawn")
		}
	}
}

// TestDiscoverCacheDryRun verifies a planning run neither records a crawl
// nor spends the cache's lifetime.
func TestDiscoverCacheDryRun(t *testing.T) {
	www := t.TempDir()
	testrepos.BuildRPMRepo(t, filepath.Join(www, "el9"))
	srv, _ := countingArchive(t, www)

	dest := t.TempDir()
	opts := &Options{
		URLs:          []string{srv.URL},
		Destination:   dest,
		Discover:      true,
		DiscoverDepth: 3,
		DiscoverCache: time.Hour,
		Workers:       2,
		DryRun:        true,
	}
	if _, err := Sync(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dest, fetch.DiscoverStateName)); !os.IsNotExist(err) {
		t.Error("a dry run recorded a crawl")
	}
}

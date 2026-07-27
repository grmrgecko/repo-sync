package server

import (
	"testing"
	"time"

	"github.com/grmrgecko/repo-sync/state"
)

// TestClassifyRequest verifies request paths map to the right resource
// kinds, crawl paths, and keep-alive roots.
func TestClassifyRequest(t *testing.T) {
	cases := []struct {
		reqPath string
		kind    string
		path    string
		root    string
	}{
		{"/almalinux/9/BaseOS/x86_64/os/repodata/repomd.xml", "rpm", "/almalinux/9/BaseOS/x86_64/os", "/almalinux/9/BaseOS/x86_64/os"},
		{"/almalinux/9/BaseOS/x86_64/os/repodata/abc-primary.xml.gz", "rpm", "/almalinux/9/BaseOS/x86_64/os", "/almalinux/9/BaseOS/x86_64/os"},
		{"/debian/dists/bookworm/InRelease", "deb", "/debian/dists/bookworm", "/debian"},
		{"/debian/dists/stable/updates/Release", "deb", "/debian/dists/stable/updates", "/debian"},
		{"/flat/Release", "deb", "/flat", "/flat"},
		{"/archlinux/core/os/x86_64/core.db", "arch", "/archlinux/core/os/x86_64", "/archlinux/core/os/x86_64"},
		{"/alpine/v3.24/main/x86_64/APKINDEX.tar.gz", "apk", "/alpine/v3.24/main/x86_64", "/alpine/v3.24/main/x86_64"},
		{"/almalinux/9/BaseOS/x86_64/os/Packages/foo.rpm", kindGeneric, "/almalinux/9/BaseOS/x86_64/os/Packages/foo.rpm", ""},
		{"/notes/README.txt", kindGeneric, "/notes/README.txt", ""},
	}
	for _, c := range cases {
		res := classifyRequest(c.reqPath)
		if res.Kind != c.kind || res.Path != c.path || res.Root != c.root {
			t.Errorf("classifyRequest(%q) = {%s %s %s}, want {%s %s %s}",
				c.reqPath, res.Kind, res.Path, res.Root, c.kind, c.path, c.root)
		}
	}
}

// TestRefreshTier verifies the tier walk and staleness budget.
func TestRefreshTier(t *testing.T) {
	schedule := []time.Duration{4 * time.Hour, 12 * time.Hour, 24 * time.Hour}
	if interval, stale := refreshTier(time.Hour, schedule); stale || interval != 4*time.Hour {
		t.Errorf("fresh resource tier = %v, %v", interval, stale)
	}
	if interval, stale := refreshTier(10*time.Hour, schedule); stale || interval != 12*time.Hour {
		t.Errorf("aging resource tier = %v, %v", interval, stale)
	}
	if _, stale := refreshTier(41*time.Hour, schedule); !stale {
		t.Error("resource past the budget not stale")
	}
}

// TestNextCrawlAt verifies crawl gating for fresh, failed, and deferred
// entries.
func TestNextCrawlAt(t *testing.T) {
	now := time.Now()
	if !nextCrawlAt(state.Entry{}, time.Hour).IsZero() {
		t.Error("never-crawled entry not due immediately")
	}
	failedAt := now.Add(30 * time.Minute)
	failed := state.Entry{LastError: "boom", LastCrawled: now, NextCrawl: failedAt}
	if got := nextCrawlAt(failed, time.Hour); !got.Equal(failedAt) {
		t.Errorf("failed entry gate = %v, want %v", got, failedAt)
	}
	ok := state.Entry{LastCrawled: now, NextCrawl: now.Add(10 * time.Minute)}
	if got := nextCrawlAt(ok, 2*time.Hour); !got.Equal(now.Add(2 * time.Hour)) {
		t.Errorf("tier gate = %v, want %v", got, now.Add(2*time.Hour))
	}
}

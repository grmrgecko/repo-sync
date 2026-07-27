package mirror

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grmrgecko/repo-sync/fetch"
	"github.com/grmrgecko/repo-sync/internal/testrepos"
)

// syncRPMFixture builds a served RPM repository, synchronizes it, and
// returns the fixture directory, repository URL, and local repository path.
func syncRPMFixture(t *testing.T, opts *Options) (string, string, string, map[string][]byte) {
	t.Helper()
	www := t.TempDir()
	repoDir := filepath.Join(www, "repos", "el9")
	pkgs := testrepos.BuildRPMRepo(t, repoDir)
	srv := testrepos.ServeDir(t, www)
	repoURL := srv.URL + "/repos/el9"

	opts.Type = RepoRPM
	if opts.Destination == "" {
		opts.Destination = t.TempDir()
	}
	if opts.Workers == 0 {
		opts.Workers = 2
	}
	if err := syncOne(context.Background(), repoURL, opts.Type, opts); err != nil {
		t.Fatal(err)
	}
	return repoDir, repoURL, filepath.Join(opts.Destination, "repos", "el9"), pkgs
}

// TestSyncRPM verifies a full synchronization mirrors packages and metadata
// with no staged leftovers.
func TestSyncRPM(t *testing.T) {
	_, _, local, pkgs := syncRPMFixture(t, &Options{})
	for name, data := range pkgs {
		got, err := os.ReadFile(filepath.Join(local, "Packages", name))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, data) {
			t.Errorf("package %s content mismatch", name)
		}
	}
	if _, err := os.Stat(filepath.Join(local, "repodata", "repomd.xml")); err != nil {
		t.Error("repomd.xml not promoted:", err)
	}
	if _, err := os.Stat(filepath.Join(local, "repodata", "repomd.xml.asc")); err != nil {
		t.Error("repomd.xml.asc not promoted:", err)
	}
	drpm, err := os.ReadFile(filepath.Join(local, "drpms", "foo-0.9_1.0-1.x86_64.drpm"))
	if err != nil {
		t.Error("delta package not mirrored:", err)
	} else if !bytes.Equal(drpm, testrepos.RPMDelta()) {
		t.Error("delta package content mismatch")
	}
	staged, _ := filepath.Glob(filepath.Join(local, "repodata", "*"+fetch.StagedSuffix))
	if len(staged) != 0 {
		t.Errorf("staged leftovers remain: %v", staged)
	}
}

// TestSyncRPMVerify verifies size-only skipping keeps same-size corruption
// while the verify option repairs it.
func TestSyncRPMVerify(t *testing.T) {
	opts := &Options{}
	repoDir, repoURL, local, _ := syncRPMFixture(t, opts)
	_ = repoDir

	// Corrupt a local package without changing its size.
	victim := filepath.Join(local, "Packages", "foo-1.0-1.x86_64.rpm")
	corrupt := bytes.Repeat([]byte("oof"), 500)
	if err := os.WriteFile(victim, corrupt, 0644); err != nil {
		t.Fatal(err)
	}

	// A plain re-run trusts the matching size and keeps the corruption.
	if err := syncOne(context.Background(), repoURL, opts.Type, opts); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(victim)
	if !bytes.Equal(got, corrupt) {
		t.Error("size-only run unexpectedly rewrote the file")
	}

	// A verify run re-hashes local files and repairs the corruption.
	opts.Verify = true
	if err := syncOne(context.Background(), repoURL, opts.Type, opts); err != nil {
		t.Fatal(err)
	}
	got, _ = os.ReadFile(victim)
	if !bytes.Equal(got, bytes.Repeat([]byte("foo"), 500)) {
		t.Error("verify run did not repair the corruption")
	}
}

// TestSyncRPMPrune verifies stale files are removed only when pruning is
// enabled.
func TestSyncRPMPrune(t *testing.T) {
	opts := &Options{}
	_, repoURL, local, _ := syncRPMFixture(t, opts)

	stale := filepath.Join(local, "Packages", "stale.rpm")
	if err := os.WriteFile(stale, []byte("junk"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := syncOne(context.Background(), repoURL, opts.Type, opts); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); err != nil {
		t.Error("file pruned without the prune option")
	}

	opts.Prune = true
	if err := syncOne(context.Background(), repoURL, opts.Type, opts); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); err == nil {
		t.Error("stale file survived pruning")
	}
}

// TestSyncRPMDroppedSignature verifies a signature the upstream stops
// serving is removed locally instead of lingering beside a new repomd.xml.
func TestSyncRPMDroppedSignature(t *testing.T) {
	opts := &Options{}
	repoDir, repoURL, local, _ := syncRPMFixture(t, opts)

	ascLocal := filepath.Join(local, "repodata", "repomd.xml.asc")
	if _, err := os.Stat(ascLocal); err != nil {
		t.Fatal("signature not mirrored:", err)
	}
	if err := os.Remove(filepath.Join(repoDir, "repodata", "repomd.xml.asc")); err != nil {
		t.Fatal(err)
	}

	if err := syncOne(context.Background(), repoURL, opts.Type, opts); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ascLocal); !os.IsNotExist(err) {
		t.Error("stale signature survived after the upstream dropped it")
	}
}

// TestSyncRPMChecksumMismatch verifies a package whose served content does
// not match the published checksum fails the synchronization.
func TestSyncRPMChecksumMismatch(t *testing.T) {
	www := t.TempDir()
	repoDir := filepath.Join(www, "repos", "el9")
	testrepos.BuildRPMRepo(t, repoDir)
	srv := testrepos.ServeDir(t, www)

	// Replace a served package with same-size different content after the
	// metadata was generated.
	victim := filepath.Join(repoDir, "Packages", "foo-1.0-1.x86_64.rpm")
	if err := os.WriteFile(victim, bytes.Repeat([]byte("fox"), 500), 0644); err != nil {
		t.Fatal(err)
	}

	opts := &Options{Type: RepoRPM, Destination: t.TempDir(), Workers: 2}
	err := syncOne(context.Background(), srv.URL+"/repos/el9", opts.Type, opts)
	if err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Errorf("expected checksum mismatch error, got %v", err)
	}
}

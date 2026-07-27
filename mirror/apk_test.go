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

// TestParseApkIndex verifies stanza splitting and field extraction.
func TestParseApkIndex(t *testing.T) {
	index := "C:Q1abc=\nP:musl\nV:1.2.5-r1\nA:x86_64\nS:1200\n\nC:Q1def=\nP:zlib\nV:1.3-r2\nS:800\n"
	pkgs, err := parseApkIndex(strings.NewReader(index))
	if err != nil {
		t.Fatal(err)
	}
	if len(pkgs) != 2 {
		t.Fatalf("got %d packages, want 2", len(pkgs))
	}
	if pkgs[0].filename() != "musl-1.2.5-r1.apk" || pkgs[0].size != 1200 {
		t.Errorf("first package = %+v", pkgs[0])
	}
	if pkgs[1].filename() != "zlib-1.3-r2.apk" || pkgs[1].size != 800 {
		t.Errorf("second package = %+v", pkgs[1])
	}
}

// TestReadApkIndex verifies parsing through the signature segment of a
// multistream index archive.
func TestReadApkIndex(t *testing.T) {
	dir := t.TempDir()
	testrepos.BuildApkRepo(t, dir)
	pkgs, err := readApkIndex(filepath.Join(dir, APKIndexName))
	if err != nil {
		t.Fatal(err)
	}
	if len(pkgs) != 2 {
		t.Errorf("got %d packages, want 2", len(pkgs))
	}
}

// TestSyncApk verifies a full synchronization mirrors packages and the
// index with no staged leftovers.
func TestSyncApk(t *testing.T) {
	www := t.TempDir()
	pkgs := testrepos.BuildApkRepo(t, filepath.Join(www, "alpine", "v3.24", "community", "x86_64"))
	srv := testrepos.ServeDir(t, www)

	dest := t.TempDir()
	opts := &Options{Type: RepoApk, Destination: dest, Workers: 2}
	if err := syncOne(context.Background(), srv.URL+"/alpine/v3.24/community/x86_64", opts.Type, opts); err != nil {
		t.Fatal(err)
	}

	local := filepath.Join(dest, "alpine", "v3.24", "community", "x86_64")
	for name, data := range pkgs {
		got, err := os.ReadFile(filepath.Join(local, name))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, data) {
			t.Errorf("package %s content mismatch", name)
		}
	}
	if _, err := os.Stat(filepath.Join(local, APKIndexName)); err != nil {
		t.Error("index not promoted:", err)
	}
	staged, _ := filepath.Glob(filepath.Join(local, "*"+fetch.StagedSuffix))
	if len(staged) != 0 {
		t.Errorf("staged leftovers remain: %v", staged)
	}
}

// TestSyncApkPrune verifies stale packages are removed when pruning.
func TestSyncApkPrune(t *testing.T) {
	www := t.TempDir()
	testrepos.BuildApkRepo(t, filepath.Join(www, "repo"))
	srv := testrepos.ServeDir(t, www)

	dest := t.TempDir()
	opts := &Options{Type: RepoApk, Destination: dest, Workers: 2, Prune: true}
	if err := syncOne(context.Background(), srv.URL+"/repo", opts.Type, opts); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dest, "repo", "old-1.0-r0.apk")
	if err := os.WriteFile(stale, []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := syncOne(context.Background(), srv.URL+"/repo", opts.Type, opts); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); err == nil {
		t.Error("stale package survived pruning")
	}
}

package mirror

import (
	"archive/tar"
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grmrgecko/repo-sync/fetch"
	"github.com/grmrgecko/repo-sync/internal/testrepos"
)

// TestParseDesc verifies field extraction from a pacman desc entry.
func TestParseDesc(t *testing.T) {
	desc := "%FILENAME%\nzlib-1.3-1-x86_64.pkg.tar.zst\n\n%CSIZE%\n1200\n\n%SHA256SUM%\nabc123\n\n%DESC%\nFirst line\nSecond line\n"
	fields := parseDesc([]byte(desc))
	if fields["FILENAME"] != "zlib-1.3-1-x86_64.pkg.tar.zst" {
		t.Errorf("FILENAME = %q", fields["FILENAME"])
	}
	if fields["CSIZE"] != "1200" {
		t.Errorf("CSIZE = %q", fields["CSIZE"])
	}
	if fields["SHA256SUM"] != "abc123" {
		t.Errorf("SHA256SUM = %q", fields["SHA256SUM"])
	}
	if fields["DESC"] != "First line" {
		t.Errorf("DESC = %q, want first value only", fields["DESC"])
	}
}

// TestReadArchDB verifies database parsing for both compressed and plain
// tar archives.
func TestReadArchDB(t *testing.T) {
	dir := t.TempDir()
	testrepos.BuildArchRepo(t, dir, "core")

	pkgs, err := readArchDB(filepath.Join(dir, "core.db"))
	if err != nil {
		t.Fatal(err)
	}
	if len(pkgs) != 2 {
		t.Fatalf("got %d packages, want 2", len(pkgs))
	}
	for _, pkg := range pkgs {
		if pkg.filename == "" || pkg.size <= 0 || pkg.sums["sha256"] == "" {
			t.Errorf("incomplete package entry: %+v", pkg)
		}
	}

	// A plain uncompressed tar database must parse through the sniffer.
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	desc := "%FILENAME%\nplain-1.0-1-any.pkg.tar.zst\n\n%CSIZE%\n10\n\n%SHA256SUM%\nabc\n"
	if err := tw.WriteHeader(&tar.Header{Name: "plain-1.0-1/desc", Mode: 0644, Size: int64(len(desc))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(desc)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	plain := filepath.Join(dir, "plain.db")
	if err := os.WriteFile(plain, buf.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}
	pkgs, err = readArchDB(plain)
	if err != nil {
		t.Fatal(err)
	}
	if len(pkgs) != 1 || pkgs[0].filename != "plain-1.0-1-any.pkg.tar.zst" {
		t.Errorf("plain tar parse = %+v", pkgs)
	}
}

// TestSyncArch verifies a full synchronization mirrors packages,
// signatures, and metadata with no staged leftovers.
func TestSyncArch(t *testing.T) {
	www := t.TempDir()
	pkgs := testrepos.BuildArchRepo(t, filepath.Join(www, "core", "os", "x86_64"), "core")
	srv := testrepos.ServeDir(t, www)

	dest := t.TempDir()
	opts := &Options{Type: RepoArch, Destination: dest, Workers: 2}
	if err := syncOne(context.Background(), srv.URL+"/core/os/x86_64", opts.Type, opts); err != nil {
		t.Fatal(err)
	}

	local := filepath.Join(dest, "core", "os", "x86_64")
	for name, data := range pkgs {
		got, err := os.ReadFile(filepath.Join(local, name))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, data) {
			t.Errorf("package %s content mismatch", name)
		}
		if _, err := os.Stat(filepath.Join(local, name+".sig")); err != nil {
			t.Errorf("signature for %s missing: %v", name, err)
		}
	}
	for _, name := range []string{"core.db", "core.files"} {
		if _, err := os.Stat(filepath.Join(local, name)); err != nil {
			t.Errorf("%s not promoted: %v", name, err)
		}
	}
	staged, _ := filepath.Glob(filepath.Join(local, "*"+fetch.StagedSuffix))
	if len(staged) != 0 {
		t.Errorf("staged leftovers remain: %v", staged)
	}
}

// TestSyncArchPrune verifies stale files are removed when pruning.
func TestSyncArchPrune(t *testing.T) {
	www := t.TempDir()
	testrepos.BuildArchRepo(t, filepath.Join(www, "core"), "core")
	srv := testrepos.ServeDir(t, www)

	dest := t.TempDir()
	opts := &Options{Type: RepoArch, Destination: dest, Workers: 2, Prune: true}
	if err := syncOne(context.Background(), srv.URL+"/core", opts.Type, opts); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dest, "core", "old-0.9-1-x86_64.pkg.tar.zst")
	if err := os.WriteFile(stale, []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := syncOne(context.Background(), srv.URL+"/core", opts.Type, opts); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); err == nil {
		t.Error("stale package survived pruning")
	}
}

// TestArchDBNameFallback verifies the database name is probed from URL
// path segments when directory listings are unavailable.
func TestArchDBNameFallback(t *testing.T) {
	www := t.TempDir()
	testrepos.BuildArchRepo(t, filepath.Join(www, "custom", "os", "x86_64"), "custom")

	// Serve files but refuse directory listings to force the fallback.
	fs := http.FileServer(http.Dir(www))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r)
			return
		}
		fs.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	src := fetch.NewSource([]string{srv.URL + "/custom/os/x86_64"})
	name, err := archDBName(context.Background(), src, srv.URL+"/custom/os/x86_64")
	if err != nil {
		t.Fatal(err)
	}
	if name != "custom" {
		t.Errorf("name = %q, want custom", name)
	}
}

package mirror

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/grmrgecko/repo-sync/fetch"
	"github.com/grmrgecko/repo-sync/internal/testrepos"
)

// TestParseRelease verifies field extraction and checksum block merging.
func TestParseRelease(t *testing.T) {
	release := "Origin: Test\nArchitectures: amd64 arm64\nComponents: main universe\nAcquire-By-Hash: yes\n" +
		"MD5Sum:\n aaa 10 main/binary-amd64/Packages\n" +
		"SHA256:\n bbb 10 main/binary-amd64/Packages\n ccc 20 main/binary-amd64/Packages.gz\n"
	rel, err := parseRelease([]byte(release))
	if err != nil {
		t.Fatal(err)
	}
	if len(rel.components) != 2 || rel.components[0] != "main" {
		t.Errorf("components = %v", rel.components)
	}
	if len(rel.architectures) != 2 {
		t.Errorf("architectures = %v", rel.architectures)
	}
	if !rel.acquireByHash {
		t.Error("acquireByHash not detected")
	}
	f := rel.files["main/binary-amd64/Packages"]
	if f == nil || f.size != 10 || f.sums["md5sum"] != "aaa" || f.sums["sha256"] != "bbb" {
		t.Errorf("merged file = %+v", f)
	}
	if rel.files["main/binary-amd64/Packages.gz"] == nil {
		t.Error("gz variant missing")
	}
}

// TestIndexArch verifies architecture extraction from release file paths.
func TestIndexArch(t *testing.T) {
	cases := map[string]string{
		"main/binary-amd64/Packages.gz":                  "amd64",
		"main/debian-installer/binary-i386/Packages":     "i386",
		"main/installer-armhf/current/images/SHA256SUMS": "armhf",
		"main/source/Sources.gz":                         "source",
		"Contents-s390x.gz":                              "s390x",
		"main/Contents-udeb-riscv64.gz":                  "riscv64",
		"main/i18n/Translation-en.gz":                    "",
		"main/dep11/icons-64x64.tar.gz":                  "",
		"Release":                                        "",
	}
	for p, want := range cases {
		if got := indexArch(p); got != want {
			t.Errorf("indexArch(%q) = %q, want %q", p, got, want)
		}
	}
}

// TestIncludeIndexFile verifies component and architecture filtering.
func TestIncludeIndexFile(t *testing.T) {
	cases := []struct {
		path  string
		comps []string
		archs []string
		want  bool
	}{
		{"main/binary-amd64/Packages.gz", nil, nil, true},
		{"universe/binary-amd64/Packages.gz", []string{"main"}, nil, false},
		{"main/binary-i386/Packages.gz", []string{"main"}, []string{"amd64"}, false},
		{"main/binary-amd64/Packages.gz", []string{"main"}, []string{"amd64"}, true},
		{"main/i18n/Translation-en.gz", []string{"main"}, []string{"amd64"}, true},
		{"Contents-arm64.gz", []string{"main"}, []string{"amd64"}, false},
		{"main/source/Sources.gz", nil, []string{"amd64"}, false},
		{"main/source/Sources.gz", nil, []string{"amd64", "source"}, true},
	}
	for _, c := range cases {
		if got := includeIndexFile(c.path, c.comps, c.archs); got != c.want {
			t.Errorf("includeIndexFile(%q, %v, %v) = %v, want %v", c.path, c.comps, c.archs, got, c.want)
		}
	}
}

// TestSourceFiles verifies checksum list merging and directory joining for
// source package stanzas.
func TestSourceFiles(t *testing.T) {
	st := stanza{
		"Directory":        "pool/main/h/hello",
		"Files":            "\naaa 100 hello_1.0.dsc\nbbb 200 hello_1.0.tar.gz",
		"Checksums-Sha256": "\nccc 100 hello_1.0.dsc\nddd 200 hello_1.0.tar.gz",
	}
	files := sourceFiles(st)
	if len(files) != 2 {
		t.Fatalf("got %d files, want 2", len(files))
	}
	byPath := map[string]*debFile{}
	for _, f := range files {
		byPath[f.path] = f
	}
	dsc := byPath["pool/main/h/hello/hello_1.0.dsc"]
	if dsc == nil || dsc.size != 100 || dsc.sums["md5sum"] != "aaa" || dsc.sums["sha256"] != "ccc" {
		t.Errorf("dsc = %+v", dsc)
	}
}

// TestSyncDebStructured verifies a dists-layout synchronization: pool files
// at the archive root, promoted indexes, by-hash entries, and no staged
// leftovers.
func TestSyncDebStructured(t *testing.T) {
	www := t.TempDir()
	pool := testrepos.BuildDebRepo(t, filepath.Join(www, "debian"))
	srv := testrepos.ServeDir(t, www)

	dest := t.TempDir()
	opts := &Options{Type: RepoDeb, Destination: dest, Workers: 2}
	if err := syncOne(context.Background(), srv.URL+"/debian/dists/test", opts.Type, opts); err != nil {
		t.Fatal(err)
	}

	root := filepath.Join(dest, "debian")
	for p, data := range pool {
		got, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(p)))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, data) {
			t.Errorf("pool file %s content mismatch", p)
		}
	}
	suite := filepath.Join(root, "dists", "test")
	if _, err := os.Stat(filepath.Join(suite, "Release")); err != nil {
		t.Error("Release not promoted:", err)
	}
	if _, err := os.Stat(filepath.Join(suite, "main", "binary-amd64", "Packages.gz")); err != nil {
		t.Error("Packages.gz missing:", err)
	}
	// The uncompressed variant is listed in the Release file but not
	// served, so it must be skipped without failing the sync.
	if _, err := os.Stat(filepath.Join(suite, "main", "binary-amd64", "Packages")); err == nil {
		t.Error("unserved uncompressed Packages unexpectedly present")
	}
	byHash, err := filepath.Glob(filepath.Join(suite, "main", "binary-amd64", "by-hash", "SHA256", "*"))
	if err != nil || len(byHash) == 0 {
		t.Error("by-hash entries missing")
	}
	staged, _ := filepath.Glob(filepath.Join(suite, "*"+fetch.StagedSuffix))
	if len(staged) != 0 {
		t.Errorf("staged leftovers remain: %v", staged)
	}
}

// TestSyncDebComponentFilter verifies filtered components are not mirrored.
func TestSyncDebComponentFilter(t *testing.T) {
	www := t.TempDir()
	testrepos.BuildDebRepo(t, filepath.Join(www, "debian"))
	srv := testrepos.ServeDir(t, www)

	dest := t.TempDir()
	opts := &Options{Type: RepoDeb, Destination: dest, Workers: 2, Components: []string{"main"}}
	if err := syncOne(context.Background(), srv.URL+"/debian/dists/test", opts.Type, opts); err != nil {
		t.Fatal(err)
	}

	root := filepath.Join(dest, "debian")
	if _, err := os.Stat(filepath.Join(root, "pool", "main", "h", "hello", "hello_1.0_amd64.deb")); err != nil {
		t.Error("main pool file missing:", err)
	}
	if _, err := os.Stat(filepath.Join(root, "pool", "universe")); err == nil {
		t.Error("filtered universe pool files present")
	}
	if _, err := os.Stat(filepath.Join(root, "dists", "test", "universe")); err == nil {
		t.Error("filtered universe indexes present")
	}
}

// TestSyncDebFlat verifies a repository without a dists directory mirrors
// relative to the repository URL itself.
func TestSyncDebFlat(t *testing.T) {
	www := t.TempDir()
	pool := testrepos.BuildFlatDebRepo(t, filepath.Join(www, "flat"))
	srv := testrepos.ServeDir(t, www)

	dest := t.TempDir()
	opts := &Options{Type: RepoDeb, Destination: dest, Workers: 2}
	if err := syncOne(context.Background(), srv.URL+"/flat", opts.Type, opts); err != nil {
		t.Fatal(err)
	}

	root := filepath.Join(dest, "flat")
	for name, data := range pool {
		got, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, data) {
			t.Errorf("package %s content mismatch", name)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "Release")); err != nil {
		t.Error("Release not promoted:", err)
	}
}

// TestSyncDebMissingPoolRetry verifies a pool file the upstream stopped
// serving does not cost the rest of the suite: the run mirrors everything
// else, reports the absence until its retry budget runs out, and then
// synchronizes cleanly with the absence recorded as known.
func TestSyncDebMissingPoolRetry(t *testing.T) {
	www := t.TempDir()
	archive := filepath.Join(www, "debian")
	testrepos.BuildDebRepo(t, archive)
	srv := testrepos.ServeDir(t, www)

	// Withdraw one pool file while its index keeps listing it, which is how
	// an incomplete upstream presents itself.
	const gone = "pool/universe/e/extra/extra_2.0_amd64.deb"
	if err := os.Remove(filepath.Join(archive, filepath.FromSlash(gone))); err != nil {
		t.Fatal(err)
	}

	dest := t.TempDir()
	opts := &Options{
		Type:           RepoDeb,
		Destination:    dest,
		Workers:        2,
		Missing:        fetch.MissingRetry,
		MissingRetries: 2,
	}

	// The first run still publishes the suite, but reports the absence.
	err := syncOne(context.Background(), srv.URL+"/debian/dists/test", opts.Type, opts)
	if err == nil {
		t.Fatal("the first run hid a missing pool file")
	}
	root := filepath.Join(dest, "debian")
	if _, err := os.Stat(filepath.Join(root, "pool", "main", "h", "hello", "hello_1.0_amd64.deb")); err != nil {
		t.Error("a missing pool file cost the rest of the pool:", err)
	}
	if _, err := os.Stat(filepath.Join(root, "dists", "test", "Release")); err != nil {
		t.Error("a missing pool file cost the suite metadata:", err)
	}

	// The second run exhausts the retry budget, so the absence is accepted
	// and recorded in the suite directory rather than failing the run.
	if err := syncOne(context.Background(), srv.URL+"/debian/dists/test", opts.Type, opts); err != nil {
		t.Fatal("the absence still failed the run after its retry budget:", err)
	}
	state := filepath.Join(root, "dists", "test", fetch.MissingStateName)
	data, err := os.ReadFile(state)
	if err != nil {
		t.Fatal("the known absence was not recorded:", err)
	}
	if !bytes.Contains(data, []byte(gone)) {
		t.Errorf("recorded state %s does not name the missing file", data)
	}

	// Serving the file again clears the record on the next run.
	testrepos.WriteFile(t, filepath.Join(archive, filepath.FromSlash(gone)), bytes.Repeat([]byte("extra"), 400))
	if err := syncOne(context.Background(), srv.URL+"/debian/dists/test", opts.Type, opts); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Error("the record survived the upstream serving the file again")
	}
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(gone))); err != nil {
		t.Error("the restored pool file was not fetched:", err)
	}
}

// TestSyncDebMissingPoolModes verifies the strict mode still fails on the
// first missing pool file and the ignore mode never fails at all.
func TestSyncDebMissingPoolModes(t *testing.T) {
	www := t.TempDir()
	archive := filepath.Join(www, "debian")
	testrepos.BuildDebRepo(t, archive)
	if err := os.Remove(filepath.Join(archive, filepath.FromSlash("pool/universe/e/extra/extra_2.0_amd64.deb"))); err != nil {
		t.Fatal(err)
	}
	srv := testrepos.ServeDir(t, www)

	strict := &Options{Type: RepoDeb, Destination: t.TempDir(), Workers: 2, Missing: fetch.MissingFail}
	if err := syncOne(context.Background(), srv.URL+"/debian/dists/test", strict.Type, strict); err == nil {
		t.Error("the strict mode tolerated a missing pool file")
	}

	lenient := &Options{Type: RepoDeb, Destination: t.TempDir(), Workers: 2, Missing: fetch.MissingIgnore}
	if err := syncOne(context.Background(), srv.URL+"/debian/dists/test", lenient.Type, lenient); err != nil {
		t.Error("the ignore mode failed on a missing pool file:", err)
	}
}

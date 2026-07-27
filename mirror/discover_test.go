package mirror

import (
	"context"
	"net/url"
	"path/filepath"
	"testing"

	"github.com/grmrgecko/repo-sync/internal/testrepos"
)

// TestChildEntry verifies anchor resolution against a directory URL.
func TestChildEntry(t *testing.T) {
	base, _ := url.Parse("http://example.com/repos/")
	cases := []struct {
		href  string
		want  string
		isDir bool
	}{
		{"el9/", "el9", true},
		{"file.rpm", "file.rpm", false},
		{"/repos/el8/", "el8", true},
		{"../", "", false},
		{"?C=N;O=D", "", false},
		{"http://other.example.com/repos/x/", "", false},
		{"/outside/", "", false},
		{"deep/nested/", "", false},
		{"#anchor", "", false},
	}
	for _, c := range cases {
		got, isDir := childEntry(base, c.href)
		if got != c.want || isDir != c.isDir {
			t.Errorf("childEntry(%q) = %q,%v, want %q,%v", c.href, got, isDir, c.want, c.isDir)
		}
	}
}

// discoverOpts builds the options a discovery test crawls with.
func discoverOpts(typ RepoType, depth int) *Options {
	return &Options{Type: typ, DiscoverDepth: depth}
}

// repoURLs reduces a crawl's repositories to their URLs, for comparison
// against what a fixture tree publishes.
func repoURLs(found *discovery) []string {
	urls := make([]string, 0, len(found.repos))
	for _, repo := range found.repos {
		urls = append(urls, repo.url)
	}
	return urls
}

// checkURLs verifies a crawl found exactly the expected repositories, in
// order.
func checkURLs(t *testing.T, found *discovery, want []string) {
	t.Helper()
	got := repoURLs(found)
	if len(got) != len(want) {
		t.Fatalf("found %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("found[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestDiscoverRepos verifies RPM repositories are found through directory
// listings and that the depth limit is honored.
func TestDiscoverRepos(t *testing.T) {
	www := t.TempDir()
	testrepos.BuildRPMRepo(t, filepath.Join(www, "a", "repo1"))
	testrepos.BuildRPMRepo(t, filepath.Join(www, "b", "nested", "repo2"))
	testrepos.BuildRPMRepo(t, filepath.Join(www, "deep", "x", "y", "z", "repo3"))
	srv := testrepos.ServeDir(t, www)

	found, err := discoverRepos(context.Background(), srv.URL, discoverOpts(RepoRPM, 3))
	if err != nil {
		t.Fatal(err)
	}
	checkURLs(t, found, []string{srv.URL + "/a/repo1", srv.URL + "/b/nested/repo2"})

	// A deeper limit reaches the third repository.
	found, err = discoverRepos(context.Background(), srv.URL, discoverOpts(RepoRPM, 6))
	if err != nil {
		t.Fatal(err)
	}
	if len(found.repos) != 3 {
		t.Errorf("depth 6 found %v, want 3 repositories", repoURLs(found))
	}
}

// TestDiscoverArchRepos verifies pacman repositories are recognized by
// their database files.
func TestDiscoverArchRepos(t *testing.T) {
	www := t.TempDir()
	testrepos.BuildArchRepo(t, filepath.Join(www, "core", "os", "x86_64"), "core")
	testrepos.BuildArchRepo(t, filepath.Join(www, "extra", "os", "x86_64"), "extra")
	srv := testrepos.ServeDir(t, www)

	found, err := discoverRepos(context.Background(), srv.URL, discoverOpts(RepoArch, 4))
	if err != nil {
		t.Fatal(err)
	}
	checkURLs(t, found, []string{srv.URL + "/core/os/x86_64", srv.URL + "/extra/os/x86_64"})
}

// TestDiscoverApkRepos verifies Alpine repositories are recognized by
// their index files, matching the arch directories under a release tree.
func TestDiscoverApkRepos(t *testing.T) {
	www := t.TempDir()
	testrepos.BuildApkRepo(t, filepath.Join(www, "v3.24", "community", "x86_64"))
	testrepos.BuildApkRepo(t, filepath.Join(www, "v3.24", "community", "aarch64"))
	srv := testrepos.ServeDir(t, www)

	found, err := discoverRepos(context.Background(), srv.URL+"/v3.24/community", discoverOpts(RepoApk, 1))
	if err != nil {
		t.Fatal(err)
	}
	checkURLs(t, found, []string{srv.URL + "/v3.24/community/aarch64", srv.URL + "/v3.24/community/x86_64"})
}

// TestDiscoverDebRepos verifies suite directories are recognized by their
// release files.
func TestDiscoverDebRepos(t *testing.T) {
	www := t.TempDir()
	testrepos.BuildDebRepo(t, filepath.Join(www, "debian"))
	testrepos.BuildFlatDebRepo(t, filepath.Join(www, "flat"))
	srv := testrepos.ServeDir(t, www)

	found, err := discoverRepos(context.Background(), srv.URL, discoverOpts(RepoDeb, 4))
	if err != nil {
		t.Fatal(err)
	}
	checkURLs(t, found, []string{srv.URL + "/debian/dists/test", srv.URL + "/flat"})
}

// TestDiscoverAllTypes verifies a crawl that names no type finds every
// format under one URL, as a vendor archive publishing both yum and apt
// repositories requires.
func TestDiscoverAllTypes(t *testing.T) {
	www := t.TempDir()
	testrepos.BuildRPMRepo(t, filepath.Join(www, "yum", "el9"))
	testrepos.BuildDebRepo(t, filepath.Join(www, "apt"))
	testrepos.BuildArchRepo(t, filepath.Join(www, "arch", "os", "x86_64"), "core")
	testrepos.BuildApkRepo(t, filepath.Join(www, "alpine", "v3.24", "x86_64"))
	srv := testrepos.ServeDir(t, www)

	found, err := discoverRepos(context.Background(), srv.URL, &Options{DiscoverDepth: 4})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]RepoType{
		srv.URL + "/alpine/v3.24/x86_64": RepoApk,
		srv.URL + "/apt/dists/test":      RepoDeb,
		srv.URL + "/arch/os/x86_64":      RepoArch,
		srv.URL + "/yum/el9":             RepoRPM,
	}
	if len(found.repos) != len(want) {
		t.Fatalf("found %v, want %v", found.repos, want)
	}
	for _, repo := range found.repos {
		if want[repo.url] != repo.typ {
			t.Errorf("%s discovered as %q, want %q", repo.url, repo.typ, want[repo.url])
		}
	}

	// Limiting the crawl to one format leaves the rest of the tree alone.
	found, err = discoverRepos(context.Background(), srv.URL, &Options{Types: []RepoType{RepoRPM}, DiscoverDepth: 4})
	if err != nil {
		t.Fatal(err)
	}
	checkURLs(t, found, []string{srv.URL + "/yum/el9"})
}

// TestDiscoverExclude verifies excluded directories are neither crawled nor
// reported, by name anywhere in the tree or by path below the crawled URL.
func TestDiscoverExclude(t *testing.T) {
	www := t.TempDir()
	testrepos.BuildRPMRepo(t, filepath.Join(www, "yum", "el", "9"))
	testrepos.BuildRPMRepo(t, filepath.Join(www, "yum", "sles", "15"))
	testrepos.BuildRPMRepo(t, filepath.Join(www, "yum", "fc", "40"))
	testrepos.BuildRPMRepo(t, filepath.Join(www, "docker", "el", "9"))
	srv := testrepos.ServeDir(t, www)

	opts := discoverOpts(RepoRPM, 4)
	opts.Exclude = []string{"sles", "fc", "docker"}
	found, err := discoverRepos(context.Background(), srv.URL, opts)
	if err != nil {
		t.Fatal(err)
	}
	checkURLs(t, found, []string{srv.URL + "/yum/el/9"})

	// A pattern with a slash pins the exclusion to one place in the tree.
	opts.Exclude = []string{"yum/*"}
	found, err = discoverRepos(context.Background(), srv.URL, opts)
	if err != nil {
		t.Fatal(err)
	}
	checkURLs(t, found, []string{srv.URL + "/docker/el/9"})
}

// TestDiscoverIncludeFiles verifies loose files outside repositories are
// matched by the include patterns, and that files inside a repository are
// left to that repository's own synchronization.
func TestDiscoverIncludeFiles(t *testing.T) {
	www := t.TempDir()
	testrepos.BuildRPMRepo(t, filepath.Join(www, "yum", "el9"))
	testrepos.WriteFile(t, filepath.Join(www, "RPM-GPG-KEY-mysql-2023"), []byte("key"))
	testrepos.WriteFile(t, filepath.Join(www, "README.txt"), []byte("notes"))
	testrepos.WriteFile(t, filepath.Join(www, "yum", "loose-1.0.rpm"), []byte("rpm"))
	testrepos.WriteFile(t, filepath.Join(www, "yum", "el9", "extra.rpm"), []byte("rpm"))
	srv := testrepos.ServeDir(t, www)

	opts := discoverOpts(RepoRPM, 4)
	opts.IncludeFiles = []string{"*.rpm", "RPM-GPG-KEY-*"}
	found, err := discoverRepos(context.Background(), srv.URL, opts)
	if err != nil {
		t.Fatal(err)
	}
	checkURLs(t, found, []string{srv.URL + "/yum/el9"})

	want := []fileTarget{
		{dirURL: srv.URL, name: "RPM-GPG-KEY-mysql-2023"},
		{dirURL: srv.URL + "/yum", name: "loose-1.0.rpm"},
	}
	if len(found.files) != len(want) {
		t.Fatalf("matched %v, want %v", found.files, want)
	}
	for i := range want {
		if found.files[i] != want[i] {
			t.Errorf("files[%d] = %v, want %v", i, found.files[i], want[i])
		}
	}

	// An exclusion wins over an include pattern.
	opts.Exclude = []string{"RPM-GPG-KEY-*"}
	found, err = discoverRepos(context.Background(), srv.URL, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(found.files) != 1 || found.files[0].name != "loose-1.0.rpm" {
		t.Errorf("matched %v, want only the loose rpm", found.files)
	}
}

// TestDiscoverAnchoredPatterns verifies a leading slash limits a pattern to
// the crawled directory itself, for both include and exclude patterns.
func TestDiscoverAnchoredPatterns(t *testing.T) {
	www := t.TempDir()
	testrepos.BuildRPMRepo(t, filepath.Join(www, "yum", "el9"))
	testrepos.WriteFile(t, filepath.Join(www, "root-1.0.rpm"), []byte("rpm"))
	testrepos.WriteFile(t, filepath.Join(www, "yum", "loose-1.0.rpm"), []byte("rpm"))
	testrepos.WriteFile(t, filepath.Join(www, "yum", "deep", "buried-1.0.rpm"), []byte("rpm"))
	srv := testrepos.ServeDir(t, www)

	// Anchored, only the root directory's file is mirrored.
	opts := discoverOpts(RepoRPM, 4)
	opts.IncludeFiles = []string{"/*.rpm"}
	found, err := discoverRepos(context.Background(), srv.URL, opts)
	if err != nil {
		t.Fatal(err)
	}
	want := []fileTarget{{dirURL: srv.URL, name: "root-1.0.rpm"}}
	if len(found.files) != len(want) || found.files[0] != want[0] {
		t.Errorf("anchored include matched %v, want %v", found.files, want)
	}

	// The same pattern unanchored matches at every depth, which is what
	// makes the anchor worth having.
	opts.IncludeFiles = []string{"*.rpm"}
	found, err = discoverRepos(context.Background(), srv.URL, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(found.files) != 3 {
		t.Errorf("unanchored include matched %v, want 3 files", found.files)
	}

	// An anchored exclusion applies to the crawled directory only, leaving
	// the same name deeper in the tree alone.
	opts.Exclude = []string{"/root-1.0.rpm"}
	found, err = discoverRepos(context.Background(), srv.URL, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(found.files) != 2 {
		t.Errorf("anchored exclude left %v, want 2 files", found.files)
	}
	for _, file := range found.files {
		if file.name == "root-1.0.rpm" {
			t.Errorf("anchored exclude kept %v", file)
		}
	}

	// An anchored directory exclusion keeps the crawl out of a top-level
	// directory without matching that name elsewhere.
	opts.Exclude = []string{"/deep"}
	found, err = discoverRepos(context.Background(), srv.URL, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(found.files) != 3 {
		t.Errorf("top-level exclude of an unrelated name left %v, want 3 files", found.files)
	}
	opts.Exclude = []string{"/yum"}
	found, err = discoverRepos(context.Background(), srv.URL, opts)
	if err != nil {
		t.Fatal(err)
	}
	checkURLs(t, found, nil)
	if len(found.files) != 1 || found.files[0].name != "root-1.0.rpm" {
		t.Errorf("anchored directory exclude left %v, want only the root rpm", found.files)
	}
}

// TestMatchSubject verifies the pairing of a pattern with the string it is
// matched against, which decides whether a pattern is anchored, pinned by
// path, or matched by name anywhere in the tree.
func TestMatchSubject(t *testing.T) {
	cases := []struct {
		pattern     string
		wantPattern string
		wantSubject string
	}{
		{"*.rpm", "*.rpm", "pkg.rpm"},
		{"/*.rpm", "*.rpm", "yum/deep/pkg.rpm"},
		{"yum/*.rpm", "yum/*.rpm", "yum/deep/pkg.rpm"},
		{"/yum/*.rpm", "yum/*.rpm", "yum/deep/pkg.rpm"},
	}
	for _, c := range cases {
		pattern, subject := matchSubject(c.pattern, "pkg.rpm", "yum/deep/pkg.rpm")
		if pattern != c.wantPattern || subject != c.wantSubject {
			t.Errorf("matchSubject(%q) = %q,%q, want %q,%q",
				c.pattern, pattern, subject, c.wantPattern, c.wantSubject)
		}
	}
}

// TestDetectType verifies a repository URL given without a type is
// identified by its metadata, and that an unidentifiable URL is reported.
func TestDetectType(t *testing.T) {
	www := t.TempDir()
	testrepos.BuildRPMRepo(t, filepath.Join(www, "el9"))
	testrepos.BuildDebRepo(t, filepath.Join(www, "debian"))
	testrepos.BuildArchRepo(t, filepath.Join(www, "core", "os", "x86_64"), "core")
	testrepos.BuildApkRepo(t, filepath.Join(www, "alpine"))
	srv := testrepos.ServeDir(t, www)

	cases := []struct {
		path string
		want RepoType
	}{
		{"/el9", RepoRPM},
		{"/debian/dists/test", RepoDeb},
		{"/core/os/x86_64", RepoArch},
		{"/alpine", RepoApk},
	}
	for _, c := range cases {
		got, err := detectType(context.Background(), srv.URL+c.path, AllTypes)
		if err != nil {
			t.Errorf("detectType(%s): %v", c.path, err)
			continue
		}
		if got != c.want {
			t.Errorf("detectType(%s) = %q, want %q", c.path, got, c.want)
		}
	}

	// A URL holding no repository metadata cannot be identified.
	if _, err := detectType(context.Background(), srv.URL+"/debian", AllTypes); err == nil {
		t.Error("expected an error detecting the type of an archive root")
	}
}

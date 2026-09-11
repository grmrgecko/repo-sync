package server

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	cfg "github.com/grmrgecko/repo-sync/config"
	"github.com/grmrgecko/repo-sync/fetch"
	"github.com/grmrgecko/repo-sync/internal/testrepos"
	"github.com/grmrgecko/repo-sync/mirror"
	"github.com/grmrgecko/repo-sync/state"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// serverFixture builds an upstream with one repository of each type plus a
// generic file, configures the mirror server against it, and returns the
// mirror's test server and the online root.
func serverFixture(t *testing.T) (*httptest.Server, string, string) {
	t.Helper()

	// Upstream content behind a single catch-all mount.
	www := t.TempDir()
	testrepos.BuildRPMRepo(t, filepath.Join(www, "almalinux", "9", "BaseOS", "x86_64", "os"))
	testrepos.BuildDebRepo(t, filepath.Join(www, "debian"))
	testrepos.BuildArchRepo(t, filepath.Join(www, "archlinux", "core", "os", "x86_64"), "core")
	testrepos.BuildApkRepo(t, filepath.Join(www, "alpine", "v3.24", "main", "x86_64"))
	testrepos.WriteFile(t, filepath.Join(www, "notes", "README.txt"), []byte("generic file"))
	upstream := testrepos.ServeDir(t, www)

	onlineRoot := t.TempDir()
	offlineRoot := t.TempDir()
	confDir := t.TempDir()
	conf := fmt.Sprintf(`
state_path: %s/state.yaml
domains:
  - domain: 127.0.0.1
    role: online
    root: %s
  - domain: offline.test
    role: offline
    root: %s
mounts:
  - path: /
    upstream: %s
`, confDir, onlineRoot, offlineRoot, upstream.URL)
	confPath := filepath.Join(confDir, "config.yaml")
	testrepos.WriteFile(t, confPath, []byte(conf))
	if err := cfg.Init(confPath); err != nil {
		t.Fatal(err)
	}
	if err := state.Load(); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(Handler())
	t.Cleanup(srv.Close)
	// Crawls dispatched by requests run in the background; drain them so
	// they do not race the next test's state reload.
	t.Cleanup(WaitCrawls)
	return srv, onlineRoot, offlineRoot
}

// waitFor polls a condition until it holds or the deadline passes; crawls
// dispatched by requests run asynchronously.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// get fetches a mirror URL with an optional Host override.
func get(t *testing.T, srv *httptest.Server, host, path string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if host != "" {
		req.Host = host
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return resp, body
}

// TestServeRPMDiscovery verifies a repomd.xml request registers and crawls
// the repository, filling the local tree behind the response.
func TestServeRPMDiscovery(t *testing.T) {
	srv, onlineRoot, _ := serverFixture(t)

	repo := "/almalinux/9/BaseOS/x86_64/os"
	resp, body := get(t, srv, "", repo+"/repodata/repomd.xml")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if len(body) == 0 {
		t.Fatal("empty repomd.xml response")
	}

	// The repository was registered and the background crawl mirrors its
	// packages behind the response.
	if _, ok := state.S.Entry("rpm:" + repo); !ok {
		t.Error("repository not registered in state")
	}
	var pkgs []string
	waitFor(t, "rpm crawl", func() bool {
		pkgs, _ = filepath.Glob(filepath.Join(onlineRoot, filepath.FromSlash(repo[1:]), "Packages", "*.rpm"))
		return len(pkgs) > 0
	})

	// Package requests are served from the crawled tree.
	resp, _ = get(t, srv, "", repo+"/Packages/"+filepath.Base(pkgs[0]))
	if resp.StatusCode != http.StatusOK {
		t.Errorf("package request status = %d", resp.StatusCode)
	}
}

// TestServeDebDiscovery verifies a Release request crawls the suite with
// pool files under the archive root.
func TestServeDebDiscovery(t *testing.T) {
	srv, onlineRoot, _ := serverFixture(t)

	resp, _ := get(t, srv, "", "/debian/dists/test/Release")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if _, ok := state.S.Entry("deb:/debian/dists/test"); !ok {
		t.Error("suite not registered in state")
	}
	waitFor(t, "deb crawl", func() bool {
		_, err := os.Stat(filepath.Join(onlineRoot, "debian", "pool", "main", "h", "hello", "hello_1.0_amd64.deb"))
		return err == nil
	})

	// A pool request under the archive root is a repository member.
	resp, _ = get(t, srv, "", "/debian/pool/main/h/hello/hello_1.0_amd64.deb")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("pool request status = %d", resp.StatusCode)
	}
}

// TestServeArchAndApkDiscovery verifies database and index requests crawl
// their repositories.
func TestServeArchAndApkDiscovery(t *testing.T) {
	srv, onlineRoot, _ := serverFixture(t)

	resp, _ := get(t, srv, "", "/archlinux/core/os/x86_64/core.db")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("arch status = %d", resp.StatusCode)
	}
	waitFor(t, "arch crawl", func() bool {
		pkgs, _ := filepath.Glob(filepath.Join(onlineRoot, "archlinux", "core", "os", "x86_64", "*.pkg.tar.zst"))
		return len(pkgs) > 0
	})

	resp, _ = get(t, srv, "", "/alpine/v3.24/main/x86_64/APKINDEX.tar.gz")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("apk status = %d", resp.StatusCode)
	}
	waitFor(t, "apk crawl", func() bool {
		apks, _ := filepath.Glob(filepath.Join(onlineRoot, "alpine", "v3.24", "main", "x86_64", "*.apk"))
		return len(apks) > 0
	})
}

// TestServeGeneric verifies plain files proxy through with caching and
// missing files are answered from the negative cache.
func TestServeGeneric(t *testing.T) {
	srv, onlineRoot, _ := serverFixture(t)

	resp, body := get(t, srv, "", "/notes/README.txt")
	if resp.StatusCode != http.StatusOK || string(body) != "generic file" {
		t.Fatalf("generic = %d %q", resp.StatusCode, body)
	}
	if _, err := os.Stat(filepath.Join(onlineRoot, "notes", "README.txt")); err != nil {
		t.Error("generic file not cached:", err)
	}

	resp, _ = get(t, srv, "", "/notes/missing.txt")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("missing file status = %d", resp.StatusCode)
	}
	if status, hit := fetchFailures.Hit("generic:/notes/missing.txt", time.Now()); !hit || status != http.StatusNotFound {
		t.Error("missing file not negative-cached")
	}
}

// TestServeGenericRevalidation verifies GenericMaxAge gates how often a
// cached plain file is revalidated. The upstream file carries an old
// Last-Modified that the cached copy inherits, so the gate is measured from
// the recorded upstream check rather than the local modification time.
func TestServeGenericRevalidation(t *testing.T) {
	www := t.TempDir()
	name := filepath.Join(www, "notes", "README.txt")
	testrepos.WriteFile(t, name, []byte("generic file"))
	old := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(name, old, old); err != nil {
		t.Fatal(err)
	}

	var upstreamHits atomic.Int64
	files := http.FileServer(http.Dir(www))
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		files.ServeHTTP(w, r)
	}))
	t.Cleanup(upstream.Close)

	confDir := t.TempDir()
	confPath := filepath.Join(confDir, "config.yaml")
	testrepos.WriteFile(t, confPath, []byte(fmt.Sprintf(`
state_path: %s/state.yaml
generic_max_age: 6h
domains:
  - domain: 127.0.0.1
    role: online
    root: %s
mounts:
  - path: /
    upstream: %s
`, confDir, t.TempDir(), upstream.URL)))
	if err := cfg.Init(confPath); err != nil {
		t.Fatal(err)
	}
	if err := state.Load(); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(Handler())
	t.Cleanup(srv.Close)

	for i := 0; i < 3; i++ {
		resp, body := get(t, srv, "", "/notes/README.txt")
		if resp.StatusCode != http.StatusOK || string(body) != "generic file" {
			t.Fatalf("request %d = %d %q", i, resp.StatusCode, body)
		}
	}
	if got := upstreamHits.Load(); got != 1 {
		t.Errorf("upstream requests = %d, want 1 within generic_max_age", got)
	}
}

// TestServeOffline verifies offline domains serve their own tree first and
// read through the online cache otherwise.
func TestServeOffline(t *testing.T) {
	srv, _, offlineRoot := serverFixture(t)

	// Not present offline: delegated to online, which proxies upstream.
	resp, body := get(t, srv, "offline.test", "/notes/README.txt")
	if resp.StatusCode != http.StatusOK || string(body) != "generic file" {
		t.Fatalf("offline read-through = %d %q", resp.StatusCode, body)
	}

	// Present offline: served from the offline tree.
	testrepos.WriteFile(t, filepath.Join(offlineRoot, "notes", "README.txt"), []byte("published copy"))
	resp, body = get(t, srv, "offline.test", "/notes/README.txt")
	if resp.StatusCode != http.StatusOK || string(body) != "published copy" {
		t.Errorf("offline copy = %d %q", resp.StatusCode, body)
	}
}

// TestServeDirectoryIndexes verifies the generated root index without a
// catch-all mount and the notice page when indexes are disabled.
func TestServeDirectoryIndexes(t *testing.T) {
	www := t.TempDir()
	testrepos.WriteFile(t, filepath.Join(www, "notes", "README.txt"), []byte("generic file"))
	upstream := testrepos.ServeDir(t, www)

	confDir := t.TempDir()
	confPath := filepath.Join(confDir, "config.yaml")
	conf := fmt.Sprintf(`
state_path: %s/state.yaml
domains:
  - domain: 127.0.0.1
    role: online
    root: %s
mounts:
  - path: /notes
    upstream: %s/notes
  - path: /almalinux
    upstream: %s/almalinux
    repos:
      - path: /almalinux/9/BaseOS/x86_64/os
        type: rpm
`, confDir, t.TempDir(), upstream.URL, upstream.URL)
	testrepos.WriteFile(t, confPath, []byte(conf))
	if err := cfg.Init(confPath); err != nil {
		t.Fatal(err)
	}
	if err := state.Load(); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(Handler())
	t.Cleanup(srv.Close)

	// Without a catch-all mount the root serves a generated index linking
	// every mount and configured repository.
	resp, body := get(t, srv, "", "/")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("index status = %d", resp.StatusCode)
	}
	for _, want := range []string{`"/notes/"`, `"/almalinux/"`, `"/almalinux/9/BaseOS/x86_64/os/"`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("index missing %s in %q", want, body)
		}
	}

	// Disabling indexes answers every directory request with the notice.
	disabled := strings.Replace(conf, "mounts:", "http:\n  directory_indexes: false\nmounts:", 1)
	testrepos.WriteFile(t, confPath, []byte(disabled))
	if err := cfg.Init(confPath); err != nil {
		t.Fatal(err)
	}
	resp, body = get(t, srv, "", "/")
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "Directory indexes are disabled") {
		t.Errorf("disabled root = %d %q", resp.StatusCode, body)
	}
	resp, body = get(t, srv, "", "/notes/")
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "Directory indexes are disabled") {
		t.Errorf("disabled subdirectory = %d %q", resp.StatusCode, body)
	}
}

// TestServeRejections verifies unknown domains and internal bookkeeping
// files are refused.
func TestServeRejections(t *testing.T) {
	srv, _, _ := serverFixture(t)

	resp, _ := get(t, srv, "unknown.test", "/notes/README.txt")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown domain status = %d", resp.StatusCode)
	}
	resp, _ = get(t, srv, "", path.Join("/almalinux", fetch.LockFileName))
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("internal file status = %d", resp.StatusCode)
	}
}

// TestPrunableTree verifies a crawl only prunes a tree it wholly owns, so a
// repository at the mirror root cannot delete another mount's cached files.
func TestPrunableTree(t *testing.T) {
	serverFixture(t)

	conf := &cfg.Config{Mounts: []cfg.MountConfig{{Path: "/"}, {Path: "/other"}}}
	if prunableTree(conf, resource{Key: "rpm:/", Path: "/"}) {
		t.Error("a root repository may not prune a tree holding another mount")
	}
	if !prunableTree(conf, resource{Key: "rpm:/other/el9", Path: "/other/el9"}) {
		t.Error("a repository owning its own tree must prune")
	}

	// A repository nested under another one blocks the outer one's prune.
	single := &cfg.Config{Mounts: []cfg.MountConfig{{Path: "/"}}}
	state.S.MarkRequested("rpm", "rpm:/vendor/el9", "/vendor/el9", "/vendor/el9", time.Now())
	if prunableTree(single, resource{Key: "rpm:/vendor", Path: "/vendor"}) {
		t.Error("an outer repository may not prune a tree holding a nested one")
	}
	if !prunableTree(single, resource{Key: "rpm:/vendor/el9", Path: "/vendor/el9"}) {
		t.Error("a repository must not be blocked by its own registration")
	}
}

// TestServeRootRepositoryKeepsOtherMounts verifies a repository discovered at
// the mirror root does not prune content another mount cached beside it.
func TestServeRootRepositoryKeepsOtherMounts(t *testing.T) {
	// Two upstreams: a repository at its own root, and unrelated content.
	repoWWW := t.TempDir()
	testrepos.BuildRPMRepo(t, repoWWW)
	repoUpstream := testrepos.ServeDir(t, repoWWW)
	otherWWW := t.TempDir()
	testrepos.WriteFile(t, filepath.Join(otherWWW, "notes.txt"), []byte("other mount content"))
	otherUpstream := testrepos.ServeDir(t, otherWWW)

	onlineRoot := t.TempDir()
	confDir := t.TempDir()
	confPath := filepath.Join(confDir, "config.yaml")
	testrepos.WriteFile(t, confPath, []byte(fmt.Sprintf(`
state_path: %s/state.yaml
domains:
  - domain: 127.0.0.1
    role: online
    root: %s
mounts:
  - path: /
    upstream: %s
  - path: /other
    upstream: %s
`, confDir, onlineRoot, repoUpstream.URL, otherUpstream.URL)))
	if err := cfg.Init(confPath); err != nil {
		t.Fatal(err)
	}
	if err := state.Load(); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(Handler())
	t.Cleanup(srv.Close)
	t.Cleanup(WaitCrawls)

	// Cache a file from the unrelated mount, then crawl the root repository.
	if resp, _ := get(t, srv, "", "/other/notes.txt"); resp.StatusCode != http.StatusOK {
		t.Fatalf("notes status = %d", resp.StatusCode)
	}
	cached := filepath.Join(onlineRoot, "other", "notes.txt")
	if _, err := os.Stat(cached); err != nil {
		t.Fatal("unrelated file not cached:", err)
	}
	if resp, _ := get(t, srv, "", "/repodata/repomd.xml"); resp.StatusCode != http.StatusOK {
		t.Fatalf("repomd status = %d", resp.StatusCode)
	}
	WaitCrawls()

	if _, err := os.Stat(cached); err != nil {
		t.Error("root repository crawl pruned another mount's cached file:", err)
	}
}

// TestServeSiblingEntryPointMiss verifies a repository keeps its
// registration when a sibling entry-point path the upstream does not serve
// is requested. Every entry point of one repository shares a state key, and
// clients probe all of them: apt asks for InRelease, Release, and
// Release.gpg, and dnf asks for signature material beside repomd.xml.
func TestServeSiblingEntryPointMiss(t *testing.T) {
	srv, _, _ := serverFixture(t)

	for _, tc := range []struct{ found, missing, key string }{
		{"/debian/dists/test/Release", "/debian/dists/test/InRelease", "deb:/debian/dists/test"},
		{
			"/almalinux/9/BaseOS/x86_64/os/repodata/repomd.xml",
			"/almalinux/9/BaseOS/x86_64/os/repodata/repomd.xml.key",
			"rpm:/almalinux/9/BaseOS/x86_64/os",
		},
	} {
		if resp, _ := get(t, srv, "", tc.found); resp.StatusCode != http.StatusOK {
			t.Fatalf("%s status = %d", tc.found, resp.StatusCode)
		}
		if _, ok := state.S.Entry(tc.key); !ok {
			t.Fatalf("%s did not register %s", tc.found, tc.key)
		}
		if resp, _ := get(t, srv, "", tc.missing); resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s status = %d, want 404", tc.missing, resp.StatusCode)
		}
		if _, ok := state.S.Entry(tc.key); !ok {
			t.Errorf("%s deregistered %s", tc.missing, tc.key)
		}
	}
}

// TestServeRejectsUnverifiedRPMEntryPoint verifies the first request cannot
// serve cached repomd.xml before its required signature has been checked.
func TestServeRejectsUnverifiedRPMEntryPoint(t *testing.T) {
	www := t.TempDir()
	repoDir := filepath.Join(www, "repo")
	testrepos.BuildRPMRepo(t, repoDir)
	key := testrepos.NewSigningKey(t)
	key.SignRPMRepo(t, repoDir)
	repomd := filepath.Join(repoDir, "repodata", "repomd.xml")
	data, err := os.ReadFile(repomd)
	if err != nil {
		t.Fatal(err)
	}
	testrepos.WriteFile(t, repomd, append(data, '\n'))
	upstream := testrepos.ServeDir(t, www)

	onlineRoot := t.TempDir()
	local := filepath.Join(onlineRoot, "repo", "repodata", "repomd.xml")
	testrepos.WriteFile(t, local, []byte("unverified cached metadata"))
	confDir := t.TempDir()
	confPath := filepath.Join(confDir, "config.yaml")
	testrepos.WriteFile(t, confPath, []byte(fmt.Sprintf(`
state_path: %s/state.yaml
domains:
  - {domain: 127.0.0.1, role: online, root: %s}
mounts:
  - {path: /, upstream: %s}
crawler:
  signature_mode: required
`, confDir, onlineRoot, upstream.URL)))
	if err := cfg.Init(confPath); err != nil {
		t.Fatal(err)
	}
	if err := state.Load(); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(Handler())
	t.Cleanup(srv.Close)

	resp, _ := get(t, srv, "", "/repo/repodata/repomd.xml")
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("repomd status = %d, want 502", resp.StatusCode)
	}
	got, err := os.ReadFile(local)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "unverified cached metadata" {
		t.Error("failed verification replaced the cached repomd.xml")
	}
}

// TestServeRejectsUnverifiedDebEntryPoint verifies a cached InRelease cannot
// bypass the synchronous signature gate on its first request.
func TestServeRejectsUnverifiedDebEntryPoint(t *testing.T) {
	www := t.TempDir()
	repoDir := filepath.Join(www, "debian")
	testrepos.BuildDebRepo(t, repoDir)
	key := testrepos.NewSigningKey(t)
	key.SignDebRepo(t, repoDir)
	inRelease := filepath.Join(repoDir, "dists", "test", "InRelease")
	signedRelease, err := os.ReadFile(inRelease)
	require.NoError(t, err)
	tampered := bytes.Replace(signedRelease, []byte("Origin: Test"), []byte("Origin: Pest"), 1)
	require.NoError(t, os.WriteFile(inRelease, tampered, 0644))
	upstream := testrepos.ServeDir(t, www)

	onlineRoot := t.TempDir()
	local := filepath.Join(onlineRoot, "debian", "dists", "test", "InRelease")
	testrepos.WriteFile(t, local, []byte("unverified cached metadata"))
	confDir := t.TempDir()
	keyPath := filepath.Join(confDir, "repository.asc")
	testrepos.WriteFile(t, keyPath, key.PublicKey(t))
	confPath := filepath.Join(confDir, "config.yaml")
	testrepos.WriteFile(t, confPath, []byte(fmt.Sprintf(`
state_path: %s/state.yaml
domains:
  - {domain: 127.0.0.1, role: online, root: %s}
mounts:
  - {path: /, upstream: %s}
crawler:
  signature_mode: required
  gpg_keys: [%s]
  keyservers: []
`, confDir, onlineRoot, upstream.URL, keyPath)))
	require.NoError(t, cfg.Init(confPath))
	require.NoError(t, state.Load())
	srv := httptest.NewServer(Handler())
	t.Cleanup(srv.Close)

	resp, _ := get(t, srv, "", "/debian/dists/test/InRelease")
	assert.Equal(t, http.StatusBadGateway, resp.StatusCode)
	got, err := os.ReadFile(local)
	require.NoError(t, err)
	assert.Equal(t, []byte("unverified cached metadata"), got, "failed verification must retain the cached InRelease")
}

// TestServeUnresolvedEntryPoint verifies a path that only looks like a
// repository entry point is not left registered for the scheduler to crawl.
func TestServeUnresolvedEntryPoint(t *testing.T) {
	srv, _, _ := serverFixture(t)

	for _, tc := range []struct{ path, key string }{
		{"/nope/repodata/repomd.xml", "rpm:/nope"},
		{"/nope/dists/sid/InRelease", "deb:/nope/dists/sid"},
		{"/nope/database.db", "arch:/nope"},
		{"/nope/" + mirror.APKIndexName, "apk:/nope"},
	} {
		resp, _ := get(t, srv, "", tc.path)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s status = %d", tc.path, resp.StatusCode)
		}
		if _, ok := state.S.Entry(tc.key); ok {
			t.Errorf("%s left %s registered", tc.path, tc.key)
		}
	}
}

// archSignatureFixture serves one pacman repository, signed when a key is
// given, behind a mirror enforcing the given signature mode. Upstream files
// carry an hour-old modification time so a later rotation is visible to
// conditional requests, which http.FileServer compares at second precision.
func archSignatureFixture(t *testing.T, key *testrepos.SigningKey, mode string) (*httptest.Server, string, string) {
	t.Helper()
	www := t.TempDir()
	repoDir := filepath.Join(www, "archlinux", "core", "os", "x86_64")
	packages := testrepos.BuildArchRepo(t, repoDir, "core")
	if key != nil {
		key.SignArchRepo(t, repoDir, "core", packages)
	} else {
		// The fixture's placeholder package signatures would fail
		// verification; an unsigned upstream publishes none.
		for filename := range packages {
			require.NoError(t, os.Remove(filepath.Join(repoDir, filename+".sig")))
		}
	}
	old := time.Now().Add(-time.Hour)
	entries, err := os.ReadDir(repoDir)
	require.NoError(t, err)
	for _, entry := range entries {
		require.NoError(t, os.Chtimes(filepath.Join(repoDir, entry.Name()), old, old))
	}
	upstream := testrepos.ServeDir(t, www)

	onlineRoot := t.TempDir()
	confDir := t.TempDir()
	keys := "[]"
	if key != nil {
		keyPath := filepath.Join(confDir, "repository.asc")
		testrepos.WriteFile(t, keyPath, key.PublicKey(t))
		keys = "[" + keyPath + "]"
	}
	confPath := filepath.Join(confDir, "config.yaml")
	testrepos.WriteFile(t, confPath, []byte(fmt.Sprintf(`
state_path: %s/state.yaml
domains:
  - {domain: 127.0.0.1, role: online, root: %s}
mounts:
  - {path: /, upstream: %s}
crawler:
  signature_mode: %s
  gpg_keys: %s
  keyservers: []
`, confDir, onlineRoot, upstream.URL, mode, keys)))
	require.NoError(t, cfg.Init(confPath))
	require.NoError(t, state.Load())
	srv := httptest.NewServer(Handler())
	t.Cleanup(srv.Close)
	t.Cleanup(WaitCrawls)
	return srv, onlineRoot, repoDir
}

// TestServeArchSignatureEntryPoint verifies a pacman database signature is
// never refreshed on its own. pacman fetches core.db and then core.db.sig,
// so a mirror that revalidated the signature as a plain file after losing
// the repository's registration would pair a rotated signature with the
// database it still holds, and every client would fail verification until
// the next crawl. The signature must take the same verified path as
// Release.gpg and repomd.xml.asc.
func TestServeArchSignatureEntryPoint(t *testing.T) {
	key := testrepos.NewSigningKey(t)
	srv, onlineRoot, repoDir := archSignatureFixture(t, key, "required")
	repo := "/archlinux/core/os/x86_64"
	localDB := filepath.Join(onlineRoot, "archlinux", "core", "os", "x86_64", "core.db")
	localSig := localDB + ".sig"

	resp, _ := get(t, srv, "", repo+"/core.db")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.FileExists(t, localSig, "the verifying crawl publishes the database signature")

	// Model a restart that lost the state file: the tree is kept, the
	// registration is gone.
	state.S.Delete("arch:" + repo)

	// Rotate the upstream: the database gains an empty trailing gzip member,
	// which changes its bytes without changing its contents, and is re-signed.
	upstreamDB := filepath.Join(repoDir, "core.db")
	rotated, err := os.ReadFile(upstreamDB)
	require.NoError(t, err)
	var trailer bytes.Buffer
	require.NoError(t, gzip.NewWriter(&trailer).Close())
	rotated = append(rotated, trailer.Bytes()...)
	require.NoError(t, os.WriteFile(upstreamDB, rotated, 0644))
	key.SignArchRepo(t, repoDir, "core", nil)
	rotatedSig, err := os.ReadFile(upstreamDB + ".sig")
	require.NoError(t, err)

	resp, body := get(t, srv, "", repo+"/core.db.sig")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	gotDB, err := os.ReadFile(localDB)
	require.NoError(t, err)
	assert.Equal(t, rotated, gotDB, "a signature request must publish the database it was verified against")
	assert.Equal(t, rotatedSig, body, "the rotated signature is served once its pair is verified")
	_, registered := state.S.Entry("arch:" + repo)
	assert.True(t, registered, "a signature request registers the repository it belongs to")
}

// TestServeProtectedFirstContactMiss verifies a repository whose first
// request is an entry point the upstream does not publish stays registered
// once its crawl succeeded. Deregistering it would hand every member file
// to the generic path, which refreshes files individually with no checksum
// or signature check, until a later entry-point request re-registers it.
func TestServeProtectedFirstContactMiss(t *testing.T) {
	srv, onlineRoot, _ := archSignatureFixture(t, nil, "if-present")
	repo := "/archlinux/core/os/x86_64"

	resp, _ := get(t, srv, "", repo+"/core.db.sig")
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, "the upstream publishes no database signature")
	assert.FileExists(t, filepath.Join(onlineRoot, "archlinux", "core", "os", "x86_64", "core.db"), "the crawl published the repository")
	_, registered := state.S.Entry("arch:" + repo)
	assert.True(t, registered, "a crawled repository stays registered after a sibling entry-point miss")
}

// TestEvictStaleKeepsRepositoryMember verifies evicting a plain-file entry
// leaves the file alone when a registered repository owns it. A file
// requested before its repository was registered is tracked as generic;
// deleting it from under the verified tree would force the next request into
// a full synchronous crawl to restore it.
func TestEvictStaleKeepsRepositoryMember(t *testing.T) {
	_, onlineRoot, _ := serverFixture(t)
	repo := "/archlinux/core/os/x86_64"
	member := repo + "/zlib-1.3-1-x86_64.pkg.tar.zst"
	local := filepath.Join(onlineRoot, filepath.FromSlash(member[1:]))
	testrepos.WriteFile(t, local, []byte("package"))

	now := time.Now()
	state.S.MarkRequested(kindGeneric, kindGeneric+":"+member, member, "", now.Add(-48*time.Hour))
	state.S.MarkRequested(string(mirror.RepoArch), "arch:"+repo, repo, repo, now)
	entry, _ := state.S.Entry(kindGeneric + ":" + member)
	evictStale(kindGeneric+":"+member, entry)

	assert.FileExists(t, local, "a repository member must survive eviction of its stale generic entry")
	_, tracked := state.S.Entry(kindGeneric + ":" + member)
	assert.False(t, tracked, "the stale generic entry is dropped")
}

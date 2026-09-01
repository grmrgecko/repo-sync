package mirror

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/grmrgecko/repo-sync/fetch"
	"github.com/grmrgecko/repo-sync/internal/testrepos"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

// TestSyncRPMMissingKeyForbidden verifies S3's 403 response for a missing
// optional key does not abort synchronization.
func TestSyncRPMMissingKeyForbidden(t *testing.T) {
	www := t.TempDir()
	repoDir := filepath.Join(www, "repos", "el9")
	testrepos.BuildRPMRepo(t, repoDir)

	files := http.FileServer(http.Dir(www))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/el9/repodata/repomd.xml.key" {
			http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
			return
		}
		files.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	dest := t.TempDir()
	opts := &Options{Type: RepoRPM, Destination: dest, Workers: 2}
	err := syncOne(context.Background(), srv.URL+"/repos/el9", opts.Type, opts)
	require.NoError(t, err)

	local := filepath.Join(dest, "repos", "el9", "repodata")
	assert.FileExists(t, filepath.Join(local, "repomd.xml"))
	assert.FileExists(t, filepath.Join(local, "repomd.xml.asc"))
	assert.NoFileExists(t, filepath.Join(local, "repomd.xml.key"))
}

// TestSyncRPMSignedPair verifies repository keys and keyserver retrieval,
// retries a split upstream rotation, and preserves the live pair when the
// upstream remains inconsistent.
func TestSyncRPMSignedPair(t *testing.T) {
	www := t.TempDir()
	repoDir := filepath.Join(www, "repos", "el9")
	testrepos.BuildRPMRepo(t, repoDir)
	key := testrepos.NewSigningKey(t)
	key.SignRPMRepo(t, repoDir)

	repomdPath := filepath.Join(repoDir, "repodata", "repomd.xml")
	firstRepomd, err := os.ReadFile(repomdPath)
	require.NoError(t, err)
	firstSig := key.Sign(t, firstRepomd)

	var phase atomic.Int32
	var signatureRequests atomic.Int32
	secondRepomd := append(append([]byte(nil), firstRepomd...), '\n')
	secondSig := key.Sign(t, secondRepomd)
	thirdRepomd := append(append([]byte(nil), secondRepomd...), '\n')
	files := http.FileServer(http.Dir(www))
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/el9/repodata/repomd.xml":
			switch phase.Load() {
			case 0:
				_, _ = w.Write(firstRepomd)
			case 1:
				_, _ = w.Write(secondRepomd)
			default:
				_, _ = w.Write(thirdRepomd)
			}
		case "/repos/el9/repodata/repomd.xml.asc":
			request := signatureRequests.Add(1)
			if phase.Load() == 1 && request > 1 {
				_, _ = w.Write(secondSig)
				return
			}
			if phase.Load() == 2 {
				_, _ = w.Write(secondSig)
				return
			}
			_, _ = w.Write(firstSig)
		default:
			files.ServeHTTP(w, r)
		}
	}))
	t.Cleanup(upstream.Close)

	dest := t.TempDir()
	opts := &Options{
		Type:          RepoRPM,
		Destination:   dest,
		Workers:       2,
		SignatureMode: SignatureIfPresent,
	}
	repoURL := upstream.URL + "/repos/el9"
	require.NoError(t, syncOne(context.Background(), repoURL, opts.Type, opts))

	local := filepath.Join(dest, "repos", "el9", "repodata")
	assert.Equal(t, firstRepomd, requireReadFile(t, filepath.Join(local, "repomd.xml")))

	// Remove the adjacent key so the rotation resolves its signer through
	// the configured keyserver.
	require.NoError(t, os.Remove(filepath.Join(repoDir, "repodata", "repomd.xml.key")))
	publicKey := key.PublicKey(t)
	otherKey := testrepos.NewSigningKey(t)
	otherPublicKey := otherKey.PublicKey(t)
	unrelatedKeyserver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(otherPublicKey)
	}))
	t.Cleanup(unrelatedKeyserver.Close)
	keyserver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "get", r.URL.Query().Get("op"))
		assert.NotEmpty(t, r.URL.Query().Get("search"))
		_, _ = w.Write(publicKey)
	}))
	t.Cleanup(keyserver.Close)
	opts.Keyservers = []string{unrelatedKeyserver.URL, keyserver.URL}
	phase.Store(1)
	signatureRequests.Store(0)
	require.NoError(t, syncOne(context.Background(), repoURL, opts.Type, opts))
	assert.GreaterOrEqual(t, signatureRequests.Load(), int32(2))
	assert.Equal(t, secondRepomd, requireReadFile(t, filepath.Join(local, "repomd.xml")))
	assert.Equal(t, secondSig, requireReadFile(t, filepath.Join(local, "repomd.xml.asc")))

	phase.Store(2)
	signatureRequests.Store(0)
	err = syncOne(context.Background(), repoURL, opts.Type, opts)
	require.Error(t, err)
	assert.Equal(t, secondRepomd, requireReadFile(t, filepath.Join(local, "repomd.xml")))
	assert.Equal(t, secondSig, requireReadFile(t, filepath.Join(local, "repomd.xml.asc")))
	assert.NoFileExists(t, filepath.Join(local, "repomd.xml"+fetch.StagedSuffix))

	// Configured keyrings pin the accepted signer, so repository and
	// keyserver keys cannot override an operator-managed key.
	keyPath := filepath.Join(t.TempDir(), "trusted.asc")
	testrepos.WriteFile(t, keyPath, otherPublicKey)
	opts.GPGKeys = []string{keyPath}
	phase.Store(1)
	signatureRequests.Store(1)
	err = syncOne(context.Background(), repoURL, opts.Type, opts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not present in the configured GPG keys")
	assert.Equal(t, secondRepomd, requireReadFile(t, filepath.Join(local, "repomd.xml")))
}

// requireReadFile reads an asserted fixture result.
func requireReadFile(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(name)
	require.NoError(t, err)
	return data
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

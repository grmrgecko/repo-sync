package fetch

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/grmrgecko/repo-sync/internal/testrepos"
)

// resumeFixture serves a distinct-content file and records the Range
// headers of incoming requests.
func resumeFixture(t *testing.T, stripRange bool) (*Source, []byte, *[]string) {
	t.Helper()
	content := make([]byte, 3000)
	for i := range content {
		content[i] = byte(i % 251)
	}
	www := t.TempDir()
	testrepos.WriteFile(t, filepath.Join(www, "big.bin"), content)

	var mu sync.Mutex
	ranges := &[]string{}
	fs := http.FileServer(http.Dir(www))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		*ranges = append(*ranges, r.Header.Get("Range"))
		mu.Unlock()
		if stripRange {
			r.Header.Del("Range")
		}
		fs.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return NewSource([]string{srv.URL}), content, ranges
}

// TestFetchFileResume verifies a valid partial file is completed with a
// ranged request.
func TestFetchFileResume(t *testing.T) {
	src, content, ranges := resumeFixture(t, false)
	want := &Expect{Size: int64(len(content)), Sums: map[string]string{"sha256": testrepos.SHA256Hex(content)}}

	dst := filepath.Join(t.TempDir(), "big.bin")
	testrepos.WriteFile(t, dst+PartialSuffix, content[:1000])
	state, err := File(context.Background(), src, "big.bin", dst, want, false, false)
	if err != nil || state != FileFetched {
		t.Fatalf("fetch = %v, %v", state, err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Error("resumed content mismatch")
	}
	if len(*ranges) != 1 || (*ranges)[0] != "bytes=1000-" {
		t.Errorf("ranges = %v, want one bytes=1000- request", *ranges)
	}
}

// TestFetchFileResumeIgnored verifies a server that ignores ranges still
// produces a correct full download.
func TestFetchFileResumeIgnored(t *testing.T) {
	src, content, _ := resumeFixture(t, true)
	want := &Expect{Size: int64(len(content)), Sums: map[string]string{"sha256": testrepos.SHA256Hex(content)}}

	dst := filepath.Join(t.TempDir(), "big.bin")
	testrepos.WriteFile(t, dst+PartialSuffix, content[:1000])
	state, err := File(context.Background(), src, "big.bin", dst, want, false, false)
	if err != nil || state != FileFetched {
		t.Fatalf("fetch = %v, %v", state, err)
	}
	got, _ := os.ReadFile(dst)
	if !bytes.Equal(got, content) {
		t.Error("content mismatch after ignored range")
	}
}

// TestFetchFileResumeBadPartial verifies a partial that is not a prefix of
// the upstream file falls back to a full download.
func TestFetchFileResumeBadPartial(t *testing.T) {
	src, content, ranges := resumeFixture(t, false)
	want := &Expect{Size: int64(len(content)), Sums: map[string]string{"sha256": testrepos.SHA256Hex(content)}}

	dst := filepath.Join(t.TempDir(), "big.bin")
	testrepos.WriteFile(t, dst+PartialSuffix, bytes.Repeat([]byte("X"), 1000))
	state, err := File(context.Background(), src, "big.bin", dst, want, false, false)
	if err != nil || state != FileFetched {
		t.Fatalf("fetch = %v, %v", state, err)
	}
	got, _ := os.ReadFile(dst)
	if !bytes.Equal(got, content) {
		t.Error("content mismatch after bad partial")
	}
	if len(*ranges) != 2 || (*ranges)[1] != "" {
		t.Errorf("ranges = %v, want a ranged then a full request", *ranges)
	}
}

// TestFetchFileConditional verifies files without published checksums are
// revalidated with conditional requests: unchanged upstreams are kept, and
// changed upstreams are fetched again.
func TestFetchFileConditional(t *testing.T) {
	www := t.TempDir()
	upstream := filepath.Join(www, "meta.tar.gz")
	testrepos.WriteFile(t, upstream, []byte("metadata"))
	srv := testrepos.ServeDir(t, www)
	src := NewSource([]string{srv.URL})

	dst := filepath.Join(t.TempDir(), "meta.tar.gz")
	state, err := File(context.Background(), src, "meta.tar.gz", dst, nil, true, false)
	if err != nil || state != FileStaged {
		t.Fatalf("first fetch = %v, %v", state, err)
	}
	if err := PromoteStaged(dst); err != nil {
		t.Fatal(err)
	}

	// An unchanged upstream answers the conditional request with 304.
	state, err = File(context.Background(), src, "meta.tar.gz", dst, nil, true, false)
	if err != nil || state != FileUnchanged {
		t.Errorf("unchanged fetch = %v, %v", state, err)
	}

	// A newer upstream file must be fetched again. The modification time
	// is pushed forward because Last-Modified has one-second resolution.
	testrepos.WriteFile(t, upstream, []byte("metadata-v2"))
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(upstream, future, future); err != nil {
		t.Fatal(err)
	}
	state, err = File(context.Background(), src, "meta.tar.gz", dst, nil, true, false)
	if err != nil || state != FileStaged {
		t.Fatalf("changed fetch = %v, %v", state, err)
	}
	got, err := os.ReadFile(dst + StagedSuffix)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "metadata-v2" {
		t.Errorf("staged content = %q", got)
	}
}

// TestFetchFileConcurrent verifies concurrent downloads to the same
// destination are serialized: no caller's partial download can interleave
// with another's.
func TestFetchFileConcurrent(t *testing.T) {
	src, content, _ := resumeFixture(t, false)
	want := &Expect{Size: int64(len(content)), Sums: map[string]string{"sha256": testrepos.SHA256Hex(content)}}

	dst := filepath.Join(t.TempDir(), "big.bin")
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := File(context.Background(), src, "big.bin", dst, want, false, true); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Error("content mismatch after concurrent downloads")
	}
}

// TestMain initializes the shared HTTP client all fetch paths depend on.
func TestMain(m *testing.M) {
	Reload()
	os.Exit(m.Run())
}

// TestLocalJoin verifies traversal attempts cannot escape the destination.
func TestLocalJoin(t *testing.T) {
	root := t.TempDir()
	got, err := LocalJoin(root, "../../etc/passwd")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "etc", "passwd")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if _, err := LocalJoin(root, "a/../b/c.rpm"); err != nil {
		t.Errorf("clean relative path rejected: %v", err)
	}
	got, err = LocalJoin("/", "etc/passwd")
	if err != nil {
		t.Fatalf("filesystem root destination rejected: %v", err)
	}
	if got != "/etc/passwd" {
		t.Errorf("got %q, want %q", got, "/etc/passwd")
	}
}

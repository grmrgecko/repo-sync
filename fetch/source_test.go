package fetch

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/grmrgecko/repo-sync/internal/testrepos"
)

// TestParseMirrorlist verifies URL extraction, comment skipping, and
// deduplication.
func TestParseMirrorlist(t *testing.T) {
	body := "# comment\n\nhttp://mirror1.example.com/repo/\nhttps://mirror2.example.com/repo\nhttp://mirror1.example.com/repo\n/bare/path\nnot a url\n"
	bases := ParseMirrorlist([]byte(body))
	want := []string{"http://mirror1.example.com/repo", "https://mirror2.example.com/repo"}
	if len(bases) != len(want) {
		t.Fatalf("got %v, want %v", bases, want)
	}
	for i := range want {
		if bases[i] != want[i] {
			t.Errorf("bases[%d] = %q, want %q", i, bases[i], want[i])
		}
	}
}

// TestSourceFailover verifies a dead first mirror fails over to a working
// one and that missing files surface the not-found sentinel.
func TestSourceFailover(t *testing.T) {
	www := t.TempDir()
	testrepos.BuildRPMRepo(t, filepath.Join(www, "el9"))
	srv := testrepos.ServeDir(t, www)

	src := NewSource([]string{"http://127.0.0.1:1/el9", srv.URL + "/el9"})
	resp, err := src.Get(context.Background(), "repodata/repomd.xml", GetOptions{})
	if err != nil {
		t.Fatal("failover did not reach the working mirror:", err)
	}
	resp.Body.Close()
	if src.active != 1 {
		t.Errorf("active mirror = %d, want 1", src.active)
	}

	if _, err := src.Get(context.Background(), "no/such/file", GetOptions{}); !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

// TestParseMetalink verifies base URL extraction from metalink documents.
func TestParseMetalink(t *testing.T) {
	body := `<?xml version="1.0"?><metalink xmlns="urn:ietf:params:xml:ns:metalink">` +
		`<files><file name="repomd.xml"><resources>` +
		`<url protocol="https" type="https">https://mirror1.example.com/fedora/repodata/repomd.xml</url>` +
		`<url protocol="rsync">rsync://mirror2.example.com/fedora/repodata/repomd.xml</url>` +
		`<url protocol="http">http://mirror3.example.com/fedora/repodata/repomd.xml</url>` +
		`<url protocol="https">https://mirror1.example.com/fedora/repodata/repomd.xml</url>` +
		`</resources></file></files></metalink>`
	bases := ParseMetalink([]byte(body))
	want := []string{"https://mirror1.example.com/fedora", "http://mirror3.example.com/fedora"}
	if len(bases) != len(want) {
		t.Fatalf("got %v, want %v", bases, want)
	}
	for i := range want {
		if bases[i] != want[i] {
			t.Errorf("bases[%d] = %q, want %q", i, bases[i], want[i])
		}
	}
	if ParseMetalink([]byte("not xml at all")) != nil {
		t.Error("non-XML input produced bases")
	}
}

// TestSourceNoBases verifies a Source with no mirrors reports an error and
// no response.
func TestSourceNoBases(t *testing.T) {
	src := NewSource(nil)
	resp, err := src.Get(context.Background(), "anything", GetOptions{})
	if err == nil {
		t.Fatal("expected an error for a source without bases")
	}
	if resp != nil {
		t.Error("response must be nil on error")
	}
}

// TestSourceRebase verifies suffix trimming for suite-relative sources.
func TestSourceRebase(t *testing.T) {
	src := NewSource([]string{"http://example.com/debian/dists/test", "http://other.example.com/mirror"})
	src.Rebase("dists/test")
	if src.bases[0] != "http://example.com/debian" {
		t.Errorf("bases[0] = %q", src.bases[0])
	}
	if src.bases[1] != "http://other.example.com/mirror" {
		t.Errorf("bases[1] changed unexpectedly: %q", src.bases[1])
	}
}

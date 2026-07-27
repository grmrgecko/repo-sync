package mirror

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grmrgecko/repo-sync/fetch"
	"github.com/grmrgecko/repo-sync/internal/testrepos"
)

// TestResolveSource verifies direct repositories are used as-is and
// mirrorlist bodies expand into multi-mirror sources.
func TestResolveSource(t *testing.T) {
	www := t.TempDir()
	testrepos.BuildRPMRepo(t, filepath.Join(www, "el9"))
	srv := testrepos.ServeDir(t, www)

	// The mirrorlist needs the real server URL, which is only known now
	// that the listener is up; files are read per-request.
	testrepos.WriteFile(t, filepath.Join(www, "ml.txt"),
		[]byte("http://127.0.0.1:1/el9\n"+srv.URL+"/el9\n"))

	direct, layoutURL, err := resolveSource(context.Background(), srv.URL+"/el9", RepoRPM)
	if err != nil {
		t.Fatal(err)
	}
	if len(direct.Bases()) != 1 {
		t.Errorf("direct bases = %v", direct.Bases())
	}
	if layoutURL != srv.URL+"/el9" {
		t.Errorf("direct layout URL = %q", layoutURL)
	}

	ml, layoutURL, err := resolveSource(context.Background(), srv.URL+"/ml.txt", RepoRPM)
	if err != nil {
		t.Fatal(err)
	}
	if len(ml.Bases()) != 2 {
		t.Errorf("mirrorlist bases = %v", ml.Bases())
	}
	if layoutURL != "http://127.0.0.1:1/el9" {
		t.Errorf("mirrorlist layout URL = %q", layoutURL)
	}

	if _, _, err := resolveSource(context.Background(), srv.URL+"/el9/Packages", RepoRPM); err == nil {
		t.Error("expected error for a non-repository URL")
	}
}

// TestResolveSourceQueryMirrorlist verifies a mirrorlist URL carrying a
// query string is read as a mirrorlist rather than probed as a repository.
// A repository path cannot be joined onto such a URL: it would land inside
// the query, and endpoints that answer 200 with an error page for an
// unrecognized parameter would then look like a repository.
func TestResolveSourceQueryMirrorlist(t *testing.T) {
	www := t.TempDir()
	testrepos.BuildRPMRepo(t, filepath.Join(www, "el9"))
	repo := testrepos.ServeDir(t, www)

	var probed []string
	list := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probed = append(probed, r.URL.String())
		if r.URL.Query().Get("repo") != "os" {
			// Several real mirrorlist services answer 200 with a message.
			w.Write([]byte("Invalid release/repo/arch combination\n"))
			return
		}
		w.Write([]byte(repo.URL + "/el9\n"))
	}))
	t.Cleanup(list.Close)

	src, layoutURL, err := resolveSource(context.Background(), list.URL+"/?release=9&repo=os", RepoRPM)
	if err != nil {
		t.Fatal(err)
	}
	if bases := src.Bases(); len(bases) != 1 || bases[0] != repo.URL+"/el9" {
		t.Errorf("bases = %v, want [%s]", bases, repo.URL+"/el9")
	}
	if layoutURL != repo.URL+"/el9" {
		t.Errorf("layout URL = %q, want %q", layoutURL, repo.URL+"/el9")
	}
	for _, u := range probed {
		if strings.Contains(u, "repodata") {
			t.Errorf("probe folded a repository path into the query: %s", u)
		}
	}
}

// TestResolveSourceMetalink verifies metalink documents expand into
// multi-mirror sources for RPM repositories.
func TestResolveSourceMetalink(t *testing.T) {
	www := t.TempDir()
	testrepos.BuildRPMRepo(t, filepath.Join(www, "el9"))
	srv := testrepos.ServeDir(t, www)

	metalink := `<?xml version="1.0"?><metalink><files><file name="repomd.xml"><resources>` +
		`<url protocol="http">http://127.0.0.1:1/dead/repodata/repomd.xml</url>` +
		`<url protocol="http">` + srv.URL + `/el9/repodata/repomd.xml</url>` +
		`</resources></file></files></metalink>`
	testrepos.WriteFile(t, filepath.Join(www, "metalink.xml"), []byte(metalink))

	src, layoutURL, err := resolveSource(context.Background(), srv.URL+"/metalink.xml", RepoRPM)
	if err != nil {
		t.Fatal(err)
	}
	if len(src.Bases()) != 2 || src.Bases()[1] != srv.URL+"/el9" {
		t.Errorf("bases = %v", src.Bases())
	}
	if layoutURL != "http://127.0.0.1:1/dead" {
		t.Errorf("layout URL = %q", layoutURL)
	}

	// The dead first mirror must fail over to the live one.
	resp, err := src.Get(context.Background(), "repodata/repomd.xml", fetch.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

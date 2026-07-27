package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	cfg "github.com/grmrgecko/repo-sync/config"
	"github.com/grmrgecko/repo-sync/internal/testrepos"
	"github.com/grmrgecko/repo-sync/mirror"
	"github.com/grmrgecko/repo-sync/state"
)

// traceFixture builds an upstream apk repository that publishes a trace of
// its own, configures the mirror server against it with tracing enabled,
// and returns the mirror's test server and the online root.
func traceFixture(t *testing.T) (*httptest.Server, string) {
	t.Helper()

	www := t.TempDir()
	repo := filepath.Join(www, "alpine", "v3.24", "main", "x86_64")
	testrepos.BuildApkRepo(t, repo)
	testrepos.WriteFile(t, filepath.Join(repo, filepath.FromSlash(mirror.TraceDir), "origin.example.net"),
		[]byte("origin trace\n"))
	upstream := testrepos.ServeDir(t, www)

	onlineRoot := t.TempDir()
	confDir := t.TempDir()
	conf := fmt.Sprintf(`
state_path: %s/state.yaml
domains:
  - domain: 127.0.0.1
    role: online
    root: %s
mounts:
  - path: /
    upstream: %s
trace:
  enabled: true
  host: mirror.example.com
  maintainer: Test Maintainer <test@example.com>
`, confDir, onlineRoot, upstream.URL)
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
	t.Cleanup(WaitCrawls)
	return srv, onlineRoot
}

// TestServeCrawlWritesTrace verifies a server crawl publishes this mirror's
// trace and mirrors upstream's, and that both are then served like any
// other repository file.
func TestServeCrawlWritesTrace(t *testing.T) {
	srv, onlineRoot := traceFixture(t)

	repo := "/alpine/v3.24/main/x86_64"
	resp, _ := get(t, srv, "", repo+"/APKINDEX.tar.gz")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("index status = %d", resp.StatusCode)
	}

	traceDir := filepath.Join(onlineRoot, filepath.FromSlash(repo[1:]), filepath.FromSlash(mirror.TraceDir))
	own := filepath.Join(traceDir, "mirror.example.com")
	waitFor(t, "crawl trace", func() bool {
		_, err := os.Stat(own)
		return err == nil
	})

	data, err := os.ReadFile(own)
	if err != nil {
		t.Fatal("reading trace:", err)
	}
	trace := string(data)
	for _, want := range []string{
		"Running on host: mirror.example.com",
		"Maintainer: Test Maintainer <test@example.com>",
		"Repository type: apk",
		"Total bytes received: ",
	} {
		if !strings.Contains(trace, want) {
			t.Errorf("trace missing %q\ngot:\n%s", want, trace)
		}
	}
	if upstreamTrace, err := os.ReadFile(filepath.Join(traceDir, "origin.example.net")); err != nil {
		t.Error("upstream trace not mirrored:", err)
	} else if string(upstreamTrace) != "origin trace\n" {
		t.Errorf("upstream trace = %q, want %q", upstreamTrace, "origin trace\n")
	}

	// The published traces are served from the crawled tree.
	resp, body := get(t, srv, "", repo+"/"+mirror.TraceDir+"/mirror.example.com")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("trace request status = %d", resp.StatusCode)
	}
	if string(body) != trace {
		t.Errorf("served trace does not match the published file\ngot:\n%s", body)
	}
}

// TestTraceInfo verifies the configured trace section maps onto the mirror
// package's description of this mirror, including the host fallback.
func TestTraceInfo(t *testing.T) {
	got := traceInfo(cfg.TraceConfig{
		Host:       "mirror.example.com",
		Maintainer: "Jane <jane@example.com>",
		Sponsor:    "Example Inc",
		Country:    "US",
		Location:   "Texas",
		Throughput: "10Gb",
	})
	want := mirror.TraceInfo{
		Host:       "mirror.example.com",
		Maintainer: "Jane <jane@example.com>",
		Sponsor:    "Example Inc",
		Country:    "US",
		Location:   "Texas",
		Throughput: "10Gb",
	}
	if got != want {
		t.Errorf("traceInfo = %+v, want %+v", got, want)
	}
	if host := traceInfo(cfg.TraceConfig{}).Host; host == "" {
		t.Error("traceInfo left the host empty; the system hostname should stand in")
	}
}

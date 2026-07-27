package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	cfg "github.com/grmrgecko/repo-sync/config"
)

// initConfig installs a minimal configuration with the given state path.
func initConfig(t *testing.T, statePath string) {
	t.Helper()
	conf := `
state_path: ` + statePath + `
domains:
  - {domain: m.test, role: online, root: /tmp/online}
mounts:
  - {path: /, upstream: "https://example.com"}
`
	name := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(name, []byte(conf), 0644); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Init(name); err != nil {
		t.Fatal(err)
	}
}

// TestLoadMissing verifies a missing state file yields a fresh store.
func TestLoadMissing(t *testing.T) {
	initConfig(t, filepath.Join(t.TempDir(), "state.yaml"))
	if err := Load(); err != nil {
		t.Fatal(err)
	}
	if len(S.Snapshot()) != 0 {
		t.Error("fresh store is not empty")
	}
}

// TestLoadUnreadable verifies read failures other than a missing file are
// reported to the caller.
func TestLoadUnreadable(t *testing.T) {
	initConfig(t, t.TempDir())
	if err := Load(); err == nil {
		t.Fatal("expected an error for an unreadable state path")
	}
}

// TestLoadNullEntryMap verifies a state file whose entries mapping is null
// yields a writable store.
func TestLoadNullEntryMap(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.yaml")
	if err := os.WriteFile(statePath, []byte("entries:\n"), 0644); err != nil {
		t.Fatal(err)
	}
	initConfig(t, statePath)
	if err := Load(); err != nil {
		t.Fatal(err)
	}
	S.MarkRequested("rpm", "rpm:/repo", "/repo", "/repo", time.Now())
	if _, ok := S.Entry("rpm:/repo"); !ok {
		t.Error("entry not recorded after loading a null entries mapping")
	}
}

// TestLoadNullEntries verifies hand-edited null entries are dropped at load
// so later lookups only see valid entries.
func TestLoadNullEntries(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.yaml")
	content := `
entries:
  rpm:/repo: null
  deb:/suite:
    kind: deb
    path: /suite
`
	if err := os.WriteFile(statePath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	initConfig(t, statePath)
	if err := Load(); err != nil {
		t.Fatal(err)
	}
	if _, ok := S.Entry("rpm:/repo"); ok {
		t.Error("null entry survived load")
	}
	if _, ok := S.Entry("deb:/suite"); !ok {
		t.Error("valid entry lost on load")
	}
	S.TouchRepoMembers("/repo/repodata/repomd.xml", time.Now())
}

package config

import (
	"os"
	"path/filepath"
	"testing"
)

// writeConfig writes a temp config file and returns its path.
func writeConfig(t *testing.T, content string) string {
	t.Helper()
	name := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(name, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return name
}

// TestInitExample verifies the shipped example configuration is valid.
func TestInitExample(t *testing.T) {
	if err := Init("../config.example.yaml"); err != nil {
		t.Fatal(err)
	}
	if _, ok := C.Domain("mirror.example.com:443"); !ok {
		t.Error("online domain not resolvable with a port")
	}
	if C.OnlineDomain().Domain != "mirror.example.com" {
		t.Errorf("online domain = %q", C.OnlineDomain().Domain)
	}
	mount, ok := C.MountFor("/almalinux/9/BaseOS/x86_64/os")
	if !ok || mount.Path != "/almalinux" {
		t.Errorf("mount = %+v, %v", mount, ok)
	}
}

// TestStatePathDefault verifies an unset state path lands next to the
// config file.
func TestStatePathDefault(t *testing.T) {
	path := writeConfig(t, `
domains:
  - {domain: m.test, role: online, root: /tmp/online}
mounts:
  - {path: /, upstream: "https://example.com"}
`)
	if err := Init(path); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(filepath.Dir(path), "state.yaml")
	if C.StatePath != want {
		t.Errorf("StatePath = %q, want %q", C.StatePath, want)
	}
}

// TestMountLongestPrefix verifies the most specific mount wins.
func TestMountLongestPrefix(t *testing.T) {
	path := writeConfig(t, `
state_path: /tmp/state.yaml
domains:
  - {domain: m.test, role: online, root: /tmp/online}
mounts:
  - {path: /, upstream: "https://fallback.example.com"}
  - {path: /centos, upstream: "https://centos.example.com"}
  - {path: /centos/vault, upstream: "https://vault.example.com"}
`)
	if err := Init(path); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"/centos/vault/7/os":  "https://vault.example.com",
		"/centos/8/BaseOS":    "https://centos.example.com",
		"/debian/dists/testy": "https://fallback.example.com",
	}
	for reqPath, want := range cases {
		mount, ok := C.MountFor(reqPath)
		if !ok || mount.Upstream != want {
			t.Errorf("MountFor(%q) = %q, want %q", reqPath, mount.Upstream, want)
		}
	}
}

// TestInitServerRejections verifies invalid server configurations fail
// with clear errors.
func TestInitServerRejections(t *testing.T) {
	cases := map[string]string{
		"two online domains": `
state_path: /tmp/state.yaml
domains:
  - {domain: a.test, role: online, root: /tmp/a}
  - {domain: b.test, role: online, root: /tmp/b}
mounts:
  - {path: /, upstream: "https://example.com"}
`,
		"no online domain": `
state_path: /tmp/state.yaml
domains:
  - {domain: a.test, role: offline, root: /tmp/a}
mounts:
  - {path: /, upstream: "https://example.com"}
`,
		"repo outside mount": `
state_path: /tmp/state.yaml
domains:
  - {domain: a.test, role: online, root: /tmp/a}
mounts:
  - path: /centos
    upstream: "https://example.com"
    repos:
      - {path: /debian/dists/x, type: deb}
`,
		"bad repo type": `
state_path: /tmp/state.yaml
domains:
  - {domain: a.test, role: online, root: /tmp/a}
mounts:
  - path: /centos
    upstream: "https://example.com"
    repos:
      - {path: /centos/7/os, type: gentoo}
`,
	}
	for name, content := range cases {
		if err := InitServer(writeConfig(t, content)); err == nil {
			t.Errorf("%s: configuration unexpectedly accepted", name)
		}
	}
}

// TestInitWithoutServerSections verifies a configuration carrying no
// domains or mounts loads for the sync commands but is refused for the
// server.
func TestInitWithoutServerSections(t *testing.T) {
	path := writeConfig(t, "state_path: /tmp/state.yaml\ncrawler:\n  workers: 9\n")
	if err := Init(path); err != nil {
		t.Fatalf("Init rejected a sync-only configuration: %v", err)
	}
	if C.Crawler.Workers != 9 {
		t.Errorf("Crawler.Workers = %d, want 9", C.Crawler.Workers)
	}
	if err := InitServer(path); err == nil {
		t.Error("InitServer accepted a configuration with no domains")
	}
}

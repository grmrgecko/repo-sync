// Package testrepos builds small fixture repositories for tests across
// the fetch, mirror, and server packages.
package testrepos

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// WriteFile creates a file with parent directories under a fixture tree.
func WriteFile(t *testing.T, name string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, data, 0644); err != nil {
		t.Fatal(err)
	}
}

// GzipBytes compresses data with gzip for index fixtures.
func GzipBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// SHA256Hex returns the hex SHA-256 of data.
func SHA256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// MD5Hex returns the hex MD5 of data.
func MD5Hex(data []byte) string {
	sum := md5.Sum(data)
	return hex.EncodeToString(sum[:])
}

// ServeDir serves a fixture tree over HTTP with directory listings, closing
// the server when the test finishes.
func ServeDir(t *testing.T, dir string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.FileServer(http.Dir(dir)))
	t.Cleanup(srv.Close)
	return srv
}

// BuildRPMRepo writes a minimal yum repository into dir and returns the
// package contents by file name.
func BuildRPMRepo(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	pkgs := map[string][]byte{
		"foo-1.0-1.x86_64.rpm": bytes.Repeat([]byte("foo"), 500),
		"bar-2.0-1.x86_64.rpm": bytes.Repeat([]byte("bar"), 700),
	}
	var pkgXML bytes.Buffer
	for name, data := range pkgs {
		WriteFile(t, filepath.Join(dir, "Packages", name), data)
		fmt.Fprintf(&pkgXML,
			`<package type="rpm"><name>%s</name><checksum type="sha256" pkgid="YES">%s</checksum><size package="%d"/><location href="Packages/%s"/></package>`,
			name, SHA256Hex(data), len(data), name)
	}
	primary := []byte(`<?xml version="1.0"?><metadata packages="2">` + pkgXML.String() + `</metadata>`)
	primaryGz := GzipBytes(t, primary)
	primaryName := SHA256Hex(primaryGz) + "-primary.xml.gz"
	WriteFile(t, filepath.Join(dir, "repodata", primaryName), primaryGz)

	// A prestodelta index references one delta package under drpms/.
	drpm := RPMDelta()
	WriteFile(t, filepath.Join(dir, "drpms", "foo-0.9_1.0-1.x86_64.drpm"), drpm)
	presto := []byte(fmt.Sprintf(
		`<?xml version="1.0"?><prestodelta><newpackage name="foo"><delta oldversion="0.9"><filename>drpms/foo-0.9_1.0-1.x86_64.drpm</filename><size>%d</size><checksum type="sha256">%s</checksum></delta></newpackage></prestodelta>`,
		len(drpm), SHA256Hex(drpm)))
	prestoGz := GzipBytes(t, presto)
	prestoName := SHA256Hex(prestoGz) + "-prestodelta.xml.gz"
	WriteFile(t, filepath.Join(dir, "repodata", prestoName), prestoGz)

	repomd := fmt.Sprintf(
		`<?xml version="1.0"?><repomd><data type="primary"><checksum type="sha256">%s</checksum><location href="repodata/%s"/><size>%d</size></data><data type="prestodelta"><checksum type="sha256">%s</checksum><location href="repodata/%s"/><size>%d</size></data></repomd>`,
		SHA256Hex(primaryGz), primaryName, len(primaryGz),
		SHA256Hex(prestoGz), prestoName, len(prestoGz))
	WriteFile(t, filepath.Join(dir, "repodata", "repomd.xml"), []byte(repomd))
	WriteFile(t, filepath.Join(dir, "repodata", "repomd.xml.asc"), []byte("fake signature"))
	return pkgs
}

// RPMDelta is the delta package content used by BuildRPMRepo.
func RPMDelta() []byte {
	return bytes.Repeat([]byte("drpm"), 100)
}

// debPackagesStanza renders one binary package stanza for a Packages index.
func debPackagesStanza(name, version, poolPath string, data []byte) string {
	return fmt.Sprintf(
		"Package: %s\nVersion: %s\nArchitecture: amd64\nFilename: %s\nSize: %d\nMD5sum: %s\nSHA256: %s\nDescription: Test package\n",
		name, version, poolPath, len(data), MD5Hex(data), SHA256Hex(data))
}

// BuildDebRepo writes a minimal structured apt repository with main and
// universe components into dir and returns the pool file contents by
// archive-relative path.
func BuildDebRepo(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	pool := map[string][]byte{
		"pool/main/h/hello/hello_1.0_amd64.deb":     bytes.Repeat([]byte("hello"), 300),
		"pool/universe/e/extra/extra_2.0_amd64.deb": bytes.Repeat([]byte("extra"), 400),
	}
	for p, data := range pool {
		WriteFile(t, filepath.Join(dir, filepath.FromSlash(p)), data)
	}

	indexes := map[string][]byte{
		"main/binary-amd64/Packages": []byte(debPackagesStanza(
			"hello", "1.0", "pool/main/h/hello/hello_1.0_amd64.deb", pool["pool/main/h/hello/hello_1.0_amd64.deb"])),
		"universe/binary-amd64/Packages": []byte(debPackagesStanza(
			"extra", "2.0", "pool/universe/e/extra/extra_2.0_amd64.deb", pool["pool/universe/e/extra/extra_2.0_amd64.deb"])),
	}

	// The Release file lists both the uncompressed and gzip variants, but
	// only the gzip variant is served, matching real archives.
	release := "Origin: Test\nSuite: test\nCodename: test\nArchitectures: amd64\nComponents: main universe\nAcquire-By-Hash: yes\n"
	listed := map[string][]byte{}
	for p, data := range indexes {
		gz := GzipBytes(t, data)
		WriteFile(t, filepath.Join(dir, "dists", "test", filepath.FromSlash(p)+".gz"), gz)
		listed[p] = data
		listed[p+".gz"] = gz
	}
	for field, hasher := range map[string]func([]byte) string{"MD5Sum": MD5Hex, "SHA256": SHA256Hex} {
		release += field + ":\n"
		for p, data := range listed {
			release += fmt.Sprintf(" %s %d %s\n", hasher(data), len(data), p)
		}
	}
	WriteFile(t, filepath.Join(dir, "dists", "test", "Release"), []byte(release))
	return pool
}

// BuildArchRepo writes a minimal pacman repository named name into dir and
// returns the package contents by file name.
func BuildArchRepo(t *testing.T, dir, name string) map[string][]byte {
	t.Helper()
	pkgs := map[string][]byte{
		"zlib-1.3-1-x86_64.pkg.tar.zst": bytes.Repeat([]byte("zlib"), 300),
		"jq-1.7-1-x86_64.pkg.tar.zst":   bytes.Repeat([]byte("jq"), 400),
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for fname, data := range pkgs {
		WriteFile(t, filepath.Join(dir, fname), data)
		WriteFile(t, filepath.Join(dir, fname+".sig"), []byte("signature"))
		desc := fmt.Sprintf("%%FILENAME%%\n%s\n\n%%NAME%%\n%s\n\n%%CSIZE%%\n%d\n\n%%SHA256SUM%%\n%s\n",
			fname, strings.SplitN(fname, "-", 2)[0], len(data), SHA256Hex(data))
		hdr := &tar.Header{
			Name: strings.TrimSuffix(fname, "-x86_64.pkg.tar.zst") + "/desc",
			Mode: 0644,
			Size: int64(len(desc)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(desc)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	WriteFile(t, filepath.Join(dir, name+".db"), buf.Bytes())
	WriteFile(t, filepath.Join(dir, name+".files"), buf.Bytes())
	return pkgs
}

// BuildApkRepo writes a minimal Alpine repository into dir and returns the
// package contents by file name. The index replicates apk's real layout: a
// signature tar segment with its end-of-archive blocks cut off and the
// index tar as separate gzip streams, concatenated.
func BuildApkRepo(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	pkgs := map[string][]byte{
		"musl-1.2.5-r1.apk": bytes.Repeat([]byte("musl"), 300),
		"zlib-1.3-r2.apk":   bytes.Repeat([]byte("zlib"), 200),
	}
	index := ""
	for fname, data := range pkgs {
		name := strings.SplitN(fname, "-", 2)[0]
		version := strings.TrimSuffix(strings.TrimPrefix(fname, name+"-"), ".apk")
		index += fmt.Sprintf("C:Q1notarealpullchecksum=\nP:%s\nV:%s\nA:x86_64\nS:%d\n\n", name, version, len(data))
		WriteFile(t, filepath.Join(dir, fname), data)
	}

	// tarBytes renders entries into an uncompressed tar archive.
	tarBytes := func(entries map[string]string) []byte {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		for name, content := range entries {
			if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0644, Size: int64(len(content))}); err != nil {
				t.Fatal(err)
			}
			if _, err := tw.Write([]byte(content)); err != nil {
				t.Fatal(err)
			}
		}
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}

	// The signature segment loses its two zero end-of-archive blocks so
	// the tar stream continues into the index segment.
	sig := tarBytes(map[string]string{".SIGN.RSA.test.rsa.pub": "signature"})
	sig = sig[:len(sig)-1024]
	idx := tarBytes(map[string]string{"DESCRIPTION": "test repo", "APKINDEX": index})
	combined := append(GzipBytes(t, sig), GzipBytes(t, idx)...)
	WriteFile(t, filepath.Join(dir, "APKINDEX.tar.gz"), combined)
	return pkgs
}

// BuildFlatDebRepo writes a minimal flat apt repository into dir and
// returns the package contents by file name.
func BuildFlatDebRepo(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	data := bytes.Repeat([]byte("flat"), 250)
	WriteFile(t, filepath.Join(dir, "hello_3.0_amd64.deb"), data)
	packages := []byte(debPackagesStanza("hello", "3.0", "hello_3.0_amd64.deb", data))
	packagesGz := GzipBytes(t, packages)
	WriteFile(t, filepath.Join(dir, "Packages.gz"), packagesGz)
	release := "Architectures: amd64\nSHA256:\n"
	for p, d := range map[string][]byte{"Packages": packages, "Packages.gz": packagesGz} {
		release += fmt.Sprintf(" %s %d %s\n", SHA256Hex(d), len(d), p)
	}
	WriteFile(t, filepath.Join(dir, "Release"), []byte(release))
	return map[string][]byte{"hello_3.0_amd64.deb": data}
}

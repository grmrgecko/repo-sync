package server

import (
	"path"
	"strings"

	"github.com/grmrgecko/repo-sync/mirror"
)

// kindGeneric marks request paths that are not repository entry points.
const kindGeneric = "generic"

// resource is a crawlable unit derived from a request path.
type resource struct {
	// Kind is a RepoType string, or kindGeneric.
	Kind string
	// Key uniquely identifies the resource in state and the lock map.
	Key string
	// Path is the repository request path (the suite directory for deb).
	Path string
	// Root is the request path prefix covering every file the resource
	// owns; deb uses the archive root so pool requests match.
	Root string
	// ReqPath preserves the original request path.
	ReqPath string
}

// classifyRequest maps a request path onto a crawlable resource. Only
// repository entry-point metadata registers a repository; every other path
// is generic and served on demand.
func classifyRequest(reqPath string) resource {
	clean := path.Clean("/" + strings.TrimPrefix(reqPath, "/"))
	base := path.Base(clean)
	dir := path.Dir(clean)

	// RPM repositories are identified by any repodata request.
	if idx := strings.Index(clean, "/repodata/"); idx >= 0 {
		root := clean[:idx]
		if root == "" {
			root = "/"
		}
		return resource{
			Kind:    string(mirror.RepoRPM),
			Key:     string(mirror.RepoRPM) + ":" + root,
			Path:    root,
			Root:    root,
			ReqPath: clean,
		}
	}

	// Apt suites are identified by their release files; the archive root
	// above dists also covers the pool.
	if base == "InRelease" || base == "Release" || base == "Release.gpg" {
		root := dir
		if idx := strings.Index(clean, "/dists/"); idx >= 0 {
			root = clean[:idx]
			if root == "" {
				root = "/"
			}
		}
		return resource{
			Kind:    string(mirror.RepoDeb),
			Key:     string(mirror.RepoDeb) + ":" + dir,
			Path:    dir,
			Root:    root,
			ReqPath: clean,
		}
	}

	// Pacman repositories are identified by their database file.
	if strings.HasSuffix(base, ".db") {
		return resource{
			Kind:    string(mirror.RepoArch),
			Key:     string(mirror.RepoArch) + ":" + dir,
			Path:    dir,
			Root:    dir,
			ReqPath: clean,
		}
	}

	// Alpine repositories are identified by their index archive.
	if base == mirror.APKIndexName {
		return resource{
			Kind:    string(mirror.RepoApk),
			Key:     string(mirror.RepoApk) + ":" + dir,
			Path:    dir,
			Root:    dir,
			ReqPath: clean,
		}
	}

	return resource{
		Kind:    kindGeneric,
		Key:     kindGeneric + ":" + clean,
		Path:    clean,
		ReqPath: clean,
	}
}

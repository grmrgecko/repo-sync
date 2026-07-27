// Package server runs the caching mirror: it answers requests from the
// local tree, fetches misses through the configured upstream mounts, and
// crawls the repositories it discovers in the background so they stay
// current without holding a request open.
package server

import (
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	cfg "github.com/grmrgecko/repo-sync/config"
	"github.com/grmrgecko/repo-sync/fetch"
	"github.com/grmrgecko/repo-sync/state"
	log "github.com/sirupsen/logrus"
)

// Handler returns the mirror server's HTTP handler.
func Handler() http.Handler {
	return http.HandlerFunc(serveMirror)
}

// regularFile stats a path, reporting only regular files as present.
func regularFile(name string) (os.FileInfo, bool) {
	info, err := os.Stat(name)
	if err != nil || !info.Mode().IsRegular() {
		return nil, false
	}
	return info, true
}

// serveLocal resolves a request path under a domain root, mapping
// directory requests to a cached index.html.
func serveLocal(root, reqPath string, isDir bool) (string, error) {
	p := reqPath
	if isDir || p == "/" {
		p = strings.TrimSuffix(p, "/") + "/index.html"
	}
	return fetch.LocalJoin(root, p)
}

// internalFile reports paths that belong to the mirror's own bookkeeping
// and must never be served.
func internalFile(reqPath string) bool {
	base := path.Base(reqPath)
	return base == fetch.LockFileName || base == fetch.PruneStateName ||
		base == fetch.MissingStateName || base == fetch.DiscoverStateName ||
		strings.HasSuffix(base, fetch.StagedSuffix) || strings.HasSuffix(base, fetch.PartialSuffix)
}

// fetchUpstream retrieves one file through the mount covering the request
// path and stores it under root.
func fetchUpstream(ctx context.Context, root, reqPath string, isDir bool) error {
	mount, ok := cfg.C.MountFor(reqPath)
	if !ok {
		return fmt.Errorf("fetch %s: %w", reqPath, fetch.ErrNotFound)
	}
	rel := reqPath
	if mount.Path != "/" {
		rel = strings.TrimPrefix(reqPath, mount.Path)
	}
	if isDir {
		rel = strings.TrimSuffix(rel, "/") + "/"
	}
	dst, err := serveLocal(root, reqPath, isDir)
	if err != nil {
		return err
	}
	// Concurrent on-demand fetches and background crawls can target the
	// same destination; fetch.File serializes them by path.
	src := fetch.NewSource([]string{mount.Upstream})
	_, err = fetch.File(ctx, src, rel, dst, nil, false, false)
	return err
}

// writeFetchError reports an upstream failure to the client.
func writeFetchError(w http.ResponseWriter, err error) {
	if errors.Is(err, fetch.ErrNotFound) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	http.Error(w, "upstream fetch failed", http.StatusBadGateway)
}

// writeCachedFailure reports a remembered upstream failure.
func writeCachedFailure(w http.ResponseWriter, status int) {
	if status == http.StatusNotFound {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	http.Error(w, "upstream fetch failed", http.StatusBadGateway)
}

// writeIndexesDisabled answers a directory request when directory indexes
// are turned off, confirming the mirror is alive without listing content.
func writeIndexesDisabled(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.WriteString(w, "<!DOCTYPE html>\n<html>\n<head><title>repo-sync mirror</title></head>\n<body>\n"+
		"<h1>repo-sync mirror</h1>\n<p>This mirror is running. Directory indexes are disabled.</p>\n"+
		"</body>\n</html>\n")
}

// writeMountIndex answers the root directory request with a generated index
// of every mount and configured repository, giving clients a navigable
// entry point when no catch-all mount covers the root.
func writeMountIndex(w http.ResponseWriter, conf *cfg.Config) {
	seen := map[string]bool{}
	var paths []string
	add := func(p string) {
		if p == "/" || seen[p] {
			return
		}
		seen[p] = true
		paths = append(paths, p)
	}
	for _, mount := range conf.Mounts {
		add(mount.Path)
		for _, repo := range mount.Repos {
			add(repo.Path)
		}
	}
	sort.Strings(paths)

	var b strings.Builder
	b.WriteString("<!DOCTYPE html>\n<html>\n<head><title>Index of /</title></head>\n<body>\n<h1>Index of /</h1>\n<ul>\n")
	for _, p := range paths {
		href := html.EscapeString(p + "/")
		fmt.Fprintf(&b, "<li><a href=\"%s\">%s</a></li>\n", href, href)
	}
	b.WriteString("</ul>\n</body>\n</html>\n")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.WriteString(w, b.String())
}

// serveMirror handles every mirror request: domain lookup, classification,
// and dispatch to the role-specific handler.
func serveMirror(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	domain, ok := cfg.C.Domain(r.Host)
	if !ok {
		http.Error(w, "unknown mirror domain", http.StatusNotFound)
		return
	}

	reqPath := path.Clean("/" + strings.TrimPrefix(r.URL.Path, "/"))
	isDir := strings.HasSuffix(r.URL.Path, "/") || reqPath == "/"
	if internalFile(reqPath) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	// Directory requests are answered before role dispatch: a notice page
	// when indexes are disabled, and a generated index of the mounts for a
	// root that no catch-all mount covers.
	if isDir {
		if !cfg.C.HTTP.DirectoryIndexes {
			writeIndexesDisabled(w)
			return
		}
		if reqPath == "/" {
			if _, ok := cfg.C.MountFor("/"); !ok {
				writeMountIndex(w, cfg.C)
				return
			}
		}
	}
	res := classifyRequest(reqPath)

	if domain.Role == cfg.RoleOnline {
		handleOnline(w, r, domain, reqPath, isDir, res)
		return
	}
	handleOffline(w, r, domain, reqPath, isDir, res)
}

// handleOnline serves from the writable cache, fetching and crawling on
// demand.
func handleOnline(w http.ResponseWriter, r *http.Request, domain cfg.DomainConfig, reqPath string, isDir bool, res resource) {
	local, err := serveLocal(domain.Root, reqPath, isDir)
	if err != nil {
		http.Error(w, "bad request path", http.StatusBadRequest)
		return
	}
	now := time.Now()

	// Directory listings are cached pass-through pages with no tracking.
	if isDir {
		if _, exists := regularFile(local); !exists {
			if err := fetchUpstream(r.Context(), domain.Root, reqPath, true); err != nil {
				writeFetchError(w, err)
				return
			}
		}
		http.ServeFile(w, r, local)
		return
	}

	if res.Kind == kindGeneric {
		// Files under a registered repository keep it alive and are
		// fetched on demand until its crawl completes.
		if state.S.TouchRepoMembers(reqPath, now) {
			if _, exists := regularFile(local); !exists {
				if err := fetchUpstream(r.Context(), domain.Root, reqPath, false); err != nil {
					writeFetchError(w, err)
					return
				}
			}
			http.ServeFile(w, r, local)
			return
		}
		handleGeneric(w, r, domain, reqPath, res, local, now)
		return
	}

	// A repository entry point registers the repository and dispatches a
	// background crawl on first sight so the whole repository fills in
	// behind this response without holding it open. The requested file
	// itself is fetched on demand so the client is answered immediately.
	// Repositories already crawled only re-dispatch here to retry a failed
	// crawl; missing optional files must not trigger a full crawl per
	// request.
	entry, tracked := state.S.MarkRequested(res.Kind, res.Key, res.Path, res.Root, now)
	_, exists := regularFile(local)
	needCrawl := entry.LastCrawled.IsZero() || (!exists && entry.LastError != "")
	if !exists {
		if err := fetchUpstream(r.Context(), domain.Root, reqPath, false); err != nil {
			// A path whose first contact failed was never shown to be a
			// repository, so the registration this request created is
			// dropped and the scheduler never crawls it. Registrations an
			// earlier request established are kept: every entry point of one
			// repository shares a key, so a repository serving Release but
			// not InRelease would otherwise deregister itself on the miss.
			if !tracked {
				state.S.Delete(res.Key)
			}
			writeFetchError(w, err)
			return
		}
	}
	if needCrawl {
		startCrawl(res)
	}
	http.ServeFile(w, r, local)
}

// handleGeneric serves a plain cached file, refreshing stale copies and
// remembering upstream failures.
func handleGeneric(w http.ResponseWriter, r *http.Request, domain cfg.DomainConfig, reqPath string, res resource, local string, now time.Time) {
	if status, hit := fetchFailures.Hit(res.Key, now); hit {
		if _, exists := regularFile(local); !exists {
			writeCachedFailure(w, status)
			return
		}
	} else {
		// GenericMaxAge bounds how often the upstream is revalidated,
		// measured from the last upstream check recorded in state. The
		// local file's modification time carries the upstream's
		// Last-Modified, so it does not record when the mirror last checked.
		entry, tracked := state.S.Entry(res.Key)
		_, exists := regularFile(local)
		if !exists || !tracked || now.Sub(entry.LastCrawled) > cfg.C.GenericMaxAge {
			err := fetchUpstream(r.Context(), domain.Root, reqPath, false)
			switch {
			case errors.Is(err, fetch.ErrNotFound):
				fetchFailures.Add(res.Key, http.StatusNotFound, now, cfg.C.NegativeCacheTTL)
				state.S.Delete(res.Key)
				if !exists {
					writeFetchError(w, err)
					return
				}
				log.WithError(err).WithField("path", reqPath).Warn("Upstream lost a stale generic file; serving the existing copy.")
			case err != nil:
				fetchFailures.Add(res.Key, http.StatusBadGateway, now, cfg.C.TransientErrorTTL)
				if !exists {
					writeFetchError(w, err)
					return
				}
				log.WithError(err).WithField("path", reqPath).Error("Unable to refresh a stale generic file; serving the existing copy.")
			default:
				state.S.MarkRequested(kindGeneric, res.Key, res.Path, "", now)
				// Record the upstream check so GenericMaxAge gates the next
				// request. The scheduler skips generic entries, so the retry
				// time this also sets is unused.
				state.S.MarkCrawled(res.Key, now, nil)
			}
		} else {
			state.S.MarkRequested(kindGeneric, res.Key, res.Path, "", now)
		}
	}
	http.ServeFile(w, r, local)
}

// handleOffline serves the read-only published tree, reading through the
// online cache for anything it does not hold.
func handleOffline(w http.ResponseWriter, r *http.Request, domain cfg.DomainConfig, reqPath string, isDir bool, res resource) {
	local, err := serveLocal(domain.Root, reqPath, isDir)
	if err != nil {
		http.Error(w, "bad request path", http.StatusBadRequest)
		return
	}
	if _, exists := regularFile(local); exists {
		http.ServeFile(w, r, local)
		return
	}
	handleOnline(w, r, cfg.C.OnlineDomain(), reqPath, isDir, res)
}

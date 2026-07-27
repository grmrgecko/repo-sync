package mirror

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"path"
	"slices"
	"sort"
	"strings"

	"github.com/grmrgecko/repo-sync/fetch"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/html"
)

// dirListing holds the entries parsed from an HTML directory index.
type dirListing struct {
	dirs  []string
	files map[string]bool
}

// repoTarget is one repository to synchronize paired with the format it is
// synchronized as. A single crawl can turn up several formats, so the type
// travels with the repository rather than with the run.
type repoTarget struct {
	url string
	typ RepoType
}

// fileTarget is one loose file an include pattern matched during discovery,
// kept as the directory it was found in and its name so its destination
// follows the same path mapping a repository's does.
type fileTarget struct {
	dirURL string
	name   string
}

// discovery holds what a crawl found below a base URL.
type discovery struct {
	repos []repoTarget
	files []fileTarget
	// failures counts the directories the crawl could not list. A crawl
	// that missed part of the tree is not evidence that what it did not
	// reach went away, so the cache does not withdraw anything from it.
	failures int
}

// discoverRepos crawls directory listings under baseURL looking for
// repositories of the formats the run accepts, descending at most
// opts.DiscoverDepth levels. Excluded entries are never crawled, and files
// matching the include patterns are collected along the way.
func discoverRepos(ctx context.Context, baseURL string, opts *Options) (*discovery, error) {
	type node struct {
		url   string
		rel   string
		depth int
	}
	types := opts.discoverTypes()
	start := strings.TrimRight(baseURL, "/") + "/"
	queue := []node{{url: start}}
	visited := map[string]bool{start: true}
	found := &discovery{}

	for len(queue) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n := queue[0]
		queue = queue[1:]
		listing, err := listDir(ctx, n.url)
		if err != nil {
			log.WithError(err).WithField("url", n.url).Warn("Failed to list directory during discovery.")
			found.failures++
			continue
		}

		// A directory containing repository metadata is synchronized as a
		// whole; its children are not crawled further.
		if typ, ok := repoTypeOf(listing, types); ok {
			found.repos = append(found.repos, repoTarget{url: strings.TrimRight(n.url, "/"), typ: typ})
			continue
		}

		// Collect the loose files the run asks for. Only directories that
		// are not repositories are considered: a repository's own files come
		// from its metadata, and pruning there would remove anything else.
		found.files = append(found.files, matchedFiles(listing, n.url, n.rel, opts)...)

		if n.depth >= opts.DiscoverDepth {
			continue
		}
		for _, dir := range listing.dirs {
			rel := path.Join(n.rel, dir)
			if matchAny(opts.Exclude, dir, rel) {
				log.WithField("url", n.url+dir+"/").Debug("Skipped excluded directory.")
				continue
			}
			child := n.url + url.PathEscape(dir) + "/"
			if visited[child] {
				continue
			}
			visited[child] = true
			queue = append(queue, node{url: child, rel: rel, depth: n.depth + 1})
		}
	}
	sort.Slice(found.repos, func(i, j int) bool { return found.repos[i].url < found.repos[j].url })
	sort.Slice(found.files, func(i, j int) bool {
		if found.files[i].dirURL != found.files[j].dirURL {
			return found.files[i].dirURL < found.files[j].dirURL
		}
		return found.files[i].name < found.files[j].name
	})
	return found, nil
}

// repoTypeOf reports the format a directory listing identifies, considering
// only the formats the run accepts. The first match wins, so a directory
// carrying more than one format's markers is synchronized as the earlier
// one rather than twice.
func repoTypeOf(listing *dirListing, types []RepoType) (RepoType, bool) {
	for _, typ := range types {
		switch typ {
		case RepoRPM:
			if slices.Contains(listing.dirs, "repodata") {
				return typ, true
			}
		case RepoDeb:
			if listing.files["InRelease"] || listing.files["Release"] {
				return typ, true
			}
		case RepoArch:
			if hasArchDB(listing.files) {
				return typ, true
			}
		case RepoApk:
			if listing.files[APKIndexName] {
				return typ, true
			}
		}
	}
	return "", false
}

// matchedFiles returns the files in a listing the include patterns select,
// in name order so a run's work is deterministic.
func matchedFiles(listing *dirListing, dirURL, rel string, opts *Options) []fileTarget {
	if len(opts.IncludeFiles) == 0 {
		return nil
	}
	names := make([]string, 0, len(listing.files))
	for name := range listing.files {
		names = append(names, name)
	}
	sort.Strings(names)

	dir := strings.TrimRight(dirURL, "/")
	var files []fileTarget
	for _, name := range names {
		entry := path.Join(rel, name)
		if matchAny(opts.Exclude, name, entry) {
			continue
		}
		if !matchAny(opts.IncludeFiles, name, entry) {
			continue
		}
		files = append(files, fileTarget{dirURL: dir, name: name})
	}
	return files
}

// matchAny reports whether any glob matches an entry. A pattern containing
// a slash is matched against the entry's path below the crawled URL, so it
// can pin a name to one place in the tree; a plain pattern is matched
// against the name alone, wherever in the tree it appears. Patterns are
// validated before a run starts, so a bad one cannot silently never match.
func matchAny(patterns []string, name, rel string) bool {
	for _, pattern := range patterns {
		pattern, subject := matchSubject(pattern, name, rel)
		if ok, err := path.Match(pattern, subject); err == nil && ok {
			return true
		}
	}
	return false
}

// matchSubject pairs a pattern with the string it is matched against. A
// leading slash anchors the pattern to the crawled URL, so it is stripped
// and the entry's path below that URL is matched; at the root that path is
// the bare name, which is what makes anchoring to the root expressible at
// all. Any other pattern carrying a slash is matched against the same path
// unanchored, and a plain pattern against the name alone.
func matchSubject(pattern, name, rel string) (string, string) {
	if strings.HasPrefix(pattern, "/") {
		return strings.TrimPrefix(pattern, "/"), rel
	}
	if strings.Contains(pattern, "/") {
		return pattern, rel
	}
	return pattern, name
}

// ValidPattern reports whether a glob is usable as an exclude or include
// pattern, so a run rejects a malformed one instead of never matching it.
// The anchoring slash is not part of the glob, so it is stripped before the
// syntax is checked.
func ValidPattern(pattern string) error {
	_, err := path.Match(strings.TrimPrefix(pattern, "/"), "")
	return err
}

// hasArchDB reports whether a directory listing contains a pacman database.
func hasArchDB(files map[string]bool) bool {
	for name := range files {
		if strings.HasSuffix(name, ".db") {
			return true
		}
	}
	return false
}

// listDir fetches an HTML directory index and extracts its immediate child
// directories and files.
func listDir(ctx context.Context, dirURL string) (*dirListing, error) {
	resp, err := fetch.Get(ctx, dirURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("list %s: status %d", dirURL, resp.StatusCode)
	}

	base, err := url.Parse(dirURL)
	if err != nil {
		return nil, err
	}
	listing := &dirListing{files: map[string]bool{}}
	seenDirs := map[string]bool{}
	tok := html.NewTokenizer(io.LimitReader(resp.Body, 8<<20))
	for {
		tt := tok.Next()
		if tt == html.ErrorToken {
			break
		}
		if tt != html.StartTagToken && tt != html.SelfClosingTagToken {
			continue
		}
		name, hasAttr := tok.TagName()
		if string(name) != "a" || !hasAttr {
			continue
		}
		var href string
		for {
			key, val, more := tok.TagAttr()
			if string(key) == "href" {
				href = string(val)
			}
			if !more {
				break
			}
		}
		entry, isDir := childEntry(base, href)
		if entry == "" {
			continue
		}
		if isDir {
			if !seenDirs[entry] {
				seenDirs[entry] = true
				listing.dirs = append(listing.dirs, entry)
			}
		} else {
			listing.files[entry] = true
		}
	}
	return listing, nil
}

// childEntry resolves an anchor href against the directory URL, returning
// the entry name when it is an immediate child of that directory.
func childEntry(base *url.URL, href string) (string, bool) {
	if href == "" || strings.HasPrefix(href, "#") {
		return "", false
	}
	ref, err := url.Parse(href)
	if err != nil || ref.RawQuery != "" || ref.Fragment != "" {
		return "", false
	}
	resolved := base.ResolveReference(ref)
	if resolved.Scheme != base.Scheme || resolved.Host != base.Host {
		return "", false
	}
	rel := strings.TrimPrefix(resolved.Path, base.Path)
	if rel == "" || rel == resolved.Path {
		return "", false
	}
	isDir := strings.HasSuffix(rel, "/")
	rel = strings.TrimSuffix(rel, "/")
	if rel == "" || strings.Contains(rel, "/") {
		return "", false
	}
	return rel, isDir
}

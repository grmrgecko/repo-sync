package mirror

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/grmrgecko/repo-sync/fetch"
	log "github.com/sirupsen/logrus"
)

// discoverState is the on-disk layout of the discovery cache, keyed by the
// base URL each crawl started from so one destination can hold several.
type discoverState struct {
	Crawls map[string]*crawlRecord `json:"crawls"`
}

// crawlRecord is one crawled base URL: when it was last scanned, the crawl
// options that scan ran with, and what it found. The options are recorded so
// a run that changes them scans again rather than replaying results the new
// patterns would not have produced.
type crawlRecord struct {
	LastCrawled time.Time     `json:"last_crawled"`
	Options     string        `json:"options"`
	Repos       []*repoRecord `json:"repos,omitempty"`
	Files       []*fileRecord `json:"files,omitempty"`
}

// repoRecord is one discovered repository. Gone marks when a scan stopped
// finding it upstream, and is what the prune grace is measured from. It is a
// pointer so a record the upstream still publishes carries no timestamp at
// all, rather than a zero one a reader has to interpret.
type repoRecord struct {
	URL  string     `json:"url"`
	Type string     `json:"type"`
	Gone *time.Time `json:"gone,omitempty"`
}

// fileRecord is one discovered loose file, kept as the directory it was
// found in and its name, matching how a run mirrors it.
type fileRecord struct {
	Dir  string     `json:"dir"`
	Name string     `json:"name"`
	Gone *time.Time `json:"gone,omitempty"`
}

// discoverCache is the discovery state for one destination tree. Crawling a
// vendor archive costs hundreds of directory listings, so the results are
// reused until they age out, and the record of what was found is also what
// tells a later run which repositories the upstream has since dropped.
type discoverCache struct {
	path  string
	state discoverState
}

// loadDiscoverCache reads the discovery state kept at the destination root.
// A missing or unreadable file starts empty: the cache only saves work and
// tracks what upstream has dropped, so losing it costs a crawl rather than
// correctness.
func loadDiscoverCache(dest string) *discoverCache {
	c := &discoverCache{
		path:  filepath.Join(dest, fetch.DiscoverStateName),
		state: discoverState{Crawls: map[string]*crawlRecord{}},
	}
	data, err := os.ReadFile(c.path)
	if err != nil {
		return c
	}
	var state discoverState
	if err := json.Unmarshal(data, &state); err != nil {
		log.WithError(err).WithField("path", c.path).Warn("Failed to parse the discovery state; scanning again.")
		return c
	}
	for base, rec := range state.Crawls {
		if rec != nil {
			c.state.Crawls[base] = rec
		}
	}
	return c
}

// crawlKey normalizes a base URL into the key its crawl is recorded under,
// so the same tree named with and without a trailing slash is one record
// rather than two.
func crawlKey(base string) string {
	return strings.TrimRight(base, "/") + "/"
}

// reuse returns a recent crawl's results for a base URL, or reports that the
// tree has to be scanned again. A crawl ages out after ttl, and one run with
// different discovery options is never replayed under new ones.
func (c *discoverCache) reuse(base, options string, ttl time.Duration, now time.Time) (*discovery, bool) {
	if ttl <= 0 {
		return nil, false
	}
	rec := c.state.Crawls[crawlKey(base)]
	if rec == nil || rec.Options != options || rec.LastCrawled.IsZero() {
		return nil, false
	}
	age := now.Sub(rec.LastCrawled)
	if age < 0 || age >= ttl {
		return nil, false
	}

	// Entries the last scan stopped finding are kept for their grace period
	// but are no longer part of the tree, so they are not replayed.
	found := &discovery{}
	for _, repo := range rec.Repos {
		if repo.Gone == nil {
			found.repos = append(found.repos, repoTarget{url: repo.URL, typ: RepoType(repo.Type)})
		}
	}
	for _, file := range rec.Files {
		if file.Gone == nil {
			found.files = append(found.files, fileTarget{dirURL: file.Dir, name: file.Name})
		}
	}
	log.WithFields(log.Fields{
		"url":          base,
		"repositories": len(found.repos),
		"files":        len(found.files),
		"age":          age.Round(time.Second).String(),
	}).Info("Reusing the cached discovery.")
	return found, true
}

// update records what a scan found and returns the targets the upstream no
// longer serves, which are the ones whose grace period has run out. A scan
// that could not list part of the tree is not evidence that anything went
// away, so it refreshes what it did find and leaves the rest alone.
//
// A withdrawal is only forgotten once the local copy goes with it, so a run
// that is not pruning keeps the record and leaves the removal to one that
// is, rather than losing track of a directory nothing will clean up.
func (c *discoverCache) update(base, options string, found *discovery, now time.Time, opts *Options) *discovery {
	key := crawlKey(base)
	rec := c.state.Crawls[key]
	if rec == nil {
		rec = &crawlRecord{}
		c.state.Crawls[key] = rec
	}
	rec.LastCrawled = now
	rec.Options = options
	clean := found.failures == 0

	seenRepos := map[string]bool{}
	for _, repo := range found.repos {
		seenRepos[repo.url] = true
	}
	seenFiles := map[string]bool{}
	for _, file := range found.files {
		seenFiles[file.dirURL+"/"+file.name] = true
	}

	gone := &discovery{}
	repos := rec.Repos[:0]
	for _, repo := range rec.Repos {
		if seenRepos[repo.URL] {
			repo.Gone = nil
			repos = append(repos, repo)
			continue
		}
		if !clean {
			repos = append(repos, repo)
			continue
		}
		if repo.Gone == nil {
			repo.Gone = &now
			log.WithField("url", repo.URL).Info("Repository is no longer published upstream.")
		}
		if !opts.Prune || now.Sub(*repo.Gone) < opts.PruneGrace {
			repos = append(repos, repo)
			continue
		}
		gone.repos = append(gone.repos, repoTarget{url: repo.URL, typ: RepoType(repo.Type)})
	}
	rec.Repos = repos

	files := rec.Files[:0]
	for _, file := range rec.Files {
		if seenFiles[file.Dir+"/"+file.Name] {
			file.Gone = nil
			files = append(files, file)
			continue
		}
		if !clean {
			files = append(files, file)
			continue
		}
		if file.Gone == nil {
			file.Gone = &now
			log.WithFields(log.Fields{"url": file.Dir, "name": file.Name}).Info("File is no longer published upstream.")
		}
		if !opts.Prune || now.Sub(*file.Gone) < opts.PruneGrace {
			files = append(files, file)
			continue
		}
		gone.files = append(gone.files, fileTarget{dirURL: file.Dir, name: file.Name})
	}
	rec.Files = files

	// Add what this scan turned up for the first time.
	known := map[string]bool{}
	for _, repo := range rec.Repos {
		known[repo.URL] = true
	}
	for _, repo := range found.repos {
		if !known[repo.url] {
			rec.Repos = append(rec.Repos, &repoRecord{URL: repo.url, Type: string(repo.typ)})
		}
	}
	knownFiles := map[string]bool{}
	for _, file := range rec.Files {
		knownFiles[file.Dir+"/"+file.Name] = true
	}
	for _, file := range found.files {
		if !knownFiles[file.dirURL+"/"+file.name] {
			rec.Files = append(rec.Files, &fileRecord{Dir: file.dirURL, Name: file.name})
		}
	}

	// Keep the file in a stable order so it reads as a listing rather than
	// as whatever order the crawl happened to finish in.
	sort.Slice(rec.Repos, func(i, j int) bool { return rec.Repos[i].URL < rec.Repos[j].URL })
	sort.Slice(rec.Files, func(i, j int) bool {
		if rec.Files[i].Dir != rec.Files[j].Dir {
			return rec.Files[i].Dir < rec.Files[j].Dir
		}
		return rec.Files[i].Name < rec.Files[j].Name
	})
	return gone
}

// save writes the discovery state, removing it once no crawl is tracked. A
// dry run leaves it as it found it, so a planning run never spends the
// cache's lifetime or its grace periods.
func (c *discoverCache) save(dryRun bool) {
	if dryRun {
		return
	}
	if len(c.state.Crawls) == 0 {
		if err := os.Remove(c.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			log.WithError(err).WithField("path", c.path).Warn("Failed to remove the discovery state.")
		}
		return
	}
	data, err := json.MarshalIndent(c.state, "", "  ")
	if err == nil {
		err = os.MkdirAll(filepath.Dir(c.path), 0755)
	}
	if err == nil {
		err = os.WriteFile(c.path, data, 0644)
	}
	if err != nil {
		log.WithError(err).WithField("path", c.path).Warn("Failed to save the discovery state.")
	}
}

// discoverOptions renders the crawl settings that decide what a scan finds,
// so a run that changes any of them scans the tree again instead of
// replaying results the new settings would not have produced.
func (o *Options) discoverOptions() string {
	types := make([]string, 0, len(o.discoverTypes()))
	for _, typ := range o.discoverTypes() {
		types = append(types, string(typ))
	}
	sort.Strings(types)
	exclude := append([]string(nil), o.Exclude...)
	sort.Strings(exclude)
	include := append([]string(nil), o.IncludeFiles...)
	sort.Strings(include)
	return fmt.Sprintf("depth=%d types=%s exclude=%s include=%s",
		o.DiscoverDepth, strings.Join(types, ","), strings.Join(exclude, ","), strings.Join(include, ","))
}

// evictDiscovered removes what a scan no longer finds upstream, so a
// repository a vendor withdrew does not linger in the mirror forever. It is
// as destructive as pruning and is gated on the same flag; a dry run reports
// the removals instead of making them.
func evictDiscovered(gone *discovery, opts *Options) {
	for _, repo := range gone.repos {
		dir, err := opts.repoDest(repo.url)
		if err != nil {
			log.WithError(err).WithField("url", repo.url).Warn("Failed to resolve a withdrawn repository's directory.")
			continue
		}
		// A repository mapping onto the destination root itself, which is
		// what --flat does, shares its directory with everything else in the
		// run; removing that tree would take the whole mirror with it.
		if !underDestination(dir, opts.Destination) {
			log.WithFields(log.Fields{"url": repo.url, "path": dir}).
				Warn("Not evicting a withdrawn repository that maps onto the destination root.")
			continue
		}
		if opts.DryRun {
			log.WithFields(log.Fields{"url": repo.url, "path": dir}).Info("Would evict a withdrawn repository.")
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			log.WithError(err).WithField("path", dir).Warn("Failed to evict a withdrawn repository.")
			continue
		}
		log.WithFields(log.Fields{"url": repo.url, "path": dir}).Info("Evicted a withdrawn repository.")
	}

	for _, file := range gone.files {
		dir, err := opts.repoDest(file.dirURL)
		if err != nil {
			log.WithError(err).WithField("url", file.dirURL).Warn("Failed to resolve a withdrawn file's directory.")
			continue
		}
		name, err := fetch.LocalJoin(dir, file.name)
		if err != nil {
			log.WithError(err).WithField("name", file.name).Warn("Failed to resolve a withdrawn file's path.")
			continue
		}
		if opts.DryRun {
			log.WithField("path", name).Info("Would evict a withdrawn file.")
			continue
		}
		if err := os.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
			log.WithError(err).WithField("path", name).Warn("Failed to evict a withdrawn file.")
			continue
		}
		log.WithField("path", name).Info("Evicted a withdrawn file.")
	}
}

// underDestination reports whether a path sits strictly below the
// destination directory, which is what makes it safe to remove as a whole.
func underDestination(dir, dest string) bool {
	dirClean := filepath.Clean(dir)
	destClean := filepath.Clean(dest)
	return dirClean != destClean && strings.HasPrefix(dirClean, destClean+string(os.PathSeparator))
}

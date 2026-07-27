// Package mirror synchronizes remote package repositories into a local
// directory tree, preserving the upstream layout so the result can be
// served directly to package managers.
package mirror

import (
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/grmrgecko/repo-sync/fetch"
)

// RepoType identifies the repository format being synchronized.
type RepoType string

const (
	// RepoRPM synchronizes yum/dnf style repositories.
	RepoRPM RepoType = "rpm"
	// RepoDeb synchronizes apt style repositories.
	RepoDeb RepoType = "deb"
	// RepoArch synchronizes pacman style repositories.
	RepoArch RepoType = "arch"
	// RepoApk synchronizes Alpine apk style repositories.
	RepoApk RepoType = "apk"
)

// AllTypes lists every supported repository format, in the order a
// directory's metadata is checked against them. RPM and deb come first as
// their markers are the most specific.
var AllTypes = []RepoType{RepoRPM, RepoDeb, RepoArch, RepoApk}

// TypeNames lists the supported format names, for user facing messages.
func TypeNames() []string {
	names := make([]string, 0, len(AllTypes))
	for _, typ := range AllTypes {
		names = append(names, string(typ))
	}
	return names
}

// Options configures a synchronization run.
type Options struct {
	// Type selects the repository format. It is empty for a run whose
	// repositories are identified by their metadata rather than named on the
	// command line, and is pinned to the repository's own format before that
	// repository is synchronized.
	Type RepoType
	// Types limits the formats a run identifies repositories as. An empty
	// list accepts every supported format.
	Types []RepoType
	// URLs lists the repository or mirrorlist URLs to synchronize.
	URLs []string
	// Destination is the local directory repositories are placed under.
	Destination string
	// Trim drops this many leading URL path components when building the
	// destination path.
	Trim int
	// Flat places repositories directly in Destination without copying the
	// URL path.
	Flat bool
	// Discover crawls each URL's directory listings for repositories.
	Discover bool
	// DiscoverDepth limits how deep discovery crawls below each URL.
	DiscoverDepth int
	// DiscoverCache is how long a crawl's results are reused before the
	// tree is scanned again. Zero scans on every run.
	DiscoverCache time.Duration
	// Exclude lists globs matched against the names of entries found while
	// crawling; a match is neither crawled nor mirrored. A pattern
	// containing a slash is matched against the entry's path below the
	// crawled URL instead of its name, and a leading slash anchors it to
	// that URL.
	Exclude []string
	// IncludeFiles lists globs matched against the names of files found
	// outside of any repository while crawling; a match is mirrored as a
	// plain file. Patterns match by path the same way Exclude does, so a
	// leading slash limits one to the crawled directory itself. Files
	// inside a repository come from its metadata and are not matched here.
	IncludeFiles []string
	// Workers is the number of concurrent download workers.
	Workers int
	// Verify re-verifies checksums of existing local files.
	Verify bool
	// Prune deletes local files no longer part of the repository.
	Prune bool
	// PruneGrace defers pruning a file until it has been unreferenced for
	// this long across runs.
	PruneGrace time.Duration
	// DryRun reports what would change without downloading packages or
	// altering the published repository.
	DryRun bool
	// Missing selects what happens when the upstream does not serve a
	// package the repository metadata lists. The zero value is the strict
	// mode, failing the repository at the first such file.
	Missing fetch.MissingMode
	// MissingRetries is how many consecutive runs must miss a file before
	// the retry mode accepts it as gone and stops failing the run over it.
	MissingRetries int
	// Trace writes a trace file describing this mirror into every
	// repository synchronized, and mirrors the traces upstream publishes
	// beside it.
	Trace bool
	// TraceInfo describes the mirror recorded in that trace file.
	TraceInfo TraceInfo
	// destBase, when set by the mirror server, pins the repository root
	// directory directly instead of deriving it from the URL path.
	destBase string
	// Components limits deb synchronization to these components.
	Components []string
	// Architectures limits deb synchronization to these architectures.
	Architectures []string
}

// discoverTypes returns the formats a run identifies repositories as: the
// formats it was limited to, the single format it was given, or every
// supported format when it named none.
func (o *Options) discoverTypes() []RepoType {
	if len(o.Types) > 0 {
		return o.Types
	}
	if o.Type != "" {
		return []RepoType{o.Type}
	}
	return AllTypes
}

// newMissing starts tracking the packages an upstream does not serve for
// one repository, recording them in dir. That is the repository's own
// directory, and for apt the suite directory rather than the archive root,
// so suites sharing a pool keep their own records.
func (o *Options) newMissing(dir string) *fetch.Missing {
	return fetch.NewMissing(dir, o.Missing, o.MissingRetries, o.DryRun)
}

// repoDest maps a repository URL onto the destination directory according
// to the configured trimming rules.
func (o *Options) repoDest(repoURL string) (string, error) {
	if o.destBase != "" {
		return o.destBase, nil
	}
	if o.Flat {
		return o.Destination, nil
	}
	u, err := url.Parse(repoURL)
	if err != nil {
		return "", err
	}
	var segs []string
	for _, seg := range strings.Split(u.Path, "/") {
		if seg != "" {
			segs = append(segs, seg)
		}
	}
	if o.Trim >= len(segs) {
		return o.Destination, nil
	}
	return fetch.LocalJoin(o.Destination, path.Join(segs[o.Trim:]...))
}

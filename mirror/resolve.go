package mirror

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/grmrgecko/repo-sync/fetch"
	log "github.com/sirupsen/logrus"
)

// probePaths lists the metadata files that identify a repository of a type.
// Types without fixed-name metadata return nothing.
func probePaths(typ RepoType) []string {
	switch typ {
	case RepoRPM:
		return []string{"repodata/repomd.xml"}
	case RepoDeb:
		return []string{"InRelease", "Release"}
	case RepoApk:
		return []string{APKIndexName}
	}
	return nil
}

// detectType identifies the format of a repository URL for a run that did
// not name one. The directory listing is consulted first, as a pacman
// database is named after its repository and has no fixed path to probe;
// the formats that do have one are then probed directly, so a URL whose
// upstream serves no listing is still identified. Only the formats the run
// accepts are considered.
func detectType(ctx context.Context, repoURL string, types []RepoType) (RepoType, error) {
	// A URL carrying a query or fragment is a mirrorlist or metalink
	// endpoint rather than a directory, and nothing can be joined onto it.
	if hasQueryOrFragment(repoURL) {
		return "", fmt.Errorf("cannot determine the repository type of %s; name it with --type", repoURL)
	}

	if listing, err := listDir(ctx, strings.TrimRight(repoURL, "/")+"/"); err == nil {
		if typ, ok := repoTypeOf(listing, types); ok {
			return typ, nil
		}
	}

	src := fetch.NewSource([]string{repoURL})
	for _, typ := range types {
		for _, probe := range probePaths(typ) {
			resp, err := src.Get(ctx, probe, fetch.GetOptions{})
			if err == nil {
				resp.Body.Close()
				return typ, nil
			}
			if !errors.Is(err, fetch.ErrNotFound) {
				return "", err
			}
		}
	}
	return "", fmt.Errorf("cannot determine the repository type of %s; name it with --type", repoURL)
}

// hasQueryOrFragment reports whether a URL carries a query or fragment,
// which makes it unusable as a base for joining repository paths onto.
// Unparsable URLs are reported as plain so the caller still probes them.
func hasQueryOrFragment(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	return u.RawQuery != "" || u.Fragment != ""
}

// resolveSource decides whether repoURL is a repository root or a mirror
// list and returns the source to fetch from, along with the URL whose path
// reflects the repository layout: repoURL itself for a direct repository,
// or the first mirror's URL when repoURL is a mirror list. RPM mirror lists
// may be plain text or metalink documents.
func resolveSource(ctx context.Context, repoURL string, typ RepoType) (*fetch.Source, string, error) {
	// Pacman databases are named after the repository, so there is no
	// fixed path to probe and pacman mirrorlists are config snippets
	// rather than URL lists; the URL is used directly.
	direct := fetch.NewSource([]string{repoURL})
	probes := probePaths(typ)
	if len(probes) == 0 {
		return direct, repoURL, nil
	}

	// Probe for repository metadata directly under the URL. A URL carrying a
	// query or fragment cannot have a repository path joined onto it, as the
	// path would land inside the query, so skip the probe: such URLs are
	// mirrorlist or metalink endpoints and are read as one below.
	if !hasQueryOrFragment(repoURL) {
		for _, probe := range probes {
			resp, err := direct.Get(ctx, probe, fetch.GetOptions{})
			if err == nil {
				resp.Body.Close()
				return direct, repoURL, nil
			}
			if !errors.Is(err, fetch.ErrNotFound) {
				return nil, "", err
			}
		}
	}

	// Fall back to reading the URL itself as a mirror list.
	body, err := fetch.ReadURL(ctx, repoURL)
	if err != nil {
		return nil, "", fmt.Errorf("read mirrorlist %s: %w", repoURL, err)
	}
	kind := "mirrorlist"
	var bases []string
	if typ == RepoRPM {
		if bases = fetch.ParseMetalink(body); len(bases) > 0 {
			kind = "metalink"
		}
	}
	if len(bases) == 0 {
		bases = fetch.ParseMirrorlist(body)
	}
	if len(bases) == 0 {
		return nil, "", fmt.Errorf("%s is neither a repository nor a mirrorlist", repoURL)
	}
	log.WithFields(log.Fields{"url": repoURL, "mirrors": len(bases), "kind": kind}).Info("Using mirror list for repository.")
	return fetch.NewSource(bases), bases[0], nil
}

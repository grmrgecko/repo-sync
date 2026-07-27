package fetch

import (
	"bufio"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	cfg "github.com/grmrgecko/repo-sync/config"
	log "github.com/sirupsen/logrus"
)

// Source is the set of base URLs a repository can be fetched from. Direct
// repositories have a single base; mirrorlists provide several with
// failover between them.
type Source struct {
	// mu guards bases and active.
	mu     sync.Mutex
	bases  []string
	active int
}

// NewSource builds a Source from base URLs, normalizing trailing slashes.
func NewSource(bases []string) *Source {
	s := &Source{}
	for _, base := range bases {
		s.bases = append(s.bases, strings.TrimRight(base, "/"))
	}
	return s
}

// Rebase trims a common relative suffix off every base URL so later fetches
// resolve against the parent directory. Bases without the suffix are kept
// unchanged.
func (s *Source) Rebase(suffix string) {
	suffix = "/" + strings.Trim(suffix, "/")
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, base := range s.bases {
		if strings.HasSuffix(base, suffix) {
			s.bases[i] = strings.TrimSuffix(base, suffix)
		} else {
			log.WithField("mirror", base).Warn("Mirror URL does not share the expected path suffix; using it unchanged.")
		}
	}
}

// GetOptions adjusts a single Source request. A non-zero ModifiedSince
// adds a conditional header, surfacing a 304 as ErrNotModified. A positive
// RangeFrom requests the remainder of a partially downloaded file.
type GetOptions struct {
	ModifiedSince time.Time
	RangeFrom     int64
}

// Get requests a repository-relative path, failing over to the next mirror
// on transport or server errors. A 404 from the responding mirror is
// treated as authoritative so optional files do not trigger failover.
func (s *Source) Get(ctx context.Context, reqPath string, o GetOptions) (*http.Response, error) {
	// Snapshot the bases so a concurrent Rebase cannot race the loop.
	s.mu.Lock()
	start := s.active
	bases := append([]string(nil), s.bases...)
	s.mu.Unlock()
	if len(bases) == 0 {
		return nil, errors.New("source has no mirror URLs")
	}

	var lastErr error
	for i := 0; i < len(bases); i++ {
		idx := (start + i) % len(bases)
		target := bases[idx] + "/" + strings.TrimPrefix(reqPath, "/")
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", agent())
		if !o.ModifiedSince.IsZero() {
			req.Header.Set("If-Modified-Since", o.ModifiedSince.UTC().Format(http.TimeFormat))
		}
		if o.RangeFrom > 0 {
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-", o.RangeFrom))
		}
		resp, err := client.Load().Do(req)
		if err != nil {
			lastErr = fmt.Errorf("fetch %s: %w", target, err)
			log.WithError(err).WithField("url", target).Debug("Mirror request failed.")
			continue
		}
		if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
			resp.Body.Close()
			return nil, fmt.Errorf("fetch %s: %w", target, ErrNotFound)
		}
		if resp.StatusCode == http.StatusNotModified && !o.ModifiedSince.IsZero() {
			resp.Body.Close()
			return nil, fmt.Errorf("fetch %s: %w", target, ErrNotModified)
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			resp.Body.Close()
			lastErr = fmt.Errorf("fetch %s: status %d", target, resp.StatusCode)
			log.WithField("url", target).WithField("status", resp.StatusCode).Debug("Mirror request failed.")
			continue
		}
		if idx != start {
			s.mu.Lock()
			s.active = idx
			s.mu.Unlock()
		}
		return resp, nil
	}
	return nil, lastErr
}

// ReadURL retrieves a raw URL fully into memory, limited to 4 MiB. It is
// used for probing mirrorlists, whose URLs may carry query strings that a
// Source cannot join paths onto.
func ReadURL(ctx context.Context, rawURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", agent())
	resp, err := client.Load().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
		return nil, fmt.Errorf("fetch %s: %w", rawURL, ErrNotFound)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("fetch %s: status %d", rawURL, resp.StatusCode)
	}
	// Read one byte past the cap so an oversized document can be detected
	// and reported rather than silently truncated into a bad parse.
	const maxSize = 4 << 20
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxSize {
		log.WithField("url", rawURL).Warn("Response exceeds the read limit and was truncated.")
		data = data[:maxSize]
	}
	return data, nil
}

// ParseMirrorlist extracts mirror base URLs from a mirrorlist body,
// skipping comments and lines that are not absolute HTTP URLs.
func ParseMirrorlist(body []byte) []string {
	seen := map[string]bool{}
	var bases []string
	scanner := bufio.NewScanner(bytes.NewReader(body))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		u, err := url.Parse(line)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			continue
		}
		base := strings.TrimRight(line, "/")
		if seen[base] {
			continue
		}
		seen[base] = true
		bases = append(bases, base)
	}
	return bases
}

// ParseMetalink extracts mirror base URLs from a metalink document's
// repomd.xml entries, keeping the document's preference order.
func ParseMetalink(body []byte) []string {
	const repomdSuffix = "/repodata/repomd.xml"
	var ml struct {
		Files []struct {
			Name string `xml:"name,attr"`
			URLs []struct {
				Value string `xml:",chardata"`
			} `xml:"resources>url"`
		} `xml:"files>file"`
	}
	if xml.Unmarshal(body, &ml) != nil {
		return nil
	}
	seen := map[string]bool{}
	var bases []string
	for _, file := range ml.Files {
		if file.Name != "repomd.xml" {
			continue
		}
		for _, u := range file.URLs {
			v := strings.TrimSpace(u.Value)
			if !strings.HasPrefix(v, "http://") && !strings.HasPrefix(v, "https://") {
				continue
			}
			base := strings.TrimSuffix(v, repomdSuffix)
			if base == v {
				continue
			}
			base = strings.TrimRight(base, "/")
			if seen[base] {
				continue
			}
			seen[base] = true
			bases = append(bases, base)
		}
	}
	return bases
}

// agent returns the user agent sent with every upstream request, falling
// back to the build identity when the configuration clears it.
func agent() string {
	if ua := cfg.C.Crawler.UserAgent; ua != "" {
		return ua
	}
	return cfg.Name + "/" + cfg.Version
}

// Get performs a plain GET of a raw URL with the mirror user agent, used
// for directory listings and other non-repository requests.
func Get(ctx context.Context, rawURL string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", agent())
	return client.Load().Do(req)
}

// Bases returns a copy of the mirror base URLs, primarily for tests and
// diagnostics.
func (s *Source) Bases() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.bases...)
}

// Active returns the base URL of the mirror currently being fetched from,
// which for a mirrorlist or metalink is the one that last answered rather
// than the list's own URL. It is empty when the source has no mirrors.
func (s *Source) Active() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active < 0 || s.active >= len(s.bases) {
		return ""
	}
	return s.bases[s.active]
}

package mirror

import (
	"archive/tar"
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/grmrgecko/repo-sync/fetch"
	log "github.com/sirupsen/logrus"
)

// archPackage is one package entry parsed from a pacman database.
type archPackage struct {
	filename string
	size     int64
	sums     map[string]string
}

// expect converts the package's metadata into a download expectation.
func (p *archPackage) expect() *fetch.Expect {
	e := &fetch.Expect{Size: p.size, Sums: p.sums}
	if p.size <= 0 {
		e.Size = -1
	}
	return e
}

// archExtras lists the companion metadata files mirrored beside the
// database, as suffixes appended to the repository name. All are optional;
// which ones exist varies by repository.
var archExtras = []string{
	".files",
	".db.tar.gz",
	".files.tar.gz",
	".links.tar.gz",
	".db.sig",
	".db.tar.gz.sig",
	".files.sig",
	".files.tar.gz.sig",
	".links.tar.gz.sig",
}

// syncArch synchronizes one pacman repository from src into destDir.
func syncArch(ctx context.Context, src *fetch.Source, repoURL, destDir string, opts *Options) error {
	keep := fetch.NewKeepSet(opts.Prune)
	miss := opts.newMissing(destDir)
	tr := newTrace(opts)
	ctx = tr.track(ctx)

	// Determine the database name, which matches the repository rather
	// than any fixed path.
	name, err := archDBName(ctx, src, repoURL)
	if err != nil {
		return err
	}
	log.WithField("database", name+".db").Debug("Resolved pacman database.")

	// Stage the database so a live tree keeps a consistent view until the
	// packages it references are in place.
	dbDst, err := fetch.LocalJoin(destDir, name+".db")
	if err != nil {
		return err
	}
	if _, err := fetch.File(ctx, src, name+".db", dbDst, nil, true, false); err != nil {
		return fmt.Errorf("fetch %s.db: %w", name, err)
	}
	pkgs, err := readArchDB(fetch.StagedOrFinal(dbDst))
	if err != nil {
		return err
	}

	// Download packages with their detached signatures. Signatures have no
	// published checksums, so an existing file is trusted as-is.
	var jobs []fetch.Job
	for _, pkg := range pkgs {
		dst, err := fetch.LocalJoin(destDir, pkg.filename)
		if err != nil {
			return err
		}
		jobs = append(jobs, fetch.Job{ReqPath: pkg.filename, Dst: dst, Want: pkg.expect()})
		jobs = append(jobs, fetch.Job{ReqPath: pkg.filename + ".sig", Dst: dst + ".sig", Want: &fetch.Expect{Size: -1}, Optional: true})
	}
	log.WithField("packages", len(pkgs)).Info("Synchronizing packages.")
	if opts.DryRun {
		fetch.PlanJobs(jobs, opts.Verify, keep)
	} else if _, err := fetch.Many(ctx, src, jobs, opts.Workers, opts.Verify, keep, miss); err != nil {
		return fmt.Errorf("fetch packages: %w", err)
	}

	// Stage the companion metadata files that exist upstream. A dry run
	// only counts them, as none are needed for planning.
	var extraJobs []fetch.Job
	for _, suffix := range archExtras {
		dst, err := fetch.LocalJoin(destDir, name+suffix)
		if err != nil {
			return err
		}
		extraJobs = append(extraJobs, fetch.Job{ReqPath: name + suffix, Dst: dst, Optional: true, Stage: true})
	}
	if opts.DryRun {
		fetch.PlanJobs(extraJobs, opts.Verify, keep)
	} else {
		states, err := fetch.Many(ctx, src, extraJobs, opts.Workers, opts.Verify, keep, nil)
		if err != nil {
			return fmt.Errorf("fetch repository metadata: %w", err)
		}
		for i, job := range extraJobs {
			if states[i] == fetch.FileMissing {
				// The upstream dropped the file; drop the local copy so a
				// stale companion or signature is never served.
				fetch.RemoveStale(job.Dst)
				continue
			}
			if err := fetch.PromoteStaged(job.Dst); err != nil {
				return err
			}
		}
	}

	// Promote the database last so the published metadata chain is
	// complete.
	if err := fetch.PromoteOrDiscard(dbDst, opts.DryRun); err != nil {
		return err
	}
	keep.Add(dbDst)

	// Publish the traces, upstream's included, before pruning so the keep
	// set covers them.
	tr.publish(ctx, destDir, src, keep, opts)

	// Remove files that are no longer part of the repository.
	if opts.Prune {
		fetch.PruneTree(destDir, keep, opts.PruneGrace, opts.DryRun)
	}

	// The database is published either way; missing packages only decide
	// whether the run reports itself as failed.
	return miss.Finish()
}

// archDBName finds the pacman database name for a repository, preferring
// the directory listing and falling back to probing names derived from the
// URL path, deepest segment first.
func archDBName(ctx context.Context, src *fetch.Source, repoURL string) (string, error) {
	if listing, err := listDir(ctx, strings.TrimRight(repoURL, "/")+"/"); err == nil {
		var names []string
		for f := range listing.files {
			if strings.HasSuffix(f, ".db") {
				names = append(names, strings.TrimSuffix(f, ".db"))
			}
		}
		sort.Strings(names)
		if len(names) > 1 {
			log.WithField("databases", names).Warn("Multiple pacman databases found; using the first.")
		}
		if len(names) > 0 {
			return names[0], nil
		}
	}

	// Listings can be disabled; a repository at .../core/os/x86_64 is
	// probed as x86_64.db, os.db, then core.db.
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
	for i := len(segs) - 1; i >= 0; i-- {
		resp, err := src.Get(ctx, segs[i]+".db", fetch.GetOptions{})
		if err == nil {
			resp.Body.Close()
			return segs[i], nil
		}
		if !errors.Is(err, fetch.ErrNotFound) {
			return "", err
		}
	}
	return "", fmt.Errorf("no pacman database found at %s", repoURL)
}

// readArchDB parses a pacman database archive and collects each package's
// file name, size, and checksums from its desc entry.
func readArchDB(filename string) ([]archPackage, error) {
	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	// The database carries no compression extension, so the format is
	// sniffed from its leading bytes.
	r, closeFn, err := fetch.SniffDecompressor(f)
	if err != nil {
		return nil, err
	}
	if closeFn != nil {
		defer closeFn()
	}

	tr := tar.NewReader(r)
	var pkgs []archPackage
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("parse pacman database %s: %w", filename, err)
		}
		if hdr.FileInfo().IsDir() || path.Base(hdr.Name) != "desc" {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(tr, 1<<20))
		if err != nil {
			return nil, fmt.Errorf("read desc entry %s: %w", hdr.Name, err)
		}
		fields := parseDesc(data)
		if fields["FILENAME"] == "" {
			continue
		}
		size, _ := strconv.ParseInt(fields["CSIZE"], 10, 64)
		sums := map[string]string{}
		if v := fields["SHA256SUM"]; v != "" {
			sums["sha256"] = strings.ToLower(v)
		}
		if v := fields["MD5SUM"]; v != "" {
			sums["md5"] = strings.ToLower(v)
		}
		pkgs = append(pkgs, archPackage{filename: fields["FILENAME"], size: size, sums: sums})
	}
	return pkgs, nil
}

// parseDesc extracts the first value of each %FIELD% block from a pacman
// desc entry.
func parseDesc(data []byte) map[string]string {
	fields := map[string]string{}
	field := ""
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		switch {
		case line == "":
			field = ""
		case strings.HasPrefix(line, "%") && strings.HasSuffix(line, "%"):
			field = strings.Trim(line, "%")
		case field != "":
			if _, ok := fields[field]; !ok {
				fields[field] = line
			}
		}
	}
	return fields
}

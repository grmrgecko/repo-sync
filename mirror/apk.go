package mirror

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/grmrgecko/repo-sync/fetch"
	log "github.com/sirupsen/logrus"
)

// APKIndexName is the fixed name of an Alpine repository index.
const APKIndexName = "APKINDEX.tar.gz"

// apkPackage is one package entry parsed from an APKINDEX. Alpine's C:
// pull checksum only covers a package's control segment, so mirroring can
// verify downloads by size alone.
type apkPackage struct {
	name    string
	version string
	size    int64
}

// filename returns the package file name as served beside the index.
func (p *apkPackage) filename() string {
	return p.name + "-" + p.version + ".apk"
}

// expect converts the package's metadata into a download expectation.
func (p *apkPackage) expect() *fetch.Expect {
	e := &fetch.Expect{Size: p.size}
	if p.size <= 0 {
		e.Size = -1
	}
	return e
}

// syncApk synchronizes one Alpine repository from src into destDir.
func syncApk(ctx context.Context, src *fetch.Source, destDir string, opts *Options) error {
	keep := fetch.NewKeepSet(opts.Prune)
	miss := opts.newMissing(destDir)
	tr := newTrace(opts)
	ctx = tr.track(ctx)

	// Stage the index so a live tree keeps a consistent view until the
	// packages it references are in place.
	idxDst, err := fetch.LocalJoin(destDir, APKIndexName)
	if err != nil {
		return err
	}
	if _, err := fetch.File(ctx, src, APKIndexName, idxDst, nil, true, false); err != nil {
		return fmt.Errorf("fetch %s: %w", APKIndexName, err)
	}
	pkgs, err := readApkIndex(fetch.StagedOrFinal(idxDst))
	if err != nil {
		return err
	}

	// Download the packages, which live beside the index.
	var jobs []fetch.Job
	for _, pkg := range pkgs {
		dst, err := fetch.LocalJoin(destDir, pkg.filename())
		if err != nil {
			return err
		}
		jobs = append(jobs, fetch.Job{ReqPath: pkg.filename(), Dst: dst, Want: pkg.expect()})
	}
	log.WithField("packages", len(pkgs)).Info("Synchronizing packages.")
	if opts.DryRun {
		fetch.PlanJobs(jobs, opts.Verify, keep)
	} else if _, err := fetch.Many(ctx, src, jobs, opts.Workers, opts.Verify, keep, miss); err != nil {
		return fmt.Errorf("fetch packages: %w", err)
	}

	// Promote the index last so the published metadata chain is complete.
	if err := fetch.PromoteOrDiscard(idxDst, opts.DryRun); err != nil {
		return err
	}
	keep.Add(idxDst)

	// Publish the traces, upstream's included, before pruning so the keep
	// set covers them.
	tr.publish(ctx, destDir, src, keep, opts)

	// Remove files that are no longer part of the repository.
	if opts.Prune {
		fetch.PruneTree(destDir, keep, opts.PruneGrace, opts.DryRun)
	}

	// The index is published either way; missing packages only decide
	// whether the run reports itself as failed.
	return miss.Finish()
}

// readApkIndex parses an APKINDEX.tar.gz archive. The file is a signature
// segment and an index segment as concatenated gzip streams, which the
// multistream gzip reader and tar reader walk through transparently.
func readApkIndex(filename string) ([]apkPackage, error) {
	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("parse apk index %s: %w", filename, err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("parse apk index %s: %w", filename, err)
		}
		if hdr.Name != "APKINDEX" {
			continue
		}
		pkgs, err := parseApkIndex(tr)
		if err != nil {
			return nil, fmt.Errorf("parse apk index %s: %w", filename, err)
		}
		return pkgs, nil
	}
	return nil, fmt.Errorf("no APKINDEX entry in %s", filename)
}

// parseApkIndex reads APKINDEX stanzas, collecting each package's name,
// version, and file size from the P:, V:, and S: fields.
func parseApkIndex(r io.Reader) ([]apkPackage, error) {
	var pkgs []apkPackage
	var cur apkPackage

	// flush records the current stanza when it is complete.
	flush := func() {
		if cur.name != "" && cur.version != "" {
			pkgs = append(pkgs, cur)
		}
		cur = apkPackage{}
	}

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 256*1024), 256*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}
		field, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch field {
		case "P":
			cur.name = value
		case "V":
			cur.version = value
		case "S":
			cur.size, _ = strconv.ParseInt(value, 10, 64)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	flush()
	return pkgs, nil
}

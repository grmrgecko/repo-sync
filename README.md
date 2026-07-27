# repo-sync

A universal Linux package repository synchronization tool. It mirrors remote
repositories into a local directory tree, preserving the upstream layout so
the result can be served directly to package managers.

Supported repository types, named by `--type` when a run has to be limited
to some of them:

- **rpm** — yum/dnf repositories (`repodata/repomd.xml`), including
  plain-text mirrorlist and metalink URLs with failover between mirrors.
- **deb** — apt repositories, both standard (`dists/<suite>` with a shared
  `pool/`) and flat layouts, including `Acquire-By-Hash` population.
- **arch** — pacman repositories (`<name>.db`), including detached package
  signatures and companion metadata (`.files`, `.db.tar.gz`, `.links.tar.gz`).
- **apk** — Alpine Linux repositories (`APKINDEX.tar.gz` with packages
  beside it).

## Usage

```
repo-sync sync [flags] <url> [<url> ...] <destination-directory>
```

The last argument is always the destination directory; every argument before
it is a repository or mirrorlist URL, synchronized one after the other.

Each repository is identified by its own metadata, so no type has to be
given: an upstream publishing both yum and apt repositories is mirrored in
one run. `--type` limits which formats are accepted, and is repeatable; a
run limited to a single type synchronizes every URL as that type rather than
identifying it, which is what a mirrorlist or metalink URL needs.

A configuration file is optional: without one the built-in defaults apply.
When one is present its `crawler` section supplies the defaults for every
command, so `workers`, `prune_grace`, `request_timeout`, `user_agent`,
`missing_mode`, `missing_retries`, and `discover_cache` are shared by the
sync commands and the server. The global flags `--config-path`, `--log-level`, `--user-agent`,
and `--request-timeout` override the configuration for any command.

```sh
# Mirror one repository. The URL path is copied below the destination, so
# this produces ./mirror/repos/CentOS/7/EA4/.
repo-sync sync https://example.com/repos/CentOS/7/EA4/ ./mirror

# Trim the first two path components: ./mirror/7/EA4/.
repo-sync sync --trim 2 https://example.com/repos/CentOS/7/EA4/ ./mirror

# No path copying at all: the repository lands directly in ./mirror.
repo-sync sync --flat https://example.com/repos/CentOS/7/EA4/ ./mirror

# Multiple repositories in one run.
repo-sync sync https://example.com/repos/a/ https://example.com/repos/b/ ./mirror

# Crawl directory listings for repositories, at most 4 levels deep.
repo-sync sync --discover --depth 4 https://example.com/repos/ ./mirror

# A pacman repository. The database name is discovered from the directory
# listing, or by probing URL path segments when listings are disabled.
repo-sync sync https://example.com/archlinux/core/os/x86_64/ ./mirror

# An Alpine repository is one arch directory; discovery syncs every arch
# of a release/repo tree in one run.
repo-sync sync https://dl-cdn.alpinelinux.org/alpine/v3.24/community/x86_64/ ./mirror
repo-sync sync --discover --depth 1 https://dl-cdn.alpinelinux.org/alpine/v3.24/community/ ./mirror

# A Fedora-style metalink URL; mirrors are used in preference order. Nothing
# can be crawled or probed through a metalink, so the type is given. The
# mirrors' paths differ, so --flat keeps the destination stable.
repo-sync sync --type rpm --flat 'https://mirrors.fedoraproject.org/metalink?repo=epel-9&arch=x86_64' ./mirror/epel9

# An apt suite. Pool files resolve against the archive root, producing
# ./mirror/debian/dists/bookworm/ and ./mirror/debian/pool/.
repo-sync sync https://deb.example.com/debian/dists/bookworm ./mirror

# Limit an apt mirror to specific components and architectures. Include
# "source" as an architecture to keep source indexes.
repo-sync sync --component main --arch amd64,source https://deb.example.com/debian/dists/bookworm ./mirror

# Limit a crawl to one format when an upstream publishes several.
repo-sync sync --type rpm --discover https://repo.example.com/ ./mirror
```

## Discovery

`--discover` treats each URL as a directory index and crawls it for
repositories, at most `--depth` levels down. A directory holding repository
metadata is mirrored as a whole and is not crawled further, so a vendor
archive is mirrored by pointing one run at its root.

`--exclude` keeps parts of that tree out of the crawl. Each pattern is a
glob, repeatable, and matched against the entry name wherever it appears in
the tree; a pattern containing a slash is matched against the path below the
crawled URL instead, pinning it to one place, and a leading slash anchors it
to that URL. An excluded directory is never listed, so nothing below it is
mirrored either.

`--include-file` mirrors loose files the crawl passes on its way, which no
repository's metadata lists — signing keys, release notes, and packages
published outside a repository. Each pattern is a glob, repeatable, and
matched by name or by path on the same rules as `--exclude`, so `'*.rpm'`
takes loose packages from anywhere in the tree while `'/*.rpm'` takes only
those beside the crawled URL itself. Only files in directories that are
*not* repositories are considered: a repository's own files come from its
metadata, and pruning there would remove anything else. These files carry no
published checksum, so they are revalidated against their modification time
rather than re-downloaded, and `--prune` never removes them.

Crawling a vendor archive costs hundreds of directory listings, so what a
crawl found is cached at the destination root in `.repo-sync-state.json` and
reused until it ages out. `--discover-cache` sets that lifetime, three days
by default; `0` scans on every run, and changing `--depth`, `--type`,
`--exclude`, or `--include-file` scans again regardless, so a new pattern
never waits out the cache.

The cache is also the record of what the upstream used to publish. A scan
that no longer finds a repository or a loose file it recorded before marks it
withdrawn, and `--prune` then removes it from the mirror once it has been
gone for `--prune-grace`, so a vendor dropping a distribution does not leave
its tree behind forever. A crawl that could not list part of the tree
withdraws nothing: an upstream that failed to answer is not one that dropped
a repository. Without `--prune` the withdrawal is recorded and logged but
nothing is removed.

```sh
# Mirror MySQL's yum and apt repositories in one run, skipping the
# distributions this mirror does not serve, and keeping the signing keys
# and loose packages published outside the repositories.
repo-sync sync --discover --depth 6 \
  --exclude sles --exclude 'fc*' --exclude docker \
  --include-file '*.rpm' --include-file '*.deb' --include-file 'RPM-GPG-KEY-mysql*' \
  http://repo.mysql.com/ ./mirror
```

## Trace files

A public mirror is expected to say something about itself: who runs it,
where it is, and when it last synchronized. Debian archives established a
convention for this — a file at `project/trace/<host>` inside the archive —
and downstream mirrors and mirror checkers read it. `--trace` publishes one
into every repository synchronized, and the mirror server publishes one
into every repository it crawls when the `trace` section is enabled.

```sh
repo-sync sync --trace --trace-maintainer 'Jane <jane@example.com>' \
  https://example.com/repos/CentOS/7/EA4/ ./mirror
```

```
Mon Jul 27 20:31:58 UTC 2026
Date: Mon, 27 Jul 2026 20:31:58 +0000
Date-Started: Mon, 27 Jul 2026 20:29:14 +0000
Creator: repo-sync 0.1.0
Running on host: mirror.example.com
Maintainer: Jane <jane@example.com>
Repository type: rpm
Upstream-mirror: https://example.com/repos/CentOS/7/EA4
Total bytes received: 5726208
Total time spent syncing: 164
Average rate: 34915 B/s
```

`Upstream-mirror` records the mirror actually fetched from, which for a
mirrorlist or metalink is the one that answered rather than the list's own
URL — the reason a trace is written here rather than by whatever schedules
the sync, which cannot know that.

The file is placed in the repository directory, or for apt at the archive
root, where the convention puts it. It is registered as part of the
repository, so `--prune` never removes it even when the repository and the
trace share a directory. `--dry-run` reports what would change without
writing one.

Everything but the dates and transfer figures comes from configuration,
and each field has a matching flag: `--trace-host` (defaults to the system
hostname), `--trace-maintainer`, `--trace-sponsor`, `--trace-country`,
`--trace-location`, and `--trace-throughput`. Setting them in the `trace`
section of a config file avoids repeating them on every run; `--trace` and
`--no-trace` then switch tracing on or off per run. Fields left unset are
omitted from the file rather than written empty.

Upstream's own traces are mirrored alongside, so `project/trace/` names
every mirror the content passed through and a reader can follow the chain
back to the archive it originated from. A trace upstream publishes under
this mirror's own host name is skipped: that file describes a different
mirror, and the one written here has to win. Upstreams that publish no
traces, or that do not serve a directory index for them, are not an error;
nothing is copied and the run continues.

Traces work the same on the mirror server, whose crawls are concurrent:
each crawl accounts for its own transfer rather than reading the shared
counters, so the totals in each trace describe that crawl alone. Every
completed crawl republishes the repository's trace, so the timestamps
track how current the cached copy is.

## Mirror server

`repo-sync server` runs a caching mirror in front of upstream repositories.
Repositories are discovered from client requests and then kept synchronized
in the background, so package managers can be pointed straight at it.

```sh
repo-sync server --config-path ./config.yaml
```

Without `--config-path` the config is read from `./config.yaml`,
`~/.config/repo-sync/config.yaml`, or `/etc/repo-sync/config.yaml`. See
[config.example.yaml](config.example.yaml) for a documented configuration
covering the listener, domains, mounts, and crawler tuning. The server
needs at least one domain and one mount; the sync commands do not.

To run it as a systemd service, install `/etc/repo-sync/config.yaml` and:

```sh
repo-sync service install
repo-sync service start
```

`service` also accepts `stop`, `restart`, `status`, and `uninstall`. The
installed unit runs `repo-sync server` as a notify service, restarts on
failure, and reloads its configuration on `systemctl reload repo-sync`.

## Building

```sh
make
```

The build stamps the binary with the contents of the `VERSION` file plus the
git commit and build date, shown by `repo-sync --version`. A plain
`go build` also works but reports the version as `dev`.

## Testing

```sh
make test
```

The suite is hermetic: fixture repositories for every supported format
(rpm, deb, arch, apk) are generated in temp directories and served over
local HTTP, so no network access is required.

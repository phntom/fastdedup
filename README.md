# fastdedup

Deduplicate millions of files in seconds using reflinks. Zero extra disk space, no data copying — just instant copy-on-write deduplication. Optimized for btrfs, also works on XFS and ZFS.

```
$ fastdedup /srv/backups

Pass 1: Scanning file sizes in /srv/backups
  Scanning: [████████████████░░░░░░░░░░░░░░]  53% 12,345/s ~1.5m
  Scanned 8,860,298 files, 571,197 unique sizes

Top file sizes by potential savings:
     #        Size     Count     Savings
     1     4.0 GiB        21    80.0 GiB
     2    76.5 GiB         2    76.5 GiB
     3   500.0 MiB       114    55.2 GiB
     4    40.6 GiB         2    40.6 GiB
     5     4.0 GiB         7    24.0 GiB
  ... and 9,995 more sizes

Pass 2: Deduplicating:
  [    1/10000]    4.0 GiB × 21        ✓ 18 deduped, 72.0 GiB saved
  [    2/10000]   76.5 GiB × 2         ✓ 1 deduped, 76.5 GiB saved
  [    3/10000]  500.0 MiB × 114      [█████████████████████░░░░░░░░░]  71% (2%) ~4.2m

Done in 5m12.3s!
  Files deduped:    3,847
  Space saved:      892.4 GiB
  Already deduped:  12,156
  Errors:           0
```

## How it works

1. **Pass 1** — scans the directory tree and counts files by size, ranking by potential savings
2. **Pass 2** — for each target size, scans for matching files and replaces duplicates with reflinks (use `--batch` to collect all sizes in one pass for speed at the cost of memory)

Reflinks are instant — the filesystem shares the underlying data blocks between files. Each file remains independent (copy-on-write), so modifying one won't affect others.

## Supported filesystems

fastdedup uses the `FICLONE` ioctl to create reflinks. Any Linux filesystem that supports reflinks will work.

| Filesystem | Reflinks | Extent detection | Notes |
|---|---|---|---|
| **btrfs** | yes | yes (FIEMAP) | Full support — fastest with extent-based skip of already-deduped files |
| **XFS** | yes | yes (FIEMAP) | Requires `reflink=1` (default since mkfs.xfs 5.1) |
| **ZFS** | yes | no | Requires OpenZFS 2.2+ with `block_cloning` enabled |
| **ext4, others** | no | — | `--dry-run` works for analysis, but actual dedup requires reflink support |

On filesystems without FIEMAP (like ZFS), fastdedup falls back to byte-by-byte content comparison. This is slightly slower than the extent-based approach on btrfs/XFS but produces identical results.

## Installation

### Ubuntu (PPA)
```bash
sudo add-apt-repository -y ppa:phntm/ppa
sudo apt update
sudo apt install -y fastdedup
```

Supports Ubuntu 22.04 (jammy), 24.04 (noble), 24.10 (oracular), 25.04 (plucky), 25.10 (questing), and 26.04 (resolute).

### Debian/Ubuntu (manual)

Download from the [releases page](https://github.com/phntom/fastdedup/releases):
```bash
sudo dpkg -i fastdedup_*_amd64.deb
```

### RHEL/Fedora/CentOS
```bash
sudo rpm -i fastdedup-*-1.x86_64.rpm
```

### Alpine Linux
```bash
sudo apk add --allow-untrusted fastdedup_*_x86_64.apk
```

### Self-extracting archive
```bash
chmod +x fastdedup-*-linux-amd64.sh
sudo ./fastdedup-*-linux-amd64.sh
```

### Binary
```bash
chmod +x fastdedup-linux-amd64
sudo mv fastdedup-linux-amd64 /usr/local/bin/fastdedup
```

## Scheduled deduplication

Install `fastdedup-daily` or `fastdedup-weekly` to automatically deduplicate all mounted btrfs, XFS, and ZFS filesystems:

### Ubuntu (PPA)
```bash
sudo apt install fastdedup-daily   # runs daily via /etc/cron.daily
# or
sudo apt install fastdedup-weekly  # runs weekly via /etc/cron.weekly
```

### Manual (from GitHub release)
```bash
sudo dpkg -i fastdedup_*_amd64.deb fastdedup-daily_*_all.deb
```

### Configuration

Edit `/etc/default/fastdedup` to customize:

```bash
# Set to "no" to disable automatic deduplication
ENABLED=yes

# Options passed to fastdedup (default: quiet mode)
OPTIONS="-q"

# Space-separated list of mount points to deduplicate
# Leave empty to auto-detect all mounted btrfs, XFS, and ZFS filesystems
MOUNTPOINTS=""

# Minimum file size in bytes (default: 524288 = 512 KiB)
# MIN_SIZE=524288

# Log file for cron output (rotated by logrotate)
LOG_FILE="/var/log/fastdedup.log"

# Disable the dedup cache (reprocess everything each run)
# export FASTDEDUP_NO_CACHE=1

# Slack/Mattermost webhook for run summaries
# export FASTDEDUP_WEBHOOK_UPDATES=https://mattermost.example.com/hooks/xxxx

# Slack/Mattermost webhook for critical alerts (errors requiring intervention)
# export FASTDEDUP_WEBHOOK_ALERTS=https://mattermost.example.com/hooks/xxxx

# Machine identifier for webhook messages (defaults to hostname)
# export FASTDEDUP_HOST_ID=my-server-01

# Healthchecks.io dead man's switch URL (pinged after each successful run)
# export FASTDEDUP_HEALTHCHECK_URL=https://hc-ping.com/xxxx

# Disable anonymized error reporting to ~/.cache/fastdedup/report.txt
# export FASTDEDUP_NO_REPORT_FILE=1
```

By default, the cron job auto-detects all mounted btrfs, XFS, and ZFS filesystems using `findmnt` and runs fastdedup in quiet mode on each. A lock file prevents overlapping runs.

## Usage

```bash
fastdedup [flags] [directory]
```

| Flag | Default | Description |
|------|---------|-------------|
| `--min-size` | 524288 | Minimum file size to process in bytes (512 KiB) |
| `--max-sizes` | 1,000,000 | Maximum unique file sizes to track in pass 1 |
| `--top` | 10,000 | Number of top file sizes by potential savings to dedup in pass 2 |
| `--dry-run` | false | Report what would be deduped without making changes |
| `-v` | false | Show file paths of deduped files and detailed diagnostics |
| `-q` | false | Quiet mode — only print final summary (for cronjobs) |
| `--batch` | false | Collect all target files in one pass (faster, uses more memory) |
| `--low-memory` | false | Scan separately for each file size (lowest memory, slower) |
| `--mem-budget` | 256 | Memory budget in MiB for path cache in default mode |
| `--no-cache` | false | Reprocess all file sizes even if unchanged since last run |
| `--hardlink` | false | Use hard links instead of reflinks (works on any filesystem — see warning below) |
| `--fix-perms` | false | Temporarily add write permission to read-only directories during dedup, then restore |
| `--snapshots` | false | Include `.snapshots` directories (skipped by default) |
| `--raw-sizes` | false | Show raw byte counts instead of human-readable |
| `--version` | false | Print version and exit |

### Hard link mode

`--hardlink` works on any Linux filesystem, but comes with important trade-offs compared to reflinks:

- **Editing one file changes all copies** — hard-linked files share the same inode, so there is no copy-on-write isolation
- **Metadata is shared** — permissions, ownership, and timestamps are the same for all linked files
- **Deleting one copy does not free space** — the data remains until the last link is removed

Use `--dry-run --hardlink` first to see what would be linked. Only use this mode if you understand the implications.

### Remembering previous runs

By default, fastdedup saves a small fingerprint of each processed file size group to `~/.cache/fastdedup/`. On the next run over the same directory, it skips groups where the set of filenames hasn't changed — meaning no files were added, removed, or renamed. This makes repeated runs over large directories nearly instant when little has changed.

Use `--no-cache` or `FASTDEDUP_NO_CACHE=1` to ignore saved state and reprocess everything.

### Concurrent run protection

fastdedup uses per-directory lock files to prevent multiple instances from processing the same directory simultaneously. If a second instance is started on the same path, it exits immediately with a clear error. Different directories can be processed in parallel. The cron job also uses `flock` to prevent overlapping scheduled runs.

### Webhooks

Set `FASTDEDUP_WEBHOOK_UPDATES` to receive run summaries in Slack or Mattermost after each run. Set `FASTDEDUP_WEBHOOK_ALERTS` to receive alerts when errors require investigation. Messages include the machine identifier (`FASTDEDUP_HOST_ID` or hostname) so you can use a shared channel for multiple servers.

Set `FASTDEDUP_HEALTHCHECK_URL` to ping a [healthchecks.io](https://healthchecks.io) (or compatible) dead man's switch URL after each successful run.

## Building from source

```bash
go build -ldflags="-s -w" -o fastdedup .
```

## Releasing

Requires [fpm](https://github.com/jordansissel/fpm), `devscripts`, and `dput`:
```bash
gem install --user-install fpm
sudo apt install devscripts dput
```

```bash
./release.sh <version>
```

This will:
1. Build binaries for all architectures
2. Create `.deb`, `.rpm`, `.apk`, and `.sh` packages
3. Tag the commit as `v<version>`
4. Build and upload source packages to the [Ubuntu PPA](https://launchpad.net/~phntm/+archive/ubuntu/ppa)

To publish on GitHub after running the release script:
```bash
git push origin v<version>
gh release create v<version> release/<version>/* --title 'v<version>' --notes-file release/<version>/description.md
```

## Testing

```bash
go run ./cmd/testdedup                        # test on default /tmp
go run ./cmd/testdedup -dir /mnt/btrfs        # test on a specific filesystem
```

## Supported architectures

amd64, arm64, i386, armhf, riscv64, ppc64le, s390x, mips64le

## Package formats

- `.deb` - Debian, Ubuntu, and derivatives
- `.rpm` - RHEL, Fedora, CentOS, openSUSE
- `.apk` - Alpine Linux
- `.sh` - Self-extracting shell archive (any Linux)
- Raw binaries for manual installation

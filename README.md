# fastdedup

Deduplicate millions of files on btrfs in seconds using reflinks. Zero extra disk space, no data copying — just instant copy-on-write deduplication.

```
$ fastdedup /srv/backups

Pass 1: Scanning file sizes...
  Scanned 8,860,298 files, 571,197 unique sizes

Top file sizes by potential savings:
     #        Size     Count     Savings
     1     4.0 GiB        21    80.0 GiB
     2    76.5 GiB         2    76.5 GiB
     3   500.0 MiB       114    55.2 GiB
     4    40.6 GiB         2    40.6 GiB
     5     4.0 GiB         7    24.0 GiB
     6     3.6 GiB         3     7.1 GiB
     7    81.7 MiB        95     7.5 GiB
     8    81.7 MiB        90     7.1 GiB
     9    81.7 MiB        90     7.1 GiB
    10    81.9 MiB        84     6.6 GiB
  ... and 9,990 more sizes

Pass 2: Deduplicating:
  [    1/10000]    4.0 GiB × 21        ✓ 18 deduped, 72.0 GiB saved
  [    2/10000]   76.5 GiB × 2         ✓ 1 deduped, 76.5 GiB saved
  [    3/10000]  500.0 MiB × 114      [█████████████████████░░░░░░░░░]  71%
```

## How it works

1. **Pass 1** — scans the directory tree and counts files by size, ranking by potential savings
2. **Pass 2** — for each target size, scans for matching files and replaces duplicates with btrfs reflinks (use `--batch` to collect all sizes in one pass for speed at the cost of memory)

Reflinks are instant — the filesystem shares the underlying data blocks between files. Each file remains independent (copy-on-write), so modifying one won't affect others.

## Installation

### Ubuntu (PPA)
```bash
sudo add-apt-repository -y ppa:phntm/ppa
sudo apt update
sudo apt install -y fastdedup
```

Supports Ubuntu 22.04 (jammy), 24.04 (noble), 24.10 (oracular), and 25.04 (plucky).

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

## Usage

```bash
fastdedup [flags] [directory]
```

| Flag | Default | Description |
|------|---------|-------------|
| `--max-sizes` | 1,000,000 | Maximum unique file sizes to track in pass 1 |
| `--top` | 10,000 | Number of top file sizes by potential savings to dedup in pass 2 |
| `--dry-run` | false | Report what would be deduped without making changes |
| `-v` | false | Show file paths of deduped files and detailed diagnostics |
| `-q` | false | Quiet mode — only print final summary (for cronjobs) |
| `--batch` | false | Collect all target files in one pass (faster, uses more memory) |
| `--snapshots` | false | Include `.snapshots` directories (skipped by default) |
| `--raw-sizes` | false | Show raw byte counts instead of human-readable |
| `-C` | | Change to directory before running |

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

## Supported architectures

amd64, arm64, i386, armhf, riscv64, ppc64le, s390x, mips64le

## Package formats

- `.deb` - Debian, Ubuntu, and derivatives
- `.rpm` - RHEL, Fedora, CentOS, openSUSE
- `.apk` - Alpine Linux
- `.sh` - Self-extracting shell archive (any Linux)
- Raw binaries for manual installation

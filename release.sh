#!/usr/bin/env bash
set -euo pipefail

VERSION="${1:?Usage: $0 <version>}"
RELEASE_DIR="release/${VERSION}"
NAME="fastdedup"
DESC="Fast file deduplication using reflinks (btrfs, XFS, ZFS)"
ARCHES=(amd64 arm64 386 arm riscv64 ppc64le s390x mips64le)
MAINTAINER="PHANTOm <phantom@kix.co.il>"
GPG_KEY="B2BE8C2EBFB7AAC6572E933C779CD5498B743A1B"
PPA_SERIES=(jammy noble questing)
PPA_TARGET="phntm-ppa"

# Series that need a bundled Go toolchain (system Go is too old)
BUNDLED_GO_SERIES=(jammy)
BUNDLED_GO_VERSION="1.22.12"
BUNDLED_GO_URL="https://go.dev/dl/go${BUNDLED_GO_VERSION}.linux-amd64.tar.gz"

# Ensure fpm is available
if ! command -v fpm &>/dev/null; then
  FPM_PATH="$HOME/.local/share/gem/ruby/$(ruby -e 'puts RUBY_VERSION.sub(/\.\d+$/,".0")')/bin/fpm"
  if [[ -x "$FPM_PATH" ]]; then
    export PATH="$(dirname "$FPM_PATH"):$PATH"
  else
    echo "ERROR: fpm not found. Install with: gem install --user-install fpm"
    exit 1
  fi
fi

mkdir -p "$RELEASE_DIR"

# ── Build binaries ──────────────────────────────────────────────────
echo "==> Building binaries for ${#ARCHES[@]} architectures..."
for arch in "${ARCHES[@]}"; do
  printf "  %-10s" "linux/$arch"
  GOOS=linux GOARCH="$arch" go build -ldflags="-s -w -X main.version=${VERSION}" \
    -o "${RELEASE_DIR}/${NAME}-linux-${arch}" .
  echo "ok"
done

# ── Package with fpm ────────────────────────────────────────────────
echo "==> Creating packages..."
for arch in "${ARCHES[@]}"; do
  # Map Go arch → distro arch names
  case "$arch" in
    amd64)    DEB=amd64    RPM=x86_64   APK=x86_64   ;;
    arm64)    DEB=arm64    RPM=aarch64   APK=aarch64   ;;
    386)      DEB=i386     RPM=i686      APK=x86       ;;
    arm)      DEB=armhf    RPM=armv7hl   APK=armv7     ;;
    riscv64)  DEB=riscv64  RPM=riscv64   APK=riscv64   ;;
    ppc64le)  DEB=ppc64el  RPM=ppc64le   APK=ppc64le   ;;
    s390x)    DEB=s390x    RPM=s390x     APK=s390x     ;;
    mips64le) DEB=mips64el RPM=mips64el  APK=mips64    ;;
  esac

  STAGING=$(mktemp -d)
  mkdir -p "${STAGING}/usr/bin"
  cp "${RELEASE_DIR}/${NAME}-linux-${arch}" "${STAGING}/usr/bin/${NAME}"

  FPM_COMMON=(
    -s dir -n "$NAME" -v "$VERSION"
    --description "$DESC"
    -C "$STAGING"
  )

  printf "  %-10s deb " "linux/$arch"
  fpm "${FPM_COMMON[@]}" -t deb -a "$DEB" -p "${RELEASE_DIR}/" usr/bin/${NAME} 2>/dev/null
  printf "rpm "
  fpm "${FPM_COMMON[@]}" -t rpm -a "$RPM" -p "${RELEASE_DIR}/" usr/bin/${NAME} 2>/dev/null
  printf "apk "
  fpm "${FPM_COMMON[@]}" -t apk -a "$APK" -p "${RELEASE_DIR}/" usr/bin/${NAME} 2>/dev/null
  printf "sh "
  fpm "${FPM_COMMON[@]}" -t sh  -a "$DEB" -p "${RELEASE_DIR}/${NAME}-${VERSION}-linux-${arch}.sh" usr/bin/${NAME} 2>/dev/null
  echo ""

  rm -rf "$STAGING"
done

# ── Tag ─────────────────────────────────────────────────────────────
if git rev-parse "v${VERSION}" &>/dev/null; then
  echo "==> Tag v${VERSION} already exists, skipping"
else
  echo "==> Tagging v${VERSION}"
  git tag -a "v${VERSION}" -m "Release v${VERSION}"
fi

# ── Ubuntu PPA ──────────────────────────────────────────────────────
echo ""
echo "==> Building and uploading Ubuntu PPA source packages..."

# Ensure dput and debuild are available
for cmd in dput debuild; do
  if ! command -v "$cmd" &>/dev/null; then
    echo "ERROR: $cmd not found. Install with: sudo apt install devscripts dput"
    exit 1
  fi
done

# Vendor dependencies (needed for offline Launchpad builds)
go mod vendor

# Remove any stale built binary so it doesn't end up in the source tarball
rm -f "${NAME}"

# Save original debian files (will be modified per-series)
cp debian/control debian/control.orig
cp debian/rules debian/rules.orig

# Download Go toolchain for bundled-Go series (if any)
needs_bundled=false
for series in "${PPA_SERIES[@]}"; do
  for bs in "${BUNDLED_GO_SERIES[@]}"; do
    [[ "$series" == "$bs" ]] && needs_bundled=true
  done
done

if $needs_bundled; then
  GO_TARBALL="go${BUNDLED_GO_VERSION}.linux-amd64.tar.gz"
  if [[ ! -f "${RELEASE_DIR}/${GO_TARBALL}" ]]; then
    echo "==> Downloading Go ${BUNDLED_GO_VERSION} toolchain..."
    curl -fsSL -o "${RELEASE_DIR}/${GO_TARBALL}" "${BUNDLED_GO_URL}"
  fi
fi

PPA_REV=1
for series in "${PPA_SERIES[@]}"; do
  PPA_VERSION="${VERSION}~ppa${PPA_REV}~${series}"

  # Check if this series needs bundled Go
  use_bundled=false
  for bs in "${BUNDLED_GO_SERIES[@]}"; do
    [[ "$series" == "$bs" ]] && use_bundled=true
  done

  if $use_bundled; then
    # Bundle Go toolchain into source tree
    echo "  ${series}: bundling Go ${BUNDLED_GO_VERSION} toolchain..."
    rm -rf _go
    mkdir -p _go
    tar -xzf "${RELEASE_DIR}/${GO_TARBALL}" -C _go --strip-components=1

    # Use modified debian/control without golang-go dependency
    cat > debian/control <<CTRL
Source: ${NAME}
Section: utils
Priority: optional
Maintainer: ${MAINTAINER}
Build-Depends: debhelper-compat (= 13)
Standards-Version: 4.6.2
Homepage: https://github.com/phntom/fastdedup
Vcs-Git: https://github.com/phntom/fastdedup.git
Vcs-Browser: https://github.com/phntom/fastdedup

Package: ${NAME}
Architecture: amd64
Depends: \${shlibs:Depends}, \${misc:Depends}
Description: ${DESC}
 fastdedup performs two-pass deduplication on filesystems that support
 reflinks (btrfs, XFS, ZFS). It surveys file sizes to identify the
 most impactful duplicates, then deduplicates them using reflinks for
 instant, copy-on-write deduplication with no extra disk space.
CTRL

    # Use modified debian/rules that uses bundled Go
    cat > debian/rules <<'RULES'
#!/usr/bin/make -f

export GOROOT := $(CURDIR)/_go
export PATH := $(GOROOT)/bin:$(PATH)
DEB_VERSION := $(shell dpkg-parsechangelog -S Version)

%:
	dh $@

override_dh_auto_build:
	HOME=$(CURDIR) GOMODCACHE=$(CURDIR)/.gomodcache GOCACHE=$(CURDIR)/.gocache GOTOOLCHAIN=local GOFLAGS=-mod=vendor $(GOROOT)/bin/go build -ldflags="-s -w -X main.version=$(DEB_VERSION)" -o fastdedup .

override_dh_auto_install:
	install -D -m 0755 fastdedup debian/fastdedup/usr/bin/fastdedup

override_dh_auto_test:
	# skip tests for PPA build

override_dh_dwz:
	# skip dwz for Go binaries

override_dh_strip:
	# skip strip for Go binaries
RULES
    chmod +x debian/rules
  else
    # Restore original debian files for series with system Go
    cp debian/control.orig debian/control
    cp debian/rules.orig debian/rules
    rm -rf _go
  fi

  # Update debian/changelog for this series
  cat > debian/changelog <<CHLOG
${NAME} (${PPA_VERSION}) ${series}; urgency=low

  * Release v${VERSION}

 -- ${MAINTAINER}  $(date -R)
CHLOG

  # Clean previous artifacts for this version
  rm -f ../${NAME}_${PPA_VERSION}*

  printf "  %-12s build..." "$series"
  debuild -S -k"${GPG_KEY}" 2>/dev/null
  echo " upload..."
  dput --force "${PPA_TARGET}" "../${NAME}_${PPA_VERSION}_source.changes" 2>/dev/null
  echo "  ${series} done"
done

# Restore original debian files and clean up
cp debian/control.orig debian/control
cp debian/rules.orig debian/rules
rm -f debian/control.orig debian/rules.orig
rm -rf _go

# Restore changelog to first non-bundled series (or first series)
FIRST_SERIES="${PPA_SERIES[0]}"
cat > debian/changelog <<CHLOG
${NAME} (${VERSION}~ppa${PPA_REV}~${FIRST_SERIES}) ${FIRST_SERIES}; urgency=low

  * Release v${VERSION}

 -- ${MAINTAINER}  $(date -R)
CHLOG

# Clean up vendor dir (it's gitignored)
rm -rf vendor/

# ── Summary ─────────────────────────────────────────────────────────
echo ""
echo "==> Release ${VERSION} ready in ${RELEASE_DIR}/"
echo "    $(ls -1 "$RELEASE_DIR" | wc -l) files"
echo ""
echo "To publish on GitHub:"
echo "  git push origin v${VERSION}"
echo "  gh release create v${VERSION} ${RELEASE_DIR}/* --title 'v${VERSION}' --notes-file ${RELEASE_DIR}/description.md"
echo ""
echo "PPA uploads submitted for: ${PPA_SERIES[*]}"
echo "  Monitor builds at: https://launchpad.net/~phntm/+archive/ubuntu/ppa"

#!/usr/bin/env bash
set -euo pipefail

VERSION="${1:?Usage: $0 <version>}"
RELEASE_DIR="release/${VERSION}"
NAME="fastdedup"
DESC="Fast file deduplication using reflinks (btrfs, XFS, ZFS)"
ARCHES=(amd64 arm64 386 arm riscv64 ppc64le s390x mips64le)
MAINTAINER="PHANTOm <phantom@kix.co.il>"
GPG_KEY="B2BE8C2EBFB7AAC6572E933C779CD5498B743A1B"
PPA_SERIES=(noble questing)
PPA_TARGET="phntm-ppa"

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
  GOOS=linux GOARCH="$arch" go build -ldflags="-s -w" \
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

PPA_REV=1
for series in "${PPA_SERIES[@]}"; do
  PPA_VERSION="${VERSION}~ppa${PPA_REV}~${series}"

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

# Restore changelog to first series
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

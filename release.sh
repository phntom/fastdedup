#!/usr/bin/env bash
set -euo pipefail

VERSION="${1:?Usage: $0 <version>}"
RELEASE_DIR="release/${VERSION}"
NAME="fastdedup"
DESC="Fast file deduplication using reflinks (btrfs, XFS, ZFS)"
ARCHES=(amd64 arm64 386 arm riscv64 ppc64le s390x mips64le)
MAINTAINER="PHANTOm <phantom@kix.co.il>"
GPG_KEY="B2BE8C2EBFB7AAC6572E933C779CD5498B743A1B"
PPA_SERIES=(jammy noble resolute)
PPA_TARGET="phntm-ppa"

# All three PPA target series build with the system golang-1.26-go via the
# source package's Build-Depends. End-of-life Ubuntu releases are no longer
# targeted — their users install from the GitHub release artifacts.

# Ensure fpm is available
if ! command -v fpm &>/dev/null; then
  FPM_PATH="$HOME/.local/share/gem/ruby/$(ruby -e 'puts RUBY_VERSION.sub(/\.\d+$/,".0")')/bin/fpm"
  if [[ -x "$FPM_PATH" ]]; then
    _fpm_bin_dir="$(dirname "$FPM_PATH")"
    export PATH="${_fpm_bin_dir}:${PATH}"
  else
    echo "ERROR: fpm not found. Install with: gem install --user-install fpm"
    exit 1
  fi
fi

mkdir -p "$RELEASE_DIR"

# ── Run tests ──────────────────────────────────────────────────────
echo "==> Running unit tests..."
if ! go test ./...; then
  echo "ERROR: unit tests failed — aborting release"
  exit 1
fi
echo "    tests passed"

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
  mkdir -p "${STAGING}/usr/bin" "${STAGING}/etc/default" "${STAGING}/etc/logrotate.d"
  cp "${RELEASE_DIR}/${NAME}-linux-${arch}" "${STAGING}/usr/bin/${NAME}"
  cp debian/fastdedup.default "${STAGING}/etc/default/${NAME}"
  cp debian/fastdedup.logrotate "${STAGING}/etc/logrotate.d/${NAME}"

  FPM_COMMON=(
    -s dir -n "$NAME" -v "$VERSION"
    --description "$DESC"
    --config-files "etc/default/${NAME}"
    --config-files "etc/logrotate.d/${NAME}"
    -C "$STAGING"
  )

  FPM_FILES=("usr/bin/${NAME}" "etc/default/${NAME}" "etc/logrotate.d/${NAME}")

  printf "  %-10s deb " "linux/$arch"
  fpm "${FPM_COMMON[@]}" -t deb -a "$DEB" -p "${RELEASE_DIR}/" "${FPM_FILES[@]}" 2>/dev/null
  printf "rpm "
  fpm "${FPM_COMMON[@]}" -t rpm -a "$RPM" -p "${RELEASE_DIR}/" "${FPM_FILES[@]}" 2>/dev/null
  printf "apk "
  fpm "${FPM_COMMON[@]}" -t apk -a "$APK" -p "${RELEASE_DIR}/" "${FPM_FILES[@]}" 2>/dev/null
  printf "sh "
  fpm "${FPM_COMMON[@]}" -t sh  -a "$DEB" -p "${RELEASE_DIR}/${NAME}-${VERSION}-linux-${arch}.sh" "${FPM_FILES[@]}" 2>/dev/null
  echo ""

  rm -rf "$STAGING"
done

# ── Cron packages (architecture-independent) ─────────────────────────
echo "==> Creating cron packages..."
for variant in daily weekly; do
  STAGING=$(mktemp -d)
  mkdir -p "${STAGING}/etc/cron.${variant}"
  cp debian/fastdedup-cron "${STAGING}/etc/cron.${variant}/${NAME}"
  chmod +x "${STAGING}/etc/cron.${variant}/${NAME}"

  printf "  %-10s deb " "${NAME}-${variant}"
  fpm -s dir -n "${NAME}-${variant}" -v "$VERSION" \
    --description "${variant^} automatic file deduplication using reflinks" \
    --depends "$NAME" \
    -a all -C "$STAGING" -t deb -p "${RELEASE_DIR}/" \
    etc/cron.${variant}/${NAME} 2>/dev/null
  echo "ok"

  rm -rf "$STAGING"
done

# ── Ubuntu PPA ──────────────────────────────────────────────────────
echo ""
echo "==> Building Ubuntu PPA source packages..."

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
PPA_CHANGES_FILES=()
for series in "${PPA_SERIES[@]}"; do
  PPA_VERSION="${VERSION}~ppa${PPA_REV}~${series}"

  # Update debian/changelog for this series
  cat > debian/changelog <<CHLOG
${NAME} (${PPA_VERSION}) ${series}; urgency=low

  * Release v${VERSION}

 -- ${MAINTAINER}  $(date -R)
CHLOG

  # Clean previous artifacts for this version
  rm -f "../${NAME}_${PPA_VERSION}"*

  printf "  %-12s building..." "$series"
  if ! debuild -S -k"${GPG_KEY}" 2>/dev/null; then
    echo " FAILED"
    echo "ERROR: PPA source build failed for ${series} — aborting release"
    exit 1
  fi
  echo " ok"
  PPA_CHANGES_FILES+=("../${NAME}_${PPA_VERSION}_source.changes")
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

# ── All builds succeeded — now tag, push, and upload ───────────────
echo ""
echo "==> All builds successful. Tagging and uploading..."

# Tag
if git rev-parse "v${VERSION}" &>/dev/null; then
  echo "    Tag v${VERSION} already exists, skipping"
else
  echo "    Tagging v${VERSION}"
  git tag -a "v${VERSION}" -m "Release v${VERSION}"
fi

# Push tag
echo "    Pushing tag..."
git push origin "v${VERSION}"

# Upload PPA packages
echo "    Uploading PPA packages..."
for changes_file in "${PPA_CHANGES_FILES[@]}"; do
  series_name=$(basename "$changes_file" | sed 's/.*~\(.*\)_source.changes/\1/')
  printf "      %-12s " "$series_name"
  if dput --force "${PPA_TARGET}" "$changes_file" 2>/dev/null; then
    echo "ok"
  else
    echo "FAILED (non-fatal)"
  fi
done

# ── Summary ─────────────────────────────────────────────────────────
echo ""
echo "==> Release ${VERSION} ready in ${RELEASE_DIR}/"
echo "    $(find "$RELEASE_DIR" -mindepth 1 -maxdepth 1 | wc -l) files"
echo ""
echo "To publish on GitHub:"
echo "  gh release create v${VERSION} ${RELEASE_DIR}/* --title 'v${VERSION}' --notes '## v${VERSION}'"
echo ""
echo "PPA uploads submitted for: ${PPA_SERIES[*]}"
echo "  Monitor builds at: https://launchpad.net/~phntm/+archive/ubuntu/ppa"

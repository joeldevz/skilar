#!/usr/bin/env bash
set -euo pipefail

# ============================================================================
# skynex — Install Script
# One command to install AI agent skills for OpenCode and Claude Code.
#
# Usage:
#   Download this script from a tagged release and verify its detached signature
#   before running it. Never pipe an unverified network response to a shell.
#
# Options:
#   --method brew|binary      Force install method (default: binary)
#   --dir PATH                Custom install directory
#   -h, --help                Show help
# ============================================================================

GITHUB_OWNER="joeldevz"
GITHUB_REPO="skynex"
BINARY_NAME="skynex"
BREW_TAP="joeldevz/tap"
MAX_COMPRESSED_BYTES=$((100 * 1024 * 1024))
MAX_EXTRACTED_BYTES=$((250 * 1024 * 1024))

release_base_url() {
  printf '%s' "https://github.com/${GITHUB_OWNER}/${GITHUB_REPO}/releases/download"
}

limit_value() {
  local name="$1" default="$2" value
  value="${!name:-$default}"
  [[ "$value" =~ ^[0-9]+$ ]] || fatal "${name} must be an integer"
  printf '%s' "$value"
}

# ============================================================================
# Colors (only when TTY)
# ============================================================================

setup_colors() {
  if [ -t 1 ] && [ "${TERM:-}" != "dumb" ]; then
    RED='\033[0;31m'
    GREEN='\033[0;32m'
    YELLOW='\033[1;33m'
    BLUE='\033[0;34m'
    CYAN='\033[0;36m'
    BOLD='\033[1m'
    DIM='\033[2m'
    NC='\033[0m'
  else
    RED='' GREEN='' YELLOW='' BLUE='' CYAN='' BOLD='' DIM='' NC=''
  fi
}

# ============================================================================
# Logging
# ============================================================================

safe_display() { printf '%s' "$1" | LC_ALL=C tr -cd '[:print:]'; }
info()    { printf '%b%s%b\n' "$BLUE" "[info] $*" "$NC"; }
success() { printf '%b%s%b\n' "$GREEN" "[ok] $*" "$NC"; }
warn()    { printf '%b%s%b\n' "$YELLOW" "[warn] $*" "$NC"; }
error()   { printf '%b%s%b\n' "$RED" "[error] $*" "$NC" >&2; }
fatal()   { error "$@"; exit 1; }
step()    { printf '\n%b%s%b\n' "$CYAN" "${BOLD}==>${NC} ${BOLD}$*${NC}" "$NC"; }

# ============================================================================
# Help
# ============================================================================

show_help() {
  cat <<EOF
skynex installer

USAGE:
  Verify this local file and its detached signature from a tagged release,
  then run: ./install.sh [OPTIONS]

OPTIONS:
  --method METHOD   Install method: brew or binary (default: binary)
  --dir PATH        Custom install directory (default: ~/.local/bin)
  -h, --help        Show this help

EXAMPLES:
  ./install.sh                     # Install a signed release binary
  ./install.sh --method brew       # Force Homebrew
  ./install.sh --dir ~/bin         # Custom install dir
EOF
}

# ============================================================================
# Banner
# ============================================================================

print_banner() {
  echo ""
  printf '%b\n' "${CYAN}${BOLD}"
  echo "   ____           _                    ____  _    _ _ _     "
  echo "  / ___| ___  ___| | ___ ___  ___     / ___|| | _(_) | |___ "
  echo " | |   / _ \/ __| |/ / '__\ \/ /     \\___ \\| |/ / | | / __|"
  echo " | |__| (_) \\__ \\   <| |   >  <       ___) |   <| | | \\__ \\"
  echo "  \\____\\___/|___/_|\\_\\_|  /_/\\_\\     |____/|_|\\_\\_|_|_|___/"
  printf '%b\n' "${NC}"
  printf '%b\n' " ${DIM}AI agent skills installer for OpenCode and Claude Code${NC}"
  echo ""
}

# ============================================================================
# Platform detection
# ============================================================================

detect_platform() {
  OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
  case "$OS" in
    linux)  OS="linux" ;;
    darwin) OS="darwin" ;;
    *)      fatal "Unsupported OS: $OS. Use the Windows PowerShell installer instead." ;;
  esac

  ARCH="$(uname -m)"
  case "$ARCH" in
    x86_64|amd64)  ARCH="amd64" ;;
    arm64|aarch64) ARCH="arm64" ;;
    *)             fatal "Unsupported architecture: $ARCH" ;;
  esac

  success "Platform: ${OS}/${ARCH}"
}

# ============================================================================
# Prerequisites
# ============================================================================

check_prerequisites() {
  step "Checking prerequisites"

  local missing=()
  if ! command -v curl &>/dev/null; then
    missing+=("curl")
  fi
  if [ ${#missing[@]} -gt 0 ]; then
    fatal "Missing required tools: ${missing[*]}. Please install them and try again."
  fi

  success "curl is available"
}

# ============================================================================
# Install method detection
# ============================================================================

detect_install_method() {
  if [ -n "${FORCE_METHOD:-}" ]; then
    case "$FORCE_METHOD" in
      brew|binary) INSTALL_METHOD="$FORCE_METHOD" ;;
      *) fatal "Unknown install method: $FORCE_METHOD. Use: brew or binary" ;;
    esac
    info "Using forced method: $INSTALL_METHOD"
    return
  fi

  step "Detecting best install method"

  # Signed release binaries are the default. Homebrew is an explicitly
  # delegated trust boundary and must be selected explicitly.
  INSTALL_METHOD="binary"
  info "Will download and verify a signed pre-built binary from GitHub Releases"
}

# ============================================================================
# Install via Homebrew
# ============================================================================

install_brew() {
  step "Installing via Homebrew"
  warn "Homebrew is an explicitly delegated trust path; release signatures are not verified by this method."

  info "Refreshing ${BREW_TAP}..."
  brew untap "$BREW_TAP" 2>/dev/null || true
  if ! brew tap "$BREW_TAP"; then
    fatal "Failed to tap $BREW_TAP"
  fi

  if brew list "$BINARY_NAME" &>/dev/null; then
    info "Already installed, upgrading ${BINARY_NAME}..."
    if brew upgrade "$BINARY_NAME" 2>/dev/null; then
      success "Upgraded ${BINARY_NAME} via Homebrew"
    else
      success "${BINARY_NAME} is already at the latest version"
    fi
  else
    info "Installing ${BINARY_NAME}..."
    if brew install "$BINARY_NAME"; then
      success "Installed ${BINARY_NAME} via Homebrew"
    else
      fatal "Failed to install ${BINARY_NAME} via Homebrew"
    fi
  fi
}

# ============================================================================
# ============================================================================
# Install via binary download
# ============================================================================

get_latest_version() {
  local url="https://api.github.com/repos/${GITHUB_OWNER}/${GITHUB_REPO}/releases/latest"
  info "Fetching latest release from GitHub..."

  local response
  response="$(curl --connect-timeout 10 --max-time 60 -sL -w "\n%{http_code}" "$url")" || fatal "Failed to fetch latest release"

  local http_code body
  http_code="$(echo "$response" | tail -n1)"
  body="$(echo "$response" | sed '$d')"

  if [ "$http_code" != "200" ]; then
    fatal "GitHub API returned HTTP $http_code. Rate limited? Try again later or use --method brew"
  fi

  LATEST_VERSION="$(echo "$body" | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -1)"

  if [ -z "$LATEST_VERSION" ]; then
    fatal "Could not determine latest version from GitHub API response"
  fi

  VERSION_NUMBER="${LATEST_VERSION#v}"
  [[ "$LATEST_VERSION" =~ ^v?[0-9A-Za-z][0-9A-Za-z._-]{0,127}$ ]] || fatal "Release tag contains unsafe characters"
  success "Latest version: $(safe_display "$LATEST_VERSION")"
}

validate_archive() {
  local archive="$1"
  local listing member mode count=0
  listing="$(tar -tzf "$archive")" || fatal "Could not list archive; refusing extraction"
  while IFS= read -r member; do
    [ -n "$member" ] || continue
    case "$member" in
      /*|*'../'*|../*|*/..|..|*'/'..'/'*) fatal "Unsafe archive member: $(safe_display "$member")" ;;
    esac
    [ "$member" = "$BINARY_NAME" ] || fatal "Unexpected archive member: $(safe_display "$member")"
    count=$((count + 1))
  done <<< "$listing"
  [ "$count" -eq 1 ] || fatal "Archive must contain exactly one member: $BINARY_NAME"

  while read -r mode _; do
    case "$mode" in
      -?????????) ;;
      *) fatal "Archive contains a non-regular or unsafe member" ;;
    esac
  done < <(tar -tvzf "$archive")
}

link_count() {
  if stat -c '%h' "$1" >/dev/null 2>&1; then stat -c '%h' "$1"; else stat -f '%l' "$1"; fi
}

install_binary() {
  step "Installing pre-built binary"

  get_latest_version

  local archive_name="${BINARY_NAME}_${VERSION_NUMBER}_${OS}_${ARCH}.tar.gz"
  local extracted_size
  local download_url="$(release_base_url)/${LATEST_VERSION}/${archive_name}"
  local checksums_url="$(release_base_url)/${LATEST_VERSION}/checksums.txt"
  local signature_url="${checksums_url}.sig"

  local tmpdir
  tmpdir="$(mktemp -d)"
  trap '[ -n "${tmpdir:-}" ] && rm -rf "$tmpdir"' EXIT

  info "Downloading $(safe_display "${archive_name}")..."
  if ! curl --connect-timeout 10 --max-time 120 -sfL -o "${tmpdir}/${archive_name}" "$download_url"; then
    fatal "Failed to download ${download_url}"
  fi

  local file_size
  file_size="$(wc -c < "${tmpdir}/${archive_name}" | tr -d '[:space:]')"
  if [ "$file_size" -lt 1000 ]; then
    fatal "Downloaded file is suspiciously small (${file_size} bytes). Archive may not exist for this platform."
  fi
  [ "$file_size" -le "$(limit_value SKYNEX_MAX_COMPRESSED_BYTES "$MAX_COMPRESSED_BYTES")" ] || fatal "Downloaded archive exceeds compressed size limit"
  success "Downloaded ${archive_name} (${file_size} bytes)"

  info "Verifying checksum..."
  curl --connect-timeout 10 --max-time 60 -sfL -o "${tmpdir}/checksums.txt" "$checksums_url" || fatal "Could not download checksums.txt; refusing unverified archive"
  curl --connect-timeout 10 --max-time 60 -sfL -o "${tmpdir}/checksums.txt.sig" "$signature_url" || fatal "Could not download checksums.txt.sig; refusing unverified archive"
  command -v ssh-keygen >/dev/null 2>&1 || fatal "ssh-keygen is required to verify release authenticity"
  cat > "${tmpdir}/allowed_signers" <<'EOF'
skynex-release ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFyDcCsQ5k4P8zC/qrmMlFi5nfV02DhT+ADQiqX65ynf skynex release signing
EOF
  ssh-keygen -Y verify -f "${tmpdir}/allowed_signers" -I skynex-release -n file -s "${tmpdir}/checksums.txt.sig" < "${tmpdir}/checksums.txt" >/dev/null 2>&1 || fatal "Invalid checksums.txt signature"
  success "Release signature verified"
  local expected_checksum
  expected_checksum="$(awk -v file="$archive_name" '$2 == file || $2 == "*" file { print tolower($1) }' "${tmpdir}/checksums.txt")"
  [ "$(printf '%s\n' "$expected_checksum" | wc -l | tr -d ' ')" = 1 ] || fatal "checksums.txt must contain exactly one entry for ${archive_name}"
  [[ "$expected_checksum" =~ ^[0-9a-f]{64}$ ]] || fatal "Malformed checksum for ${archive_name}"
  local actual_checksum
  if command -v sha256sum &>/dev/null; then
    actual_checksum="$(sha256sum "${tmpdir}/${archive_name}" | awk '{print tolower($1)}')"
  elif command -v shasum &>/dev/null; then
    actual_checksum="$(shasum -a 256 "${tmpdir}/${archive_name}" | awk '{print tolower($1)}')"
  else
    fatal "No SHA-256 checksum utility available"
  fi
  [ "$actual_checksum" = "$expected_checksum" ] || fatal "Checksum mismatch! Expected ${expected_checksum}, got ${actual_checksum}"
  success "Checksum verified"

  info "Validating archive members..."
  validate_archive "${tmpdir}/${archive_name}"
  actual_checksum="$(sha256sum "${tmpdir}/${archive_name}" 2>/dev/null | awk '{print tolower($1)}' || shasum -a 256 "${tmpdir}/${archive_name}" | awk '{print tolower($1)}')"
  [ "$actual_checksum" = "$expected_checksum" ] || fatal "Archive changed after checksum verification"

  info "Extracting ${BINARY_NAME}..."
  if ! tar -xOzf "${tmpdir}/${archive_name}" -- "$BINARY_NAME" | head -c "$((MAX_EXTRACTED_BYTES + 1))" > "${tmpdir}/${BINARY_NAME}"; then
    fatal "Failed to extract archive"
  fi

  if [ ! -f "${tmpdir}/${BINARY_NAME}" ]; then
    fatal "Binary '${BINARY_NAME}' not found in archive"
  fi
  chmod 700 "${tmpdir}/${BINARY_NAME}" || fatal "Extracted binary is not executable"
  extracted_size="$(wc -c < "${tmpdir}/${BINARY_NAME}" | tr -d '[:space:]')"
  [ "$extracted_size" -gt 0 ] && [ "$extracted_size" -le "$(limit_value SKYNEX_MAX_EXTRACTED_BYTES "$MAX_EXTRACTED_BYTES")" ] || fatal "Extracted binary exceeds size limit"

  # Determine install directory
  local install_dir="${INSTALL_DIR:-}"
  if [ -z "$install_dir" ]; then
    if [ -d "/usr/local/bin" ] && [ -w "/usr/local/bin" ]; then
      install_dir="/usr/local/bin"
    elif [ "$(id -u)" = "0" ]; then
      install_dir="/usr/local/bin"
    else
      install_dir="${HOME}/.local/bin"
    fi
  fi

  if [ -e "$install_dir" ] || [ -L "$install_dir" ]; then
    [ ! -L "$install_dir" ] || fatal "Refusing symlink install directory"
    [ -d "$install_dir" ] || fatal "Install path is not a directory"
  else
    mkdir -p "$install_dir" || true
  fi
  [ -d "$install_dir" ] && [ ! -L "$install_dir" ] || fatal "Install directory is not safe"

  info "Installing to $(safe_display "${install_dir}/${BINARY_NAME}")..."
  if "${tmpdir}/${BINARY_NAME}" internal-install-binary "${tmpdir}/${BINARY_NAME}" "${install_dir}/${BINARY_NAME}" 2>/dev/null; then
    :
  elif command -v sudo &>/dev/null; then
    warn "Permission denied. Trying with sudo..."
    sudo "${tmpdir}/${BINARY_NAME}" internal-install-binary "${tmpdir}/${BINARY_NAME}" "${install_dir}/${BINARY_NAME}" \
      || fatal "Verified binary could not perform privileged installation"
  else
    fatal "Cannot write to ${install_dir}. Run with sudo or use --dir to specify a writable directory."
  fi

  success "Installed ${BINARY_NAME} to ${install_dir}/${BINARY_NAME}"

  if [[ ":$PATH:" != *":${install_dir}:"* ]]; then
    warn "${install_dir} is not in your PATH"
    echo ""
    warn "Add this to your shell profile (~/.bashrc, ~/.zshrc, etc.):"
    printf '%b%s%b\n' "$DIM" "  export PATH=\"\$PATH:$(safe_display "${install_dir}")\"" "$NC"
    echo ""
  fi
}

# ============================================================================
# Verify installation
# ============================================================================

verify_installation() {
  step "Verifying installation"

  hash -r 2>/dev/null || true

  if command -v "$BINARY_NAME" &>/dev/null; then
    local version_output
    version_output="$("$BINARY_NAME" --help 2>&1 | head -1 || true)"
    success "${BINARY_NAME} is installed and ready"
    return 0
  fi

  local locations=(
    "/usr/local/bin/${BINARY_NAME}"
    "${HOME}/.local/bin/${BINARY_NAME}"
  )

  for loc in "${locations[@]}"; do
    if [ -x "$loc" ]; then
      success "Found ${BINARY_NAME} at ${loc}"
      warn "Binary location is not in your PATH. Add it to use '${BINARY_NAME}' directly."
      return 0
    fi
  done

  warn "Could not verify installation. You may need to restart your shell."
  return 0
}

# ============================================================================
# Next steps
# ============================================================================

print_next_steps() {
  echo ""
  printf '%b\n' "${GREEN}${BOLD}Installation complete!${NC}"
  echo ""
  printf '%b\n' "${BOLD}Next steps:${NC}"
  printf '%b\n' "  ${CYAN}1.${NC} Run ${BOLD}${BINARY_NAME}${NC} to start the interactive installer"
  printf '%b\n' "  ${CYAN}2.${NC} Select your AI tool(s): Claude Code, OpenCode"
  printf '%b\n' "  ${CYAN}3.${NC} Follow the prompts"
  echo ""
  printf '%b\n' "${DIM}For help: ${BINARY_NAME} --help${NC}"
  printf '%b\n' "${DIM}Docs: https://github.com/${GITHUB_OWNER}/${GITHUB_REPO}${NC}"
  echo ""
}

# ============================================================================
# Main
# ============================================================================

main() {
  setup_colors

  FORCE_METHOD=""
  INSTALL_DIR=""

  while [ $# -gt 0 ]; do
    case "$1" in
      --method)
        [ $# -lt 2 ] && fatal "--method requires an argument"
        FORCE_METHOD="$2"; shift 2
        ;;
      --dir)
        [ $# -lt 2 ] && fatal "--dir requires an argument"
        INSTALL_DIR="$2"; shift 2
        ;;
      -h|--help)
        setup_colors
        show_help
        exit 0
        ;;
      *)
        fatal "Unknown option: $1. Use --help for usage."
        ;;
    esac
  done

  print_banner

  step "Detecting platform"
  detect_platform

  check_prerequisites
  detect_install_method

  case "$INSTALL_METHOD" in
    brew)   install_brew ;;
    binary) install_binary ;;
  esac

  verify_installation
  print_next_steps
}

main "$@"

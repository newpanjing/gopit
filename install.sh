#!/usr/bin/env bash
# GoPit 跨平台安装、升级与启动脚本。
set -euo pipefail

PROGRAM_NAME="pit"
DEFAULT_INSTALL_DIR="${HOME}/.local/bin"
COMMAND="install"
REPOSITORY="newpanjing/gopit"
INSTALL_DIR="${DEFAULT_INSTALL_DIR}"

usage() {
  cat <<'EOF'
Usage:
  install.sh
  install.sh upgrade

Downloads the latest executable from github.com/newpanjing/gopit.
EOF
}

fail() { printf 'Error: %s\n' "$*" >&2; exit 1; }

require_command() { command -v "$1" >/dev/null 2>&1 || fail "missing required command: $1"; }

detect_platform() {
  case "$(uname -s)" in
    Darwin) OS="darwin" ;;
    Linux) OS="linux" ;;
    MINGW*|MSYS*|CYGWIN*) OS="windows" ;;
    *) fail "unsupported operating system: $(uname -s)" ;;
  esac
  case "$(uname -m)" in
    x86_64|amd64) ARCH="amd64" ;;
    arm64|aarch64) ARCH="arm64" ;;
    *) fail "unsupported architecture: $(uname -m)" ;;
  esac
}

download_and_install() {
  require_command curl
  detect_repository
  detect_platform
  local binary="${PROGRAM_NAME}"
  if [[ "${OS}" == "windows" ]]; then binary="${PROGRAM_NAME}.exe"; fi
  local asset="${PROGRAM_NAME}_${OS}_${ARCH}"
  if [[ "${OS}" == "windows" ]]; then asset="${asset}.exe"; fi
  local url="https://github.com/${REPOSITORY}/releases/latest/download/${asset}"
  local temporary_dir
  temporary_dir="$(mktemp -d)"
  trap 'rm -rf "${temporary_dir}"' EXIT
  printf 'Downloading %s\n' "${url}"
  curl --fail --location --silent --show-error "${url}" -o "${temporary_dir}/${binary}"
  [[ -f "${temporary_dir}/${binary}" ]] || fail "release asset download failed"
  mkdir -p "${INSTALL_DIR}"
  cp "${temporary_dir}/${binary}" "${INSTALL_DIR}/${binary}"
  chmod 0755 "${INSTALL_DIR}/${binary}"
  printf 'Installed %s to %s\n' "${PROGRAM_NAME}" "${INSTALL_DIR}/${binary}"
  add_path
}

add_path() {
  local profile="${HOME}/.bashrc"
  if [[ "$(uname -s)" == "Darwin" ]]; then profile="${HOME}/.zshrc"; fi
  local marker="# GoPit PATH"
  if [[ ! -f "${profile}" ]] || ! grep -Fq "${marker}" "${profile}"; then
    printf '\n%s\nexport PATH="%s:$PATH"\n' "${marker}" "${INSTALL_DIR}" >> "${profile}"
  fi
  export PATH="${INSTALL_DIR}:${PATH}"
  printf 'PATH updated for this shell and %s\n' "${profile}"
}

if [[ "${1:-}" == "upgrade" ]]; then COMMAND="upgrade"; shift; fi
if [[ "${1:-}" == "-h" || "${1:-}" == "--help" ]]; then usage; exit 0; fi
[[ $# -eq 0 ]] || fail "usage: install.sh [upgrade]"

case "${COMMAND}" in
  install|upgrade) download_and_install ;;
esac

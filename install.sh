#!/usr/bin/env bash
# GoPit 跨平台安装、升级与启动脚本。
set -euo pipefail

PROGRAM_NAME="pit"
DEFAULT_INSTALL_DIR="${HOME}/.local/bin"
COMMAND="install"
REPOSITORY="${GOPIT_REPOSITORY:-}"
INSTALL_DIR="${GOPIT_INSTALL_DIR:-${DEFAULT_INSTALL_DIR}}"
VERSION="latest"

usage() {
  cat <<'EOF'
Usage:
  install.sh install [--repo owner/repo] [--dir path] [--version tag]
  install.sh upgrade [--repo owner/repo] [--dir path] [--version tag]
  install.sh run [--repo owner/repo] [--dir path] [pit arguments...]

The repository is resolved from --repo, GOPIT_REPOSITORY, or the current Git
remote. For a curl download, set GOPIT_REPOSITORY=owner/repo.
EOF
}

fail() { printf 'Error: %s\n' "$*" >&2; exit 1; }

require_command() { command -v "$1" >/dev/null 2>&1 || fail "missing required command: $1"; }

detect_repository() {
  if [[ -n "${REPOSITORY}" ]]; then return; fi
  if command -v git >/dev/null 2>&1; then
    local remote
    remote="$(git config --get remote.origin.url 2>/dev/null || true)"
    remote="${remote#git@github.com:}"
    remote="${remote#https://github.com/}"
    remote="${remote%.git}"
    if [[ "${remote}" == */* ]]; then REPOSITORY="${remote}"; fi
  fi
  [[ -n "${REPOSITORY}" ]] || fail "set --repo owner/repo or GOPIT_REPOSITORY"
}

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
  local release_path="latest/download"
  if [[ "${VERSION}" != "latest" ]]; then release_path="download/${VERSION}"; fi
  local url="https://github.com/${REPOSITORY}/releases/${release_path}/${asset}"
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
  case ":${PATH}:" in
    *":${INSTALL_DIR}:"*) ;;
    *) printf 'Add %s to PATH to invoke %s directly.\n' "${INSTALL_DIR}" "${PROGRAM_NAME}" ;;
  esac
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    install|upgrade|run) COMMAND="$1"; shift ;;
    --repo) REPOSITORY="${2:-}"; shift 2 ;;
    --dir) INSTALL_DIR="${2:-}"; shift 2 ;;
    --version) VERSION="${2:-}"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) break ;;
  esac
done

case "${COMMAND}" in
  install|upgrade) download_and_install ;;
  run)
    run_binary="${PROGRAM_NAME}"
    if [[ "$(uname -s)" == MINGW* || "$(uname -s)" == MSYS* || "$(uname -s)" == CYGWIN* ]]; then run_binary="${PROGRAM_NAME}.exe"; fi
    if [[ ! -x "${INSTALL_DIR}/${run_binary}" ]]; then download_and_install; fi
    exec "${INSTALL_DIR}/${run_binary}" "$@"
    ;;
esac

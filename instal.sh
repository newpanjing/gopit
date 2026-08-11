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
  instal.sh install [--repo owner/repo] [--dir path] [--version tag]
  instal.sh upgrade [--repo owner/repo] [--dir path] [--version tag]
  instal.sh run [--repo owner/repo] [--dir path] [tunnel arguments...]

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
  require_command tar
  detect_repository
  detect_platform
  local asset="${PROGRAM_NAME}_${OS}_${ARCH}.tar.gz"
  local release_path="latest/download"
  if [[ "${VERSION}" != "latest" ]]; then release_path="download/${VERSION}"; fi
  local url="https://github.com/${REPOSITORY}/releases/${release_path}/${asset}"
  local temporary_dir
  temporary_dir="$(mktemp -d)"
  trap 'rm -rf "${temporary_dir}"' EXIT
  printf 'Downloading %s\n' "${url}"
  curl --fail --location --silent --show-error "${url}" -o "${temporary_dir}/${asset}"
  tar -xzf "${temporary_dir}/${asset}" -C "${temporary_dir}"
  [[ -f "${temporary_dir}/${PROGRAM_NAME}_${OS}_${ARCH}/${PROGRAM_NAME}" ]] || fail "release archive has an unexpected layout"
  mkdir -p "${INSTALL_DIR}"
  install -m 0755 "${temporary_dir}/${PROGRAM_NAME}_${OS}_${ARCH}/${PROGRAM_NAME}" "${INSTALL_DIR}/${PROGRAM_NAME}"
  printf 'Installed %s to %s\n' "${PROGRAM_NAME}" "${INSTALL_DIR}/${PROGRAM_NAME}"
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
    if [[ ! -x "${INSTALL_DIR}/${PROGRAM_NAME}" ]]; then download_and_install; fi
    exec "${INSTALL_DIR}/${PROGRAM_NAME}" "$@"
    ;;
esac

#!/usr/bin/env bash

set -Eeuo pipefail

APP_NAME="Rafael Panel"
APP_DIR="/usr/local/rafael"
REPOSITORY="${RAFAEL_GITHUB_REPOSITORY:-Rafael7277/rafael-panel}"
RELEASE_ASSET="rafael-linux-amd64.tar.gz"
VERSION="${1:-latest}"
TEMP_DIR=""

fail() {
    printf 'Error: %s\n' "$*" >&2
    exit 1
}

cleanup() {
    if [[ -n "${TEMP_DIR}" && -d "${TEMP_DIR}" ]]; then
        rm -rf -- "${TEMP_DIR}"
    fi
}

trap cleanup EXIT

require_root() {
    if [[ "${EUID}" -ne 0 ]]; then
        fail "Run the installer as root (for example: sudo bash install.sh)."
    fi
}

validate_repository() {
    [[ "${REPOSITORY}" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] || \
        fail "Invalid GitHub repository path: ${REPOSITORY}"
}

resolve_release_url() {
    local release_base="https://github.com/${REPOSITORY}/releases"
    if [[ "${VERSION}" == "latest" ]]; then
        printf '%s/latest/download/%s\n' "${release_base}" "${RELEASE_ASSET}"
        return
    fi
    [[ "${VERSION}" =~ ^v[0-9]+([.][0-9]+)*([.-][A-Za-z0-9._-]+)?$ ]] || \
        fail "Use 'latest' or a release tag such as v63."
    printf '%s/download/%s/%s\n' "${release_base}" "${VERSION}" "${RELEASE_ASSET}"
}

verify_archive_layout() {
    local archive="$1"
    local listing verbose_listing
    listing="$(tar -tzf "${archive}")" || fail "The downloaded release is not a valid tar.gz archive."
    if printf '%s\n' "${listing}" | awk '
        /^\// { invalid = 1 }
        /(^|\/)\.\.($|\/)/ { invalid = 1 }
        !/^rafael(\/|$)/ { invalid = 1 }
        END { exit invalid ? 0 : 1 }
    '; then
        fail "The release archive contains an unsafe path."
    fi
    verbose_listing="$(tar -tvzf "${archive}")" || fail "Could not inspect the release archive entries."
    if printf '%s\n' "${verbose_listing}" | awk 'substr($0, 1, 1) != "-" && substr($0, 1, 1) != "d" { invalid = 1 } END { exit invalid ? 0 : 1 }'; then
        fail "The release archive contains a non-regular entry; refusing to extract it as root."
    fi
    printf '%s\n' "${listing}" | grep -qx 'rafael/rafael-panel' || \
        fail "The release archive does not contain Rafael Panel."
    printf '%s\n' "${listing}" | grep -qx 'rafael/install-local.sh' || \
        fail "The release archive does not contain its first-install setup."
}

main() {
    local url temp_dir archive checksum_file install_root
    require_root
    validate_repository

    [[ "$(uname -s)" == "Linux" ]] || fail "The release installer currently supports Linux only."
    case "$(uname -m)" in
        x86_64 | amd64) ;;
        *) fail "The current Rafael Panel release is Linux amd64; found $(uname -m)." ;;
    esac
    command -v curl >/dev/null 2>&1 || fail "curl is required. Install curl and run this command again."
    command -v sha256sum >/dev/null 2>&1 || fail "sha256sum is required."
    command -v tar >/dev/null 2>&1 || fail "tar is required."

    url="$(resolve_release_url)"
    temp_dir="$(mktemp -d -t rafael-install.XXXXXX)"
    TEMP_DIR="${temp_dir}"
    archive="${temp_dir}/${RELEASE_ASSET}"
    checksum_file="${archive}.sha256"

    printf '%s\n' "Downloading ${APP_NAME} (${VERSION}) from ${REPOSITORY}..."
    curl --fail --location --silent --show-error --retry 3 "${url}" -o "${archive}" || \
        fail "Could not download the Rafael Panel release. Confirm the repository and release exist."
    curl --fail --location --silent --show-error --retry 3 "${url}.sha256" -o "${checksum_file}" || \
        fail "The release SHA-256 file is missing; refusing to install an unverified archive."

    local expected_hash actual_hash
    expected_hash="$(awk 'NR == 1 { print $1 }' "${checksum_file}")"
    [[ "${expected_hash}" =~ ^[A-Fa-f0-9]{64}$ ]] || fail "The release SHA-256 file is malformed."
    actual_hash="$(sha256sum "${archive}" | awk '{ print $1 }')"
    [[ "${actual_hash,,}" == "${expected_hash,,}" ]] || fail "The release archive SHA-256 does not match."
    verify_archive_layout "${archive}"

    if systemctl cat rafael >/dev/null 2>&1 || [[ -x /usr/bin/rafael ]]; then
        printf '%s\n' "An existing Rafael installation was found; handing the verified archive to its updater."
        /usr/bin/rafael update "${archive}" || fail "Rafael's updater could not apply the release. Check 'rafael log' and try again."
        return
    fi

    if [[ -e "${APP_DIR}/rafael-panel" || -e "${APP_DIR}/current" ]]; then
        fail "Rafael files exist at ${APP_DIR}, but its managed service/updater is missing. Refusing to overwrite them."
    fi

    install_root="${temp_dir}/unpacked"
    mkdir -p "${install_root}"
    tar --no-same-owner --no-same-permissions -xzf "${archive}" -C "${install_root}"
    [[ -x "${install_root}/rafael/rafael-panel" ]] || chmod 755 "${install_root}/rafael/rafael-panel"
    printf '\n%s\n' "Starting the guided first-install setup..."
    (cd "${install_root}/rafael" && /bin/bash ./install-local.sh)
}

main "$@"

#!/bin/bash

# =============================================================================
# Rafael Panel stable release manager.
#
# Lives OUTSIDE the release tree at /usr/local/rafael/rafael-updater.sh so a
# broken current release can never prevent update or rollback.
#
# Layout it manages:
#   /usr/local/rafael/
#       current -> releases/vNN          (atomic symlink activation)
#       releases/v37/ v39/ ...           (immutable release dirs)
#       rafael-updater.sh                (this file, stable layer)
#       rafael-launcher.sh               (/usr/bin/rafael source, stable layer)
#       persistent/                      (backups, preserved flat leftovers)
#
# Persistent state it must never touch destructively:
#   /etc/default/rafael                  (env; DSN preserved, masked in logs)
#   PostgreSQL rafael_panel              (external; pg_dump backup only)
#   /var/log/x-ui/                       (logs)
#   TLS certificates                     (detected, preserved IN PLACE,
#                                         never deleted or moved)
#
# Usage:
#   rafael-updater.sh preflight [archive]   read-only checks, no changes
#   rafael-updater.sh update [archive]      full safe deployment
#   rafael-updater.sh rollback [version]    symlink swap to a release
#   rafael-updater.sh status                list releases + active one
# =============================================================================

set -Eeuo pipefail

ROOT_DIR="/usr/local/rafael"
RELEASES_DIR="${ROOT_DIR}/releases"
CURRENT_LINK="${ROOT_DIR}/current"
ENV_FILE="/etc/default/rafael"
SERVICE_NAME="rafael"
UNIT_SRC_NAME="rafael.service.debian"
UNIT_DST="/etc/systemd/system/${SERVICE_NAME}.service"
CLI_ALIAS="/usr/bin/rafael"
LAUNCHER_SRC_NAME="rafael-launcher.sh"
LOCK_FILE="${ROOT_DIR}/.rafael-updater.lock"
BACKUP_ROOT="/var/backups/rafael"
UPDATER_NAME="rafael-updater.sh"

HEALTH_CHECK_SECONDS="${RAFAEL_HEALTH_CHECK_SECONDS:-45}"
HEALTH_CHECK_INTERVAL="${RAFAEL_HEALTH_CHECK_INTERVAL:-3}"
HEALTH_STABILITY_SECONDS="${RAFAEL_HEALTH_STABILITY_SECONDS:-30}"
KEEP_RELEASES="${RAFAEL_KEEP_RELEASES:-3}"

STAGE_DIR=""
RELEASE_MARKER=""
LAST_BACKUP_DIR=""

# -----------------------------------------------------------------------------
# Utilities
# -----------------------------------------------------------------------------

log() {
    local line
    line="$(date '+%Y-%m-%d %H:%M:%S') $*"
    LOG_LINES+=("${line}")
    echo "$*"
}

die() {
    log "UPDATE FAILED: $*"
    if [[ -n "${STAGE_DIR:-}" && -d "${STAGE_DIR}" ]]; then
        rm -rf "${STAGE_DIR}"
    fi
    exit 1
}

require_root() {
    if [[ "${EUID}" -ne 0 ]]; then
        echo "This command must run as root." >&2
        exit 1
    fi
}

mask_dsn() {
    local dsn="${1:-}"
    if [[ -z "${dsn}" ]]; then
        echo "(not set)"
        return
    fi
    printf '%s' "${dsn}" | sed -E 's/password=[^ ]*/password=***/'
}

# resolve_current prints the release name current points at (e.g. v39), or
# empty when the symlink is missing or is the flat-install boot shim.
resolve_current() {
    if [[ -L "${CURRENT_LINK}" ]]; then
        local target
        target="$(readlink "${CURRENT_LINK}")"
        # The unit's ExecStartPre creates a self-pointing link on flat
        # installs so the current-based paths resolve; it is a boot shim,
        # not a release pointer.
        [[ "${target}" == "." ]] && return 0
        basename "$(readlink -f "${CURRENT_LINK}")"
    elif [[ -d "${CURRENT_LINK}" ]]; then
        echo "(directory-not-symlink)"
    fi
}

read_env_value() {
    local key="$1"
    if [[ -f "${ENV_FILE}" ]]; then
        sed -n "s/^${key}=//p" "${ENV_FILE}" | tail -n 1
    fi
}

read_dsn_component() {
    local key="$1"
    local dsn
    dsn="$(read_env_value XUI_DB_DSN)"
    [[ -z "${dsn}" ]] && return 0
    printf '%s' "${dsn}" | tr -d '"' | sed -n "s/.*${key}=\([^ ]*\).*/\1/p"
}

# validate_path refuses unsafe values before they reach rm/mv operations.
validate_path_under_root() {
    local p="$1"
    [[ -n "${p}" && "${p}" == "${ROOT_DIR}"/* && "${p}" != "${ROOT_DIR}/" && "${p}" != *".."* ]] || {
        echo "Refusing unsafe path: '${p}'" >&2
        return 1
    }
}

# load_panel_env exports the panel env file (same semantics as systemd's
# EnvironmentFile): flat KEY=VALUE lines, values may be double-quoted.
load_panel_env() {
    if [[ -f "${ENV_FILE}" ]]; then
        set -a
        # shellcheck disable=SC1090
        source "${ENV_FILE}"
        set +a
    fi
}

# set_env_bin_folder atomically rewrites XUI_BIN_FOLDER in the env file to the
# given directory, preserving every other line. This MUST be called BEFORE the
# first restart that can occur after the flat bin directory is moved: systemd
# reads EnvironmentFile at unit start, and ExecStartPre (bootstrap.sh) only
# takes effect on the NEXT start — so an ExecStartPre correction is always one
# restart too late and the panel would boot with a stale bin path.
set_env_bin_folder() {
    local bin_dir="$1"
    local tmp
    tmp="$(mktemp "${ENV_FILE}.tmp.XXXXXX")" || {
        log "ENV FAIL: could not create temp file to update ${ENV_FILE}."
        return 1
    }
    {
        printf 'XUI_BIN_FOLDER=%s\n' "${bin_dir}"
        grep -vE '^XUI_BIN_FOLDER=' "${ENV_FILE}" 2>/dev/null || true
    } >"${tmp}"
    chmod --reference="${ENV_FILE}" "${tmp}" 2>/dev/null || chmod 600 "${tmp}"
    if ! mv -f "${tmp}" "${ENV_FILE}"; then
        rm -f "${tmp}"
        log "ENV FAIL: could not install updated ${ENV_FILE}."
        return 1
    fi
    log "ENV: XUI_BIN_FOLDER=${bin_dir} (written to ${ENV_FILE} before restart)."
    return 0
}

# -----------------------------------------------------------------------------
# Locking
# -----------------------------------------------------------------------------

acquire_lock() {
    mkdir -p "${ROOT_DIR}"
    exec 9>"${LOCK_FILE}"
    if ! flock -n 9; then
        echo "Another rafael-updater process is running (lock: ${LOCK_FILE})." >&2
        exit 1
    fi
}

# -----------------------------------------------------------------------------
# Archive validation
# -----------------------------------------------------------------------------

# is_valid_release_name: vNN (digits only) — guards every path built from a
# version string so no archive-controlled value can escape the tree.
is_valid_release_name() {
    [[ "${1:-}" =~ ^v[0-9]+$ ]]
}

# check_no_traversal: rejects absolute paths, .. traversal, and any entry not
# rooted under the expected top-level "rafael/" directory.
check_no_traversal() {
    local archive="$1"
    local entries bad
    entries="$(tar -tzf "${archive}")" || return 1
    bad="$(printf '%s\n' "${entries}" | grep -Ev '^(rafael/|rafael$)' || true)"
    if [[ -n "${bad}" ]]; then
        log "Archive contains entries outside 'rafael/':"
        printf '%s\n' "${bad}" | head -5 | sed 's/^/    /'
        return 1
    fi
    if printf '%s\n' "${entries}" | grep -q '\.\./'; then
        log "Archive contains '..' traversal entries."
        return 1
    fi
    return 0
}

# check_no_special_members rejects symlink, hardlink, device and FIFO members.
# Entry NAMES alone cannot make extraction safe: GNU tar resolves a symlink
# created by an earlier member when writing later members that traverse it, so
# a "rafael/x -> /etc" member followed by "rafael/x/evil" would write OUTSIDE
# the staging directory as root. The release format needs only regular files
# and directories, so anything else is rejected outright — the member itself,
# not just its target. GNU tar's verbose listing makes the member type the
# first field: '-' regular, 'd' directory, 'l' symlink, 'h' hardlink.
check_no_special_members() {
    local archive="$1"
    local listing bad
    # split listing and filtering so a tar failure cannot hide behind awk's
    # success (the command substitution would otherwise only fail with
    # pipefail, which this check must not depend on)
    if ! listing="$(tar -tvzf "${archive}" 2>/dev/null)"; then
        log "VALIDATION FAIL: could not inspect archive member types."
        return 1
    fi
    bad="$(printf '%s\n' "${listing}" | awk '$1 !~ /^[-d]/')"
    if [[ -n "${bad}" ]]; then
        log "Archive contains non-regular members (symlinks, hardlinks, devices and FIFOs are not allowed):"
        printf '%s\n' "${bad}" | head -5 | sed 's/^/    /'
        return 1
    fi
    return 0
}

# check_elf_x86_64 verifies the ELF header directly (dependency-free):
# magic \x7fELF, class 2 (64-bit), machine 0x3e (x86-64).
check_elf_x86_64() {
    local bin="$1"
    local magic machine
    magic="$(head -c 5 "${bin}" | od -An -tx1 | tr -d ' \n')"
    [[ "${magic}" == "7f454c4602" ]] || return 1
    machine="$(dd if="${bin}" bs=1 skip=18 count=2 2>/dev/null | od -An -tx1 | tr -d ' \n')"
    [[ "${machine}" == "3e00" ]] || return 1
    return 0
}

# is_executable_file: single place for the executability check.
is_executable_file() {
    [[ -x "$1" ]]
}

# smoke_run executes the staged panel binary's -v flag (version print; no
# servers, no DB writes) as an unprivileged user when possible. The staging
# directory is deliberately 0700 (mktemp default + explicit chmod), so the
# whole path down to the binary must be made traversable for the
# unprivileged user first — otherwise `su nobody` cannot even reach the
# binary and every real-host smoke check fails with permission denied.
# If unprivileged execution is unavailable (no nobody account, su restricted
# by PAM), fall back to a root run: systemd executes the panel as root, so
# a root -v is representative, and a binary that is broken fails both ways.
smoke_run() {
    local panel="$1" dir parent err
    dir="$(dirname "${panel}")"
    chmod 755 "${dir}" "${panel}" 2>/dev/null || true
    parent="$(dirname "${dir}")"
    # Open the staging directory (and only that — matched by name so nothing
    # else is ever chmodded) for the unprivileged check. The whole staging
    # tree is removed immediately after validation.
    case "${parent}" in
        */.rafael-stage-*) chmod 755 "${parent}" 2>/dev/null || true ;;
    esac
    if id nobody >/dev/null 2>&1; then
        if err="$(su -s /bin/sh nobody -c "\"${panel}\" -v" 2>&1)"; then
            return 0
        fi
        log "SMOKE: unprivileged -v run failed (${err}); falling back to a root run."
    fi
    "${panel}" -v >/dev/null 2>&1
}

# validate_archive runs every static check on the tar.gz. Never executes
# anything from the archive. Sets RELEASE_MARKER on success.
validate_archive() {
    local archive="$1"
    local manifest_path="${2:-}"

    [[ -f "${archive}" ]] || { log "VALIDATION FAIL: archive not found: ${archive}"; return 1; }
    [[ -r "${archive}" ]] || { log "VALIDATION FAIL: archive not readable: ${archive}"; return 1; }

    log "VALIDATION: checking archive structure..."
    check_no_traversal "${archive}" || return 1
    check_no_special_members "${archive}" || return 1

    # Required entries.
    local required=(
        "rafael/rafael-panel"
        "rafael/rafael.sh"
        "rafael/bootstrap.sh"
        "rafael/${UNIT_SRC_NAME}"
        "rafael/${UPDATER_NAME}"
        "rafael/${LAUNCHER_SRC_NAME}"
        "rafael/bin/xray-linux-amd64"
    )
    local entries
    entries="$(tar -tzf "${archive}")" || { log "VALIDATION FAIL: tar listing failed"; return 1; }
    local f
    for f in "${required[@]}"; do
        if ! printf '%s\n' "${entries}" | grep -qx "${f}"; then
            log "VALIDATION FAIL: archive missing required file: ${f}"
            return 1
        fi
    done

    # Build marker.
    local build_marker
    # Windows-created bundles can preserve CRLF in rafael.sh. Strip the CR
    # before matching the whole marker line so it cannot leak into the version.
    build_marker="$(tar -xOf "${archive}" rafael/rafael.sh 2>/dev/null | sed -n 's/\r$//; s/^CLI_BUILD_VERSION="\([^"]*\)"$/\1/p')"
    if ! is_valid_release_name "${build_marker}"; then
        log "VALIDATION FAIL: missing or ambiguous build marker (got: '${build_marker:-none}'; expected vNN)."
        return 1
    fi
    RELEASE_MARKER="${build_marker}"

    # Checksum manifest when supplied (archive.tar.gz.sha256 or SHA256SUMS).
    if [[ -z "${manifest_path}" ]]; then
        local cand
        for cand in "${archive}.sha256" "${archive%.tar.gz}.sha256"; do
            [[ -f "${cand}" ]] && { manifest_path="${cand}"; break; }
        done
    fi
    if [[ -z "${manifest_path}" ]]; then
        local m dir_cand
        for m in "SHA256SUMS" "sha256sums.txt" "SHA256SUMS.txt"; do
            dir_cand="$(dirname -- "${archive}")/${m}"
            [[ -f "${dir_cand}" ]] && { manifest_path="${dir_cand}"; break; }
        done
    fi
    if [[ -n "${manifest_path}" ]]; then
        log "VALIDATION: verifying SHA-256 manifest: ${manifest_path}"
        local archive_name expected actual
        archive_name="$(basename -- "${archive}")"
        expected="$(awk -v n="${archive_name}" '$2 ~ n {print $1; exit}' "${manifest_path}")"
        if [[ -z "${expected}" ]]; then
            expected="$(grep -E '^[0-9a-fA-F]{64}' "${manifest_path}" | head -n 1 | awk '{print $1}')"
        fi
        if [[ -z "${expected}" ]]; then
            log "VALIDATION FAIL: manifest exists but contains no usable SHA-256 entry."
            return 1
        fi
        actual="$(sha256sum "${archive}" | awk '{print $1}')"
        if [[ "${actual}" != "${expected}" ]]; then
            log "VALIDATION FAIL: SHA-256 mismatch."
            log "  expected: ${expected}"
            log "  actual:   ${actual}"
            return 1
        fi
        log "VALIDATION: SHA-256 verified."
    else
        log "VALIDATION: no SHA-256 manifest found for this archive (continuing without; supply a .sha256 sidecar to enforce)."
    fi

    return 0
}

# validate_staged_release checks the EXTRACTED tree: ELF arch, permissions,
# and an unprivileged -v smoke check of the binary.
validate_staged_release() {
    local staged_root="$1" # the rafael/ dir inside staging
    local panel="${staged_root}/rafael-panel"

    [[ -f "${panel}" ]] || { log "VALIDATION FAIL: staged release missing rafael-panel"; return 1; }
    is_executable_file "${panel}" || { log "VALIDATION FAIL: staged rafael-panel is not executable"; return 1; }

    if ! check_elf_x86_64 "${panel}"; then
        log "VALIDATION FAIL: rafael-panel is not an ELF 64-bit x86-64 executable."
        return 1
    fi

    # Smoke check: -v prints the version and exits. Never starts a server,
    # never touches the database. Run as an unprivileged user.
    if ! smoke_run "${panel}"; then
        log "VALIDATION FAIL: staged rafael-panel -v smoke check failed (did not execute cleanly)."
        return 1
    fi

    local xray_bin="${staged_root}/bin/xray-linux-amd64"
    [[ -f "${xray_bin}" ]] || { log "VALIDATION FAIL: staged release missing xray binary"; return 1; }
    is_executable_file "${xray_bin}" || { log "VALIDATION FAIL: staged xray binary is not executable"; return 1; }
    if ! check_elf_x86_64 "${xray_bin}"; then
        log "VALIDATION FAIL: xray binary is not ELF 64-bit x86-64."
        return 1
    fi

    # The unit a release ships must reference the stable current/ paths: the
    # migration installs this unit BEFORE the flat files are tidied, so a
    # flat-path unit would leave the service unbootable once tidy runs. (The
    # path is the fixed production path, not a variable — mirrors the unit
    # template's own static content.)
    if [[ -f "${staged_root}/${UNIT_SRC_NAME}" ]]; then
        if ! grep -q "^ExecStart=/usr/local/rafael/current/rafael-panel$" "${staged_root}/${UNIT_SRC_NAME}" \
            || ! grep -q "^WorkingDirectory=/usr/local/rafael/current$" "${staged_root}/${UNIT_SRC_NAME}"; then
            log "VALIDATION FAIL: staged ${UNIT_SRC_NAME} does not reference /usr/local/rafael/current paths."
            return 1
        fi
    fi

    log "VALIDATION PASS: staged release verified (ELF x86-64, executable, unit paths, smoke check OK)."
    return 0
}

# -----------------------------------------------------------------------------
# Certificate safety
# -----------------------------------------------------------------------------

# query_setting reads one key from the settings table via a READ-ONLY psql
# SELECT run as the postgres system user. Never writes to the database.
query_setting() {
    local key="$1"
    local db_name
    db_name="$(read_dsn_component dbname)"
    [[ -n "${db_name}" ]] || return 1
    sudo -u postgres psql -t -A -d "${db_name}" -c "SELECT value FROM settings WHERE key='${key}'" 2>/dev/null
}

# preflight_certs discovers the configured TLS paths (webCertFile, webCertKey,
# subCertFile, subKeyFile) straight from the settings table and verifies each
# exists and is readable. Certificates are NEVER deleted, moved, or modified.
# Per spec: if the paths cannot be determined safely -> NO-GO.
preflight_certs() {
    local query_ok=1
    local values=()

    local key val
    for key in webCertFile webCertKey subCertFile subKeyFile; do
        val="$(query_setting "${key}")" || query_ok=0
        [[ -n "${val}" ]] || val=""
        values+=("${val}")
    done

    if (( ! query_ok )); then
        log "CERT CHECK FAIL: could not read panel settings from PostgreSQL; certificate paths cannot be determined safely."
        return 1
    fi

    local found_any=0
    local failed=0
    local i=0
    for key in webCertFile webCertKey subCertFile subKeyFile; do
        local path="${values[i]}"
        ((i++))
        if [[ -z "${path}" ]]; then
            continue
        fi
        found_any=1
        if [[ -e "${path}" ]]; then
            if [[ -r "${path}" ]]; then
                log "CERT CHECK: ${key}=${path} exists and is readable."
            else
                log "CERT CHECK FAIL: ${key}=${path} exists but is NOT readable."
                failed=1
            fi
        else
            log "CERT CHECK: ${key}=${path} does not exist (stale setting; left untouched, never deleted)."
        fi
    done

    if (( ! found_any )); then
        log "CERT CHECK: no TLS certificate paths configured; nothing to protect."
    fi
    return "${failed}"
}

# configured_cert_paths_under prints configured cert paths that live under
# the flat root (they must be preserved IN PLACE during migration).
configured_cert_paths_under() {
    local base="$1"
    local key val
    for key in webCertFile webCertKey subCertFile subKeyFile; do
        val="$(query_setting "${key}" || true)"
        [[ -z "${val}" ]] && continue
        if [[ "${val}" == "${base}"/* && -e "${val}" ]]; then
            printf '%s\n' "${val}"
        fi
    done
}

# backup_certs_flat makes backup copies of certificate-like files found in
# the flat install root into persistent/certs. Originals are never moved.
backup_certs_flat() {
    local flat_dir="$1"
    [[ -d "${flat_dir}" ]] || return 0

    local persistent_dir="${ROOT_DIR}/persistent/certs"
    local f copied=0
    while IFS= read -r -d '' f; do
        [[ -f "${f}" ]] || continue
        mkdir -p "${persistent_dir}"
        local dest="${persistent_dir}/$(basename -- "${f}")"
        if [[ ! -e "${dest}" ]]; then
            cp -p "${f}" "${dest}"
            chmod 600 "${dest}" 2>/dev/null || true
            log "CERT SAFETY: backup copy saved: ${f} -> ${dest}"
            copied=1
        fi
    done < <(find "${flat_dir}" -maxdepth 1 -type f \( -name '*.pem' -o -name '*.crt' -o -name '*.key' -o -name '*.cer' \) -print0 2>/dev/null)
    (( copied )) || log "CERT SAFETY: no certificate-like files found in flat root (only settings-configured paths matter)."
    return 0
}

# -----------------------------------------------------------------------------
# Backups
# -----------------------------------------------------------------------------

db_backup() {
    local db_name db_user backup_dir
    db_name="$(read_dsn_component dbname)"
    db_user="$(read_dsn_component user)"
    backup_dir="${BACKUP_ROOT}/$(date +%Y%m%d%H%M%S)"
    mkdir -p "${backup_dir}"
    chmod 700 "${backup_dir}"

    if [[ -n "${db_name}" ]]; then
        local dump="${backup_dir}/rafael_panel.sql"
        log "BACKUP: pg_dump ${db_name} -> ${dump}"
        if ! sudo -u postgres pg_dump "${db_name}" >"${dump}"; then
            log "BACKUP FAIL: pg_dump failed."
            return 1
        fi
        if [[ ! -s "${dump}" ]]; then
            log "BACKUP FAIL: pg_dump produced an empty file."
            return 1
        fi
        chmod 600 "${dump}"
        log "BACKUP: database dump OK ($(stat -c%s "${dump}") bytes)."
    else
        log "BACKUP FAIL: could not determine database name from the DSN."
        return 1
    fi

    # Env + unit + CLI alias backups (small, always taken).
    [[ -f "${ENV_FILE}" ]] && cp -p "${ENV_FILE}" "${backup_dir}/rafael.env"
    [[ -f "${UNIT_DST}" ]] && cp -p "${UNIT_DST}" "${backup_dir}/rafael.service"
    [[ -f "${CLI_ALIAS}" ]] && cp -p "${CLI_ALIAS}" "${backup_dir}/rafael-cli"

    log "BACKUP: artifacts saved in ${backup_dir}"
    LAST_BACKUP_DIR="${backup_dir}"
    return 0
}

# -----------------------------------------------------------------------------
# Health check
# -----------------------------------------------------------------------------

http_health_probe() {
    # Port and base path come from the panel settings via the running
    # release's `setting -show` subcommand (reads the DB, starts no server).
    local panel="${CURRENT_LINK}/rafael-panel"
    is_executable_file "${panel}" || return 1

    local settings cert_settings port base_path cert_file scheme
    settings="$(load_panel_env; "${panel}" setting -show true 2>/dev/null || true)"
    port="$(printf '%s\n' "${settings}" | sed -n 's/^port:[[:space:]]*//p' | grep -E '^[0-9]+' | head -n1)"
    base_path="$(printf '%s\n' "${settings}" | sed -n 's/^webBasePath:[[:space:]]*//p' | head -n1)"
    [[ -z "${base_path}" ]] && base_path="/"

    # NOTE: must use `setting -getCert true` (read-only display). The `cert`
    # subcommand with empty flags calls updateCert("","") which would WIPE
    # the certificate settings in the database.
    cert_settings="$(load_panel_env; "${panel}" setting -getCert true 2>/dev/null || true)"
    cert_file="$(printf '%s\n' "${cert_settings}" | sed -n 's/^cert:[[:space:]]*//p' | head -n1)"
    if [[ -n "${cert_file}" ]]; then
        scheme="https"
    else
        scheme="http"
    fi

    if [[ -z "${port}" ]]; then
        log "HEALTH: could not determine panel port; skipping HTTP probe (is-active only)."
        return 0
    fi

    local url="${scheme}://127.0.0.1:${port}${base_path}"
    log "HEALTH: probing ${url}"
    local code
    code="$(curl -sk -o /dev/null -w '%{http_code}' --max-time 5 "${url}" 2>/dev/null || echo "000")"
    case "${code}" in
        200|301|302|404) log "HEALTH: HTTP probe OK (${code})."; return 0 ;;
        # DomainValidatorMiddleware intentionally returns 403 when this
        # loopback probe uses 127.0.0.1 as Host while webDomain is configured.
        # The HTTP listener is still responding; health_check separately
        # verifies that systemd stays active and the Xray process is running.
        403) log "HEALTH: HTTP probe OK (403; panel domain restriction rejected loopback Host)."; return 0 ;;
        *) log "HEALTH: HTTP probe failed (${code})."; return 1 ;;
    esac
}

# xray_process_check verifies the Xray core is actually RUNNING. The panel
# starts the core unconditionally at boot (web.go startTask -> RestartXray)
# and a failed core start only logs a warning — the web UI keeps serving.
# An HTTP-200 health check therefore passes while the proxy engine is dead
# (e.g. a stale XUI_BIN_FOLDER pointing at a moved flat bin directory), which
# is exactly the false positive this check closes. Read-only: inspects the
# env file, the release tree, and the process table — writes nothing.
xray_process_check() {
    local bin_folder xray_bin active
    bin_folder="$(read_env_value XUI_BIN_FOLDER)"
    if [[ -z "${bin_folder}" ]]; then
        log "HEALTH CHECK FAIL: XUI_BIN_FOLDER not set in ${ENV_FILE}; cannot verify the Xray core."
        return 1
    fi

    # Verify the binary the environment points at exists. Resolve through the
    # ACTIVE RELEASE directory (the same file current/bin resolves to) rather
    # than through the symlink, so the check holds on hosts where the symlink
    # itself is hard to traverse.
    active="$(resolve_current)"
    is_valid_release_name "${active}" || {
        log "HEALTH CHECK FAIL: cannot resolve active release for the Xray binary check."
        return 1
    }
    xray_bin="${RELEASES_DIR}/${active}/bin/xray-linux-amd64"
    if ! is_executable_file "${xray_bin}"; then
        log "HEALTH CHECK FAIL: Xray binary missing or not executable at ${xray_bin}."
        return 1
    fi

    # Process check. The running core's command line contains the binary path
    # from the env file it was started with, so match against that. If pgrep
    # is unavailable (minimal systems), degrade to a warning instead of
    # failing the update on a missing utility.
    if ! command -v pgrep >/dev/null 2>&1; then
        log "HEALTH: pgrep unavailable; Xray process not verified (web UI + is-active only)."
        return 0
    fi
    if ! pgrep -f "${bin_folder}/xray-linux-amd64" >/dev/null 2>&1; then
        log "HEALTH CHECK FAIL: Xray core process is not running (${bin_folder}/xray-linux-amd64)."
        return 1
    fi
    log "HEALTH: Xray core process running with ${bin_folder}/xray-linux-amd64."
    return 0
}

health_check() {
    local deadline=$(( $(date +%s) + HEALTH_CHECK_SECONDS ))
    local restarts_at_start
    restarts_at_start="$(systemctl show "${SERVICE_NAME}" -p NRestarts --value 2>/dev/null || echo 0)"
    local was_active_once=0

    while (( $(date +%s) < deadline )); do
        if systemctl is-active --quiet "${SERVICE_NAME}"; then
            was_active_once=1
        fi
        local restarts_now
        restarts_now="$(systemctl show "${SERVICE_NAME}" -p NRestarts --value 2>/dev/null || echo 0)"
        if (( restarts_now > restarts_at_start + 2 )); then
            log "HEALTH CHECK FAIL: crash loop detected (NRestarts ${restarts_at_start} -> ${restarts_now})."
            return 1
        fi
        if (( was_active_once )) && http_health_probe && xray_process_check; then
            # Require the process to stay up for the stability window.
            local stable_until=$(( $(date +%s) + HEALTH_STABILITY_SECONDS ))
            while (( $(date +%s) < stable_until )); do
                systemctl is-active --quiet "${SERVICE_NAME}" || {
                    log "HEALTH CHECK FAIL: service went inactive during stability window."
                    return 1
                }
                sleep "${HEALTH_CHECK_INTERVAL}"
            done
            log "HEALTH CHECK PASS: active, stable for ${HEALTH_STABILITY_SECONDS}s, HTTP responding, Xray core running."
            return 0
        fi
        sleep "${HEALTH_CHECK_INTERVAL}"
    done
    log "HEALTH CHECK FAIL: no healthy state within ${HEALTH_CHECK_SECONDS}s."
    return 1
}

# verify_env_integrity checks the env file still points at the same database
# and at the current release's bin directory (the updater must not declare
# success while the service would boot with a stale XUI_BIN_FOLDER).
verify_env_integrity() {
    local db_type db_name
    db_type="$(read_env_value XUI_DB_TYPE)"
    db_name="$(read_dsn_component dbname)"
    if [[ "${db_type}" != "postgres" ]]; then
        log "ENV CHECK FAIL: XUI_DB_TYPE is '${db_type}', expected 'postgres'."
        return 1
    fi
    if [[ -z "${db_name}" ]]; then
        log "ENV CHECK FAIL: XUI_DB_DSN lost its dbname component."
        return 1
    fi

    local bin_folder active
    bin_folder="$(read_env_value XUI_BIN_FOLDER)"
    if [[ "${bin_folder}" != "${ROOT_DIR}/current/bin" ]]; then
        log "ENV CHECK FAIL: XUI_BIN_FOLDER is '${bin_folder}', expected '${ROOT_DIR}/current/bin'."
        return 1
    fi
    active="$(resolve_current)"
    is_valid_release_name "${active}" || {
        log "ENV CHECK FAIL: cannot resolve active release for the bin check."
        return 1
    }
    if ! is_executable_file "${RELEASES_DIR}/${active}/bin/xray-linux-amd64"; then
        log "ENV CHECK FAIL: xray binary not found for active release ${active} (${bin_folder})."
        return 1
    fi

    log "ENV CHECK: postgres + dbname=${db_name} preserved, XUI_BIN_FOLDER=${bin_folder} (DSN masked: $(mask_dsn "$(read_env_value XUI_DB_DSN)"))."
    return 0
}

# -----------------------------------------------------------------------------
# Flat-install migration (one-time)
# -----------------------------------------------------------------------------

is_flat_install() {
    # A flat install has the panel binary in the root dir AND a current
    # pointer that is not a release: either no link at all, or the unit's
    # self-pointing boot shim (ExecStartPre creates "current -> ." on flat
    # installs so the current-based paths resolve). A release layout always
    # resolves current to releases/vNN.
    [[ -d "${ROOT_DIR}" ]] && [[ -f "${ROOT_DIR}/rafael-panel" ]] \
        && ! is_valid_release_name "$(resolve_current)"
}

# migrate_flat_install COPIES (never moves) the flat install's release files
# into releases/v<marker>. The flat originals stay untouched until the
# service is confirmed running from the release tree (tidy_flat_leftovers).
# Certificate files are never moved at any point.
migrate_flat_install() {
    local flat_marker="$1"
    local release_dir="${RELEASES_DIR}/${flat_marker}"

    if [[ -e "${release_dir}" ]]; then
        # A dir that exists but lacks the files the current-based unit needs
        # is an INTERRUPTED copy (crash mid-migration): re-copy over it
        # instead of activating a half-built release on retry.
        if [[ -f "${release_dir}/rafael-panel" && -f "${release_dir}/bin/xray-linux-amd64" ]]; then
            log "MIGRATION: target release dir already exists and looks complete (${release_dir}); flat copy not needed."
            return 0
        fi
        log "MIGRATION: existing ${release_dir} is incomplete (interrupted copy?); re-copying."
    fi

    log "MIGRATION: copying flat installation into ${release_dir} (originals untouched)..."
    mkdir -p "${RELEASES_DIR}"
    mkdir -p "${release_dir}"

    local known=(
        rafael-panel rafael.sh bootstrap.sh install-local.sh "${UNIT_SRC_NAME}"
    )
    local item
    for item in "${known[@]}"; do
        if [[ -e "${ROOT_DIR}/${item}" ]]; then
            cp -a "${ROOT_DIR}/${item}" "${release_dir}/"
        fi
    done
    if [[ -d "${ROOT_DIR}/bin" ]]; then
        cp -a "${ROOT_DIR}/bin" "${release_dir}/bin"
    fi

    chmod 755 "${release_dir}/rafael-panel" "${release_dir}/rafael.sh" "${release_dir}/bootstrap.sh" 2>/dev/null || true

    log "MIGRATION: Build ${flat_marker} copied into ${release_dir}."
    return 0
}

# tidy_flat_leftovers moves the now-duplicated flat release files into
# persistent/flat-preserved AFTER the service is confirmed running from the
# release tree. Certificate files configured in the panel settings are always
# preserved in place. Nothing is deleted.
tidy_flat_leftovers() {
    local preserve_dir="${ROOT_DIR}/persistent/flat-preserved"
    mkdir -p "${preserve_dir}"

    # Cert paths configured in the DB must keep working at their exact
    # absolute paths — never move them.
    local -A keep=()
    local cert_path
    while IFS= read -r cert_path; do
        [[ -n "${cert_path}" ]] && keep["${cert_path}"]=1
    done < <(configured_cert_paths_under "${ROOT_DIR}")

    local items=(
        rafael-panel rafael.sh bootstrap.sh install-local.sh "${UNIT_SRC_NAME}" bin
    )
    local item
    for item in "${items[@]}"; do
        local src="${ROOT_DIR}/${item}"
        [[ -e "${src}" ]] || continue
        # Never move anything a configured certificate points at.
        if [[ -n "${keep["${src}"]:-}" ]]; then
            log "MIGRATION: ${src} is a configured certificate path; preserved in place."
            continue
        fi
        mv "${src}" "${preserve_dir}/"
        log "MIGRATION: flat duplicate moved to ${preserve_dir}/$(basename -- "${src}")."
    done
}

# install_stable_layer installs/refreshes the release manager + CLI launcher
# OUTSIDE the release tree. Idempotent. Never touches releases.
install_stable_layer() {
    local source_dir="${1:-}" # optional dir containing rafael-updater.sh / rafael-launcher.sh

    if [[ -n "${source_dir}" && -f "${source_dir}/${UPDATER_NAME}" ]]; then
        install -m 755 "${source_dir}/${UPDATER_NAME}" "${ROOT_DIR}/${UPDATER_NAME}"
    fi
    if [[ -n "${source_dir}" && -f "${source_dir}/${LAUNCHER_SRC_NAME}" ]]; then
        install -m 755 "${source_dir}/${LAUNCHER_SRC_NAME}" "${CLI_ALIAS}"
    elif [[ ! -x "${CLI_ALIAS}" ]]; then
        # Last-resort fallback: embed the launcher.
        cat >"${CLI_ALIAS}" <<'EOF'
#!/bin/bash
# Stable Rafael launcher (outside the release tree).
# Panel operations run through current; release-control operations are
# routed to the stable release manager so a broken current release can
# never block an update or rollback.
set -e
ROOT_DIR="/usr/local/rafael"
cmd="${1:-menu}"
case "${cmd}" in
    update|rollback)
        exec bash "${ROOT_DIR}/rafael-updater.sh" "${cmd}" "${2:-}"
        ;;
    releases)
        exec bash "${ROOT_DIR}/rafael-updater.sh" status
        ;;
    *)
        if [[ -x "${ROOT_DIR}/current/rafael.sh" ]]; then
            exec bash "${ROOT_DIR}/current/rafael.sh" "$@"
        elif [[ -x "${ROOT_DIR}/rafael.sh" ]]; then
            exec bash "${ROOT_DIR}/rafael.sh" "$@"
        fi
        echo "Rafael: no runnable release under ${ROOT_DIR}." >&2
        echo "Control commands remain available:" >&2
        echo "  rafael update <archive>   rafael rollback [version]   rafael releases" >&2
        exit 1
        ;;
esac
EOF
        chmod 755 "${CLI_ALIAS}"
    fi

    if [[ ! -x "${ROOT_DIR}/${UPDATER_NAME}" ]]; then
        log "UPDATE FAILED: no release manager available at ${ROOT_DIR}/${UPDATER_NAME}."
        return 1
    fi

    log "STABLE LAYER: release manager + CLI launcher installed."
    return 0
}

# activate_release atomically points current at a release dir.
activate_release() {
    local target="$1"
    local release_dir="${RELEASES_DIR}/${target}"
    validate_path_under_root "${release_dir}" || return 1
    is_valid_release_name "${target}" || { log "Invalid release name: ${target}"; return 1; }
    [[ -d "${release_dir}" ]] || { log "Release dir missing: ${release_dir}"; return 1; }

    local tmp_link="${ROOT_DIR}/.current.tmp.$$"
    ln -sfn "releases/${target}" "${tmp_link}"
    mv -Tf "${tmp_link}" "${CURRENT_LINK}"   # atomic replacement
    log "ACTIVATION: current -> releases/${target}"
    return 0
}

# install_unit_from_release installs the unit shipped inside a release dir.
install_unit_from_release() {
    local release_dir="$1"
    if [[ -f "${release_dir}/${UNIT_SRC_NAME}" ]]; then
        cp -f "${release_dir}/${UNIT_SRC_NAME}" "${UNIT_DST}"
        chmod 644 "${UNIT_DST}"
        log "SYSTEMD: unit installed from ${release_dir}/${UNIT_SRC_NAME} (stable current/ paths)."
    else
        log "SYSTEMD WARN: no unit template in release; keeping existing unit."
    fi
}

# prune_releases keeps the newest N releases. Never deletes the active one.
prune_releases() {
    local keep="${KEEP_RELEASES}"
    local active
    active="$(resolve_current)"
    local names=()
    local d n
    while IFS= read -r -d '' d; do
        n="$(basename -- "${d}")"
        is_valid_release_name "${n}" || continue
        names+=("${n}")
    done < <(find "${RELEASES_DIR}" -mindepth 1 -maxdepth 1 -type d -print0 2>/dev/null)

    (( ${#names[@]} > keep )) || return 0

    local sorted to_remove name
    sorted="$(printf '%s\n' "${names[@]}" | sed 's/^v//' | sort -n | sed 's/^/v/')"
    to_remove="$(printf '%s\n' "${sorted}" | head -n -$((keep)))"
    while IFS= read -r name; do
        [[ -z "${name}" ]] && continue
        if [[ "${name}" == "${active}" ]]; then
            log "PRUNE: skipping active release ${name}."
            continue
        fi
        local dir="${RELEASES_DIR}/${name}"
        validate_path_under_root "${dir}" || continue
        log "PRUNE: removing old release ${name} (retention: keep ${keep})."
        rm -rf "${dir}"
    done <<<"${to_remove}"
}

# -----------------------------------------------------------------------------
# Rollback
# -----------------------------------------------------------------------------

do_rollback() {
    local target="${1:-}"
    local active
    active="$(resolve_current)"
    if [[ -z "${target}" ]]; then
        # Default: newest release that is not the active one.
        target="$(find "${RELEASES_DIR}" -mindepth 1 -maxdepth 1 -type d -printf '%f\n' 2>/dev/null \
            | grep -E '^v[0-9]+$' | sed 's/^v//' | sort -rn | sed 's/^/v/' \
            | grep -v "^${active}$" | head -n 1 || true)"
        if [[ -z "${target}" ]]; then
            log "ROLLBACK FAIL: no other release available to roll back to."
            return 1
        fi
    fi
    is_valid_release_name "${target}" || { log "ROLLBACK FAIL: invalid release name '${target}'."; return 1; }
    [[ -d "${RELEASES_DIR}/${target}" ]] || { log "ROLLBACK FAIL: release ${target} not found."; return 1; }
    [[ "${target}" == "${active}" ]] && { log "ROLLBACK: ${target} is already active."; return 0; }

    log "ROLLBACK: switching current -> releases/${target} (from ${active:-none})..."
    activate_release "${target}" || return 1
    systemctl daemon-reload
    if ! systemctl restart "${SERVICE_NAME}"; then
        log "ROLLBACK FAIL: service did not restart on ${target}."
        return 1
    fi
    if ! health_check; then
        log "ROLLBACK FAIL: previous release ${target} failed health check. Manual recovery required."
        log "  current now points at: $(resolve_current)"
        log "  available releases: $(ls -1 "${RELEASES_DIR}" 2>/dev/null | tr '\n' ' ')"
        return 1
    fi
    log "ROLLBACK PASS: Build ${target} is active and healthy."
    return 0
}

# -----------------------------------------------------------------------------
# Preflight (read-only)
# -----------------------------------------------------------------------------

do_preflight() {
    local archive="${1:-}"
    log "PREFLIGHT: read-only checks (no production changes)."
    log "PREFLIGHT: root dir          = ${ROOT_DIR}"
    if is_flat_install; then
        local marker
        marker="$(sed -n 's/\r$//; s/^CLI_BUILD_VERSION="\([^"]*\)"$/\1/p' "${ROOT_DIR}/rafael.sh" 2>/dev/null)"
        log "PREFLIGHT: install mode     = FLAT (Build ${marker:-unknown})"
        log "PREFLIGHT: one-time migration to releases/ will run during update."
    elif [[ -L "${CURRENT_LINK}" ]]; then
        log "PREFLIGHT: install mode     = RELEASE-based (current -> $(resolve_current))"
    else
        log "PREFLIGHT: install mode     = UNKNOWN (no current symlink, no flat binary)."
    fi
    log "PREFLIGHT: env file          = ${ENV_FILE} ($( [[ -f ${ENV_FILE} ]] && echo present || echo MISSING ))"
    log "PREFLIGHT: db type           = $(read_env_value XUI_DB_TYPE || echo unknown)"
    log "PREFLIGHT: db name           = $(read_dsn_component dbname || echo unknown) (DSN masked: $(mask_dsn "$(read_env_value XUI_DB_DSN)"))"
    log "PREFLIGHT: systemd           = $(systemctl is-active "${SERVICE_NAME}" 2>/dev/null || echo unknown)"

    if [[ -n "${archive}" ]]; then
        RELEASE_MARKER=""
        if validate_archive "${archive}"; then
            log "PREFLIGHT PASS: archive ${archive} validated (build ${RELEASE_MARKER})."
        else
            log "PREFLIGHT FAIL: archive ${archive} did not pass validation."
            return 1
        fi
    else
        log "PREFLIGHT PASS (no archive given; environment checks only)."
    fi
    return 0
}

# -----------------------------------------------------------------------------
# Full update flow
# -----------------------------------------------------------------------------

do_update() {
    local archive="${1:-}"

    # --- locate archive -------------------------------------------------------
    if [[ -z "${archive}" ]]; then
        local candidates=()
        local base f
        for base in "${PWD}" "/root" "/tmp" "${HOME}"; do
            [[ -d "${base}" ]] || continue
            while IFS= read -r -d '' f; do
                candidates+=("${f}")
            done < <(find "${base}" -maxdepth 3 -type f -name 'rafael-linux-amd64.tar.gz' -print0 2>/dev/null)
        done
        if [[ ${#candidates[@]} -eq 0 ]]; then
            die "no update archive supplied or found (pass the path as an argument)."
        fi
        if [[ ${#candidates[@]} -eq 1 ]]; then
            archive="${candidates[0]}"
        else
            echo "Detected update archives:"
            local i=1
            for f in "${candidates[@]}"; do
                echo "  ${i}) ${f}"
                ((i++))
            done
            read -rp "Select an archive: " sel
            [[ "${sel}" =~ ^[0-9]+$ ]] && (( sel >= 1 && sel <= ${#candidates[@]} )) || die "invalid selection."
            archive="${candidates[sel-1]}"
        fi
    fi

    # --- 1. validate BEFORE touching the service -------------------------------
    RELEASE_MARKER=""
    log "UPDATE: current release = $(resolve_current || echo flat/unknown), target archive = ${archive}"
    if ! validate_archive "${archive}"; then
        log "VALIDATION FAIL"
        die "archive validation failed; service was NOT stopped and nothing was changed."
    fi
    local target="${RELEASE_MARKER}"
    log "VALIDATION PASS: archive build = ${target}"

    # --- 2. stage + validate extracted tree (service still running) ------------
    # Stage under ROOT_DIR (not /tmp) so the final move into releases/ is a
    # same-filesystem rename — atomic — rather than a cross-device copy.
    STAGE_DIR="$(mktemp -d "${ROOT_DIR}/.rafael-stage-XXXXXX")"
    chmod 700 "${STAGE_DIR}"
    trap '[[ -n "${STAGE_DIR:-}" && -d "${STAGE_DIR}" ]] && rm -rf "${STAGE_DIR}"; STAGE_DIR=""' EXIT
    tar -xzf "${archive}" -C "${STAGE_DIR}"
    local staged="${STAGE_DIR}/rafael"

    # Mark the known executables BEFORE validation: release archives are
    # built on Windows, where tar cannot reliably store the exec bit. The
    # validation below then checks structure, ELF architecture, and smoke-
    # runs the binary — chmod here weakens nothing.
    chmod 755 "${staged}" "${staged}/bin" 2>/dev/null || true
    local x
    for x in rafael-panel rafael.sh bootstrap.sh install-local.sh rafael-updater.sh rafael-launcher.sh; do
        [[ -f "${staged}/${x}" ]] && chmod 755 "${staged}/${x}"
    done
    [[ -f "${staged}/bin/xray-linux-amd64" ]] && chmod 755 "${staged}/bin/xray-linux-amd64"

    if ! validate_staged_release "${staged}"; then
        die "staged release validation failed; service was NOT stopped and nothing was changed."
    fi

    # Normalize the remaining permissions.
    find "${staged}" -type d -exec chmod 755 {} +
    find "${staged}" -type f -exec chmod 644 {} +
    for x in rafael-panel rafael.sh bootstrap.sh install-local.sh rafael-updater.sh rafael-launcher.sh; do
        [[ -f "${staged}/${x}" ]] && chmod 755 "${staged}/${x}"
    done
    [[ -f "${staged}/bin/xray-linux-amd64" ]] && chmod 755 "${staged}/bin/xray-linux-amd64"

    # --- 3. idempotency: same version already active? ---------------------------
    if [[ "$(resolve_current)" == "${target}" && -d "${RELEASES_DIR}/${target}" && "${RAFAEL_FORCE_REINSTALL:-}" != "1" ]]; then
        if systemctl is-active --quiet "${SERVICE_NAME}"; then
            log "UPDATE: Build ${target} is already the active, healthy release (idempotent no-op)."
            log "UPDATE SUCCESS (no change needed)."
            return 0
        fi
    fi

    # --- 4. backups --------------------------------------------------------------
    if ! db_backup; then
        die "backup phase failed; aborting before any production change."
    fi

    # --- 5. certificate safety (NO-GO on undeterminable paths) -------------------
    if ! preflight_certs; then
        die "certificate preflight failed (paths undeterminable or unreadable). NO-GO."
    fi

    # --- 6. one-time flat migration (copy-then-switch; nothing deleted) ----------
    local active_before
    local migrated_marker=""
    active_before="$(resolve_current)"
    if is_flat_install; then
        local flat_marker
        flat_marker="$(sed -n 's/\r$//; s/^CLI_BUILD_VERSION="\([^"]*\)"$/\1/p' "${ROOT_DIR}/rafael.sh" 2>/dev/null)"
        is_valid_release_name "${flat_marker}" || die "cannot determine flat-install build marker."
        backup_certs_flat "${ROOT_DIR}"
        migrate_flat_install "${flat_marker}" || die "flat-install migration failed."
        # Remember that THIS run created releases/<flat_marker>: if the target
        # archive carries the same marker (flat v39 -> fixed v39), step 7 must
        # replace the just-migrated copy instead of dying on the collision.
        migrated_marker="${flat_marker}"
        active_before="${flat_marker}"

        # Switch current, the systemd unit, the env file and the stable layer
        # to the release layout BEFORE the first restart on the preserved old
        # build, so a failed update can never leave the service without a
        # valid target. ORDERING INVARIANT: from the moment the flat files are
        # tidied (below), the flat paths no longer exist — so the unit
        # installed here MUST be the CURRENT-BASED unit from the validated
        # staged release (releases/v<flat_marker> keeps the faithful Build 37
        # flat unit for inspection only; it is NEVER installed). Likewise the
        # env file must already carry current/bin BEFORE the restart: systemd
        # reads EnvironmentFile at unit start, and an ExecStartPre (bootstrap)
        # correction only takes effect on the NEXT start.
        activate_release "${flat_marker}" || die "could not point current at preserved ${flat_marker}."
        install_unit_from_release "${staged}"
        set_env_bin_folder "${ROOT_DIR}/current/bin" || die "could not update XUI_BIN_FOLDER in ${ENV_FILE} before restart."
        install_stable_layer "${staged}"
        systemctl daemon-reload
        if ! systemctl restart "${SERVICE_NAME}" || ! health_check; then
            log "MIGRATION: restart/health check on preserved ${flat_marker} failed; restoring flat unit, env and layout."
            if [[ -n "${LAST_BACKUP_DIR:-}" && -f "${LAST_BACKUP_DIR}/rafael.service" ]]; then
                cp -f "${LAST_BACKUP_DIR}/rafael.service" "${UNIT_DST}"
            fi
            if [[ -n "${LAST_BACKUP_DIR:-}" && -f "${LAST_BACKUP_DIR}/rafael.env" ]]; then
                cp -f "${LAST_BACKUP_DIR}/rafael.env" "${ENV_FILE}"
            fi
            rm -f "${CURRENT_LINK}"   # remove the symlink only; never the tree
            systemctl daemon-reload
            systemctl restart "${SERVICE_NAME}" || true
            die "flat migration failed at restart; flat layout restored."
        fi
        # Service now runs from the release tree; duplicates can be tidied.
        # Invariant from here on: the installed unit and env reference
        # current/ paths only — no systemd reference to flat paths remains.
        tidy_flat_leftovers
    fi

    # --- 7. install release (refuse silent overwrite) ----------------------------
    local target_dir="${RELEASES_DIR}/${target}"
    if [[ -d "${target_dir}" ]]; then
        if [[ "${target}" == "${migrated_marker}" ]]; then
            # The dir was created by THIS run's flat migration (the flat
            # install carried the same marker as the target archive — e.g.
            # flat v39 updated by a fixed v39). It holds the OLD build we
            # just preserved; replacing it is the whole point of this
            # update, so no explicit FORCE is required. Nothing is deleted.
            local stamp
            stamp="$(date +%Y%m%d%H%M%S)"
            mv "${target_dir}" "${RELEASES_DIR}/${target}.replaced-${stamp}"
            log "STAGE: migrated ${target} (old flat build) moved aside as ${target}.replaced-${stamp}."
        elif [[ "${RAFAEL_FORCE_REINSTALL:-}" == "1" ]]; then
            local stamp
            stamp="$(date +%Y%m%d%H%M%S)"
            mv "${target_dir}" "${RELEASES_DIR}/${target}.replaced-${stamp}"
            log "STAGE: existing ${target} moved aside as ${target}.replaced-${stamp} (RAFAEL_FORCE_REINSTALL=1)."
        else
            die "release ${target} already exists at ${target_dir}; set RAFAEL_FORCE_REINSTALL=1 to replace it explicitly."
        fi
    fi

    # Refresh the stable layer from this archive.
    install_stable_layer "${staged}"

    # Move staged tree into releases. Staging lives under ROOT_DIR, so this is
    # a same-filesystem rename — atomic — not a cross-device copy.
    mkdir -p "${RELEASES_DIR}"
    mv "${staged}" "${target_dir}" || die "failed to move staged release into ${target_dir}."
    rm -rf "${STAGE_DIR}"   # now-empty staging parent (the tree itself was moved)
    STAGE_DIR=""
    log "STAGE: release installed at ${target_dir}."

    # --- 8. atomic activation ------------------------------------------------------
    if ! activate_release "${target}"; then
        log "ACTIVATION FAIL"
        do_rollback "${active_before}" || true
        die "activation failed; rolled back to ${active_before}."
    fi
    install_unit_from_release "${target_dir}"
    systemctl daemon-reload

    # --- 9. restart + health check --------------------------------------------------
    if ! systemctl restart "${SERVICE_NAME}"; then
        log "RESTART FAIL: systemctl restart failed."
        do_rollback "${active_before}" || true
        die "service restart failed; rolled back to ${active_before}."
    fi

    if ! health_check; then
        log "HEALTH CHECK FAIL for Build ${target}."
        if do_rollback "${active_before}"; then
            log "UPDATE FAILED: rolled back to Build ${active_before}. Build ${target} retained for inspection."
        else
            log "UPDATE FAILED + ROLLBACK FAILED: manual recovery required."
        fi
        exit 1
    fi

    if ! verify_env_integrity; then
        log "ENV CHECK FAIL after activation."
        do_rollback "${active_before}" || true
        die "environment integrity check failed; rolled back."
    fi

    # --- 10. retention -----------------------------------------------------------------
    prune_releases

    log "UPDATE SUCCESS: Build ${target} active, healthy, previous releases retained."
    return 0
}

# -----------------------------------------------------------------------------
# Status
# -----------------------------------------------------------------------------

do_status() {
    echo "Rafael release status"
    echo "  root:    ${ROOT_DIR}"
    if [[ -L "${CURRENT_LINK}" ]]; then
        echo "  current: $(resolve_current) -> $(readlink "${CURRENT_LINK}")"
    elif [[ -d "${CURRENT_LINK}" ]]; then
        echo "  current: DIRECTORY (not a symlink!) at $(readlink -f "${CURRENT_LINK}")"
    else
        echo "  current: not set"
    fi
    if is_flat_install; then
        echo "  mode:    FLAT install (migration pending)"
    fi
    echo "  releases:"
    local d
    while IFS= read -r d; do
        [[ -z "${d}" ]] && continue
        echo "    ${d}"
    done < <(find "${RELEASES_DIR}" -mindepth 1 -maxdepth 1 -type d -printf '%f\n' 2>/dev/null | sort -V)
    echo "  service: $(systemctl is-active "${SERVICE_NAME}" 2>/dev/null || echo unknown)"
    echo "  db:      $(read_dsn_component dbname || echo unknown) ($(read_env_value XUI_DB_TYPE || echo unknown), DSN masked)"
    return 0
}

# -----------------------------------------------------------------------------
# Entry point
# -----------------------------------------------------------------------------

main() {
    local cmd="${1:-status}"
    shift || true

    require_root
    acquire_lock

    case "${cmd}" in
        preflight)
            do_preflight "${1:-}"
            ;;
        update)
            do_update "${1:-}"
            ;;
        rollback)
            do_rollback "${1:-}"
            ;;
        status|releases)
            do_status
            ;;
        *)
            echo "Usage: rafael-updater.sh {preflight|update|rollback|status} [archive|version]"
            exit 1
            ;;
    esac
}

# Guard so the updater can also be SOURCED as a library (used by the CLI
# bootstrap bridge, which validates an archive before installing this file
# as the stable release manager).
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
    main "$@"
fi

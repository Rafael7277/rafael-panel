#!/bin/bash

# Release-aware Rafael CLI. Panel operations resolve the application
# directory from this script's location (works flat AND under the versioned
# release layout, including through the /usr/local/rafael/current symlink).
# Release management (update / rollback) is delegated to the stable release
# manager /usr/local/rafael/rafael-updater.sh which lives OUTSIDE the release
# tree, so a broken current release can never prevent a rollback.
set -e

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
APP_DIR="${RAFAEL_MAIN_FOLDER:-${SCRIPT_DIR}}"
ROOT_DIR="/usr/local/rafael"
RELEASE_MANAGER="${RAFAEL_RELEASE_MANAGER:-${ROOT_DIR}/rafael-updater.sh}"
ENV_FILE="/etc/default/rafael"
CLI_BUILD_VERSION="v63"
# SHA-256 of the rafael-updater.sh that ships in the SAME release archive.
# Injected by the release builder at build time. The bootstrap bridge below
# refuses to install/source a bundled updater whose hash does not match this
# pin, so a tampered archive can never get its "updater" executed.
# Empty (working tree / unbundled CLI) => the bridge fails CLOSED.
BOOTSTRAP_UPDATER_SHA256=""
LOCAL_UPDATE_SEARCH_PATHS=("${PWD}" "${HOME}" "/root" "/tmp")
SELECTED_UPDATE_ARCHIVE=""

run_systemctl() {
    systemctl "$@"
}

load_env() {
    if [[ -f "${ENV_FILE}" ]]; then
        set -a
        # shellcheck disable=SC1090
        source "${ENV_FILE}"
        set +a
    fi
}

run_panel_setting() {
    load_env
    "${APP_DIR}/rafael-panel" setting "$@"
}

run_panel_cert() {
    load_env
    "${APP_DIR}/rafael-panel" cert "$@"
}

restart_panel_and_xray() {
    run_systemctl restart rafael
}

get_panel_version() {
    if [[ -x "${APP_DIR}/rafael-panel" ]]; then
        "${APP_DIR}/rafael-panel" -v 2>/dev/null | head -n 1
        return
    fi
    echo "unknown"
}

# ---------------------------------------------------------------------
# Release management: delegated to the stable manager script.
# ---------------------------------------------------------------------

# bootstrap_release_manager closes the legacy-CLI bootstrap gap: on a host
# that has a flat installation and NO stable release manager yet (a legacy
# Build 37 box, or a fresh flat install), `rafael update <archive>` extracts
# ONLY the rafael/rafael-updater.sh member of the given release archive,
# verifies it against the build-time SHA-256 pin above, runs the updater's
# own full archive validation (traversal/symlink/ELF/marker/manifest), and
# only then installs it as the stable release manager and delegates the
# update to it. Nothing from the archive is executed before validation.
bootstrap_release_manager() {
    local archive_path="$1"

    if [[ "${EUID}" -ne 0 ]]; then
        echo "This command must run as root." >&2
        return 1
    fi
    if [[ ! -f "${archive_path}" ]]; then
        echo "Archive path not found: ${archive_path}" >&2
        return 1
    fi
    if [[ -z "${BOOTSTRAP_UPDATER_SHA256}" ]]; then
        echo "Bootstrap refused: this CLI carries no updater pin" >&2
        echo "(BOOTSTRAP_UPDATER_SHA256 is empty; unbundled working-tree CLI)." >&2
        return 1
    fi

    # Stream the single updater member to a private temp file. A fixed
    # member name piped to stdout cannot traverse, link, or create anything.
    local tmp_updater actual_pin
    tmp_updater="$(mktemp /tmp/rafael-updater-bootstrap-XXXXXX.sh)"
    if ! tar -xzOf "${archive_path}" rafael/rafael-updater.sh > "${tmp_updater}" 2>/dev/null; then
        rm -f "${tmp_updater}"
        echo "Archive does not bundle a release updater (rafael/rafael-updater.sh)." >&2
        echo "Only release-layout archives can bootstrap the release manager." >&2
        return 1
    fi

    actual_pin="$(sha256sum "${tmp_updater}" | awk '{print $1}')"
    if [[ "${actual_pin}" != "${BOOTSTRAP_UPDATER_SHA256}" ]]; then
        rm -f "${tmp_updater}"
        echo "Bootstrap refused: bundled updater SHA-256 does not match this CLI's pin." >&2
        echo "  expected: ${BOOTSTRAP_UPDATER_SHA256}" >&2
        echo "  actual:   ${actual_pin}" >&2
        return 1
    fi

    # The pinned updater is byte-identical to the one shipped with this CLI
    # build, so it is trusted content: source it as a library (its main
    # guard keeps it from self-executing) to reuse its validation stack.
    # shellcheck disable=SC1090
    if ! source "${tmp_updater}"; then
        rm -f "${tmp_updater}"
        echo "Failed to load the pinned release updater." >&2
        return 1
    fi
    if ! validate_archive "${archive_path}"; then
        echo "Bootstrap refused: archive failed the release updater's validation." >&2
        return 1
    fi

    # Validation passed: install the pinned updater as the stable release
    # manager, outside any release tree, then delegate the update to it.
    # The manager re-validates the archive itself before touching anything.
    mkdir -p "$(dirname -- "${RELEASE_MANAGER}")"
    if ! install -m 755 "${tmp_updater}" "${RELEASE_MANAGER}" 2>/dev/null; then
        cp -f "${tmp_updater}" "${RELEASE_MANAGER}"
        chmod 755 "${RELEASE_MANAGER}"
    fi
    rm -f "${tmp_updater}"
    echo "Release manager bootstrapped from ${archive_path}"
    echo "  -> ${RELEASE_MANAGER}"
    exec bash "${RELEASE_MANAGER}" update "${archive_path}"
}

update_panel() {
    local archive_path="${1:-}"
    if [[ ! -x "${RELEASE_MANAGER}" ]]; then
        if [[ -n "${archive_path}" ]]; then
            # No stable manager yet (legacy flat host): the release archive
            # bootstraps it, after full validation of the archive.
            bootstrap_release_manager "${archive_path}"
            return 1
        fi
        echo "Release manager not found: ${RELEASE_MANAGER}"
        echo "A release-based update requires either the release manager installed at"
        echo "  ${RELEASE_MANAGER}"
        echo "or an explicit release archive: rafael update /path/to/rafael-linux-amd64.tar.gz"
        return 1
    fi
    if [[ -n "${archive_path}" ]]; then
        exec bash "${RELEASE_MANAGER}" update "${archive_path}"
    fi
    exec bash "${RELEASE_MANAGER}" update
}

rollback_panel() {
    local target="${1:-}"
    if [[ ! -x "${RELEASE_MANAGER}" ]]; then
        echo "Release manager not found: ${RELEASE_MANAGER}"
        return 1
    fi
    exec bash "${RELEASE_MANAGER}" rollback "${target}"
}

show_releases() {
    if [[ ! -x "${RELEASE_MANAGER}" ]]; then
        echo "Release manager not found: ${RELEASE_MANAGER}"
        return 1
    fi
    exec bash "${RELEASE_MANAGER}" status
}

# Legacy flat-layout updater, kept as a fallback when the release manager is
# not installed (fresh flat installs, rescue situations). It performs a
# full backup + restart-failure rollback exactly as before.
choose_local_update_archive() {
    SELECTED_UPDATE_ARCHIVE=""
    local candidates=()
    local base
    for base in "${LOCAL_UPDATE_SEARCH_PATHS[@]}"; do
        [[ -d "${base}" ]] || continue
        while IFS= read -r -d '' file; do
            candidates+=("${file}")
        done < <(find "${base}" -maxdepth 3 -type f -name 'rafael-linux-amd64.tar.gz' -print0 2>/dev/null)
    done

    if [[ "${#candidates[@]}" -eq 0 ]]; then
        read -rp "Archive not found automatically. Enter full path to rafael-linux-amd64.tar.gz: " manual_path
        [[ -f "${manual_path}" ]] || return 1
        SELECTED_UPDATE_ARCHIVE="${manual_path}"
        return 0
    fi

    local unique_candidates=()
    local seen="|"
    local item
    for item in "${candidates[@]}"; do
        if [[ "${seen}" == *"|${item}|"* ]]; then
            continue
        fi
        seen="${seen}${item}|"
        unique_candidates+=("${item}")
    done

    echo "Detected update archives:"
    local i=1
    local detected_version
    for item in "${unique_candidates[@]}"; do
        detected_version="$(extract_cli_build_version "${item}")"
        [[ -n "${detected_version}" ]] || detected_version="unknown"
        echo "  ${i}) ${item} [${detected_version}]"
        ((i++))
    done
    echo "  0) Enter path manually"
    read -rp "Select an archive: " selected
    if [[ "${selected}" == "0" ]]; then
        read -rp "Archive path: " manual_path
        [[ -f "${manual_path}" ]] || return 1
        SELECTED_UPDATE_ARCHIVE="${manual_path}"
        return 0
    fi
    if ! [[ "${selected}" =~ ^[0-9]+$ ]] || (( selected < 1 || selected > ${#unique_candidates[@]} )); then
        return 1
    fi
    SELECTED_UPDATE_ARCHIVE="${unique_candidates[selected-1]}"
}

extract_cli_build_version() {
    local archive_path="$1"
    if [[ ! -f "${archive_path}" ]]; then
        return 1
    fi
    tar -xOf "${archive_path}" rafael/rafael.sh 2>/dev/null | sed -n 's/\r$//; s/^CLI_BUILD_VERSION="\([^"]*\)"$/\1/p'
}

compare_build_versions() {
    local left="${1#v}"
    local right="${2#v}"
    if [[ ! "${left}" =~ ^[0-9]+$ || ! "${right}" =~ ^[0-9]+$ ]]; then
        [[ "$1" == "$2" ]] && echo 0 || echo -2
        return
    fi
    if (( left > right )); then
        echo 1
    elif (( left < right )); then
        echo -1
    else
        echo 0
    fi
}

# install_stable_cli installs the stable /usr/bin/rafael launcher when the
# release provides one, so a legacy flat update never clobbers the launcher
# that routes update/rollback/releases to the release manager.
install_stable_cli() {
    local src_dir="${1:-${APP_DIR}}"
    if [[ -f "${src_dir}/rafael-launcher.sh" ]]; then
        install -m 755 "${src_dir}/rafael-launcher.sh" /usr/bin/rafael 2>/dev/null || {
            cp -f "${src_dir}/rafael-launcher.sh" /usr/bin/rafael
            chmod +x /usr/bin/rafael
        }
    else
        cp -f "${src_dir}/rafael.sh" /usr/bin/rafael
        chmod +x /usr/bin/rafael
    fi
}

manual_update_panel() {
    local archive_path="${1:-}"
    if [[ -n "${archive_path}" ]]; then
        if [[ ! -f "${archive_path}" ]]; then
            echo "Archive path not found: ${archive_path}"
            return 1
        fi
        SELECTED_UPDATE_ARCHIVE="${archive_path}"
    else
        choose_local_update_archive || {
            echo "No valid archive selected."
            return 1
        }
    fi
    archive_path="${SELECTED_UPDATE_ARCHIVE}"

    # Release-layout archives (they bundle rafael-updater.sh) MUST NOT go
    # through the legacy flat overlay: it would install the archive's
    # current-based unit over a flat installation and break the service.
    # Refuse clearly and point at the safe command.
    if tar -tzf "${archive_path}" 2>/dev/null | grep -qx 'rafael/rafael-updater.sh'; then
        echo "Rejected: ${archive_path} is a release-layout archive."
        echo "The legacy 'update-local' flow must not be used for release-layout updates."
        echo "Use the safe, atomic release update instead:"
        echo "  rafael update ${archive_path}"
        return 1
    fi

    local detected_build current_build compare_result
    detected_build="$(extract_cli_build_version "${archive_path}")"
    current_build="${CLI_BUILD_VERSION}"
    [[ -n "${detected_build}" ]] || detected_build="unknown"

    echo "Current CLI build: ${current_build}"
    echo "Archive CLI build: ${detected_build}"
    compare_result="$(compare_build_versions "${detected_build}" "${current_build}")"
    case "${compare_result}" in
        1) echo "Archive build is newer than installed CLI." ;;
        0) echo "Archive build matches the installed CLI version." ;;
        -1) echo "Archive build is older than installed CLI." ;;
        *) echo "Archive build version could not be compared automatically." ;;
    esac

    read -rp "Apply this update now? [y/N]: " confirm_update
    if [[ "${confirm_update}" != "y" && "${confirm_update}" != "Y" ]]; then
        echo "Update cancelled."
        return 0
    fi

    local temp_dir backup_dir
    temp_dir="$(mktemp -d /tmp/rafael-update-XXXXXX)"
    backup_dir="${APP_DIR}.backup-$(date +%Y%m%d%H%M%S)"
    trap 'rm -rf "${temp_dir}"' RETURN

    tar -xzf "${archive_path}" -C "${temp_dir}"
    if [[ ! -f "${temp_dir}/rafael/rafael-panel" ]]; then
        echo "Invalid archive: rafael/rafael-panel not found."
        return 1
    fi
    chmod +x "${temp_dir}/rafael/rafael-panel" "${temp_dir}/rafael/rafael.sh" "${temp_dir}/rafael/bootstrap.sh" "${temp_dir}/rafael/bin/xray-linux-"* 2>/dev/null || true

    cp -a "${APP_DIR}" "${backup_dir}"
    run_systemctl stop rafael || true

    cp -rf "${temp_dir}/rafael/." "${APP_DIR}/"
    chmod +x "${APP_DIR}/rafael-panel" "${APP_DIR}/rafael.sh" "${APP_DIR}/bootstrap.sh" "${APP_DIR}/bin/xray-linux-"* 2>/dev/null || true
    cp -f "${APP_DIR}/rafael.service.debian" "/etc/systemd/system/rafael.service"
    install_stable_cli

    run_systemctl daemon-reload
    run_systemctl enable rafael >/dev/null 2>&1 || true
    if ! run_systemctl restart rafael; then
        echo "Update failed, restoring backup..."
        rm -rf "${APP_DIR}"
        mv -f "${backup_dir}" "${APP_DIR}"
        cp -f "${APP_DIR}/rafael.service.debian" "/etc/systemd/system/rafael.service"
        install_stable_cli
        run_systemctl daemon-reload
        run_systemctl restart rafael || true
        return 1
    fi

    echo "Update applied successfully."
    echo "Backup saved at: ${backup_dir}"
    return 0
}

parse_dsn_value() {
    local key="$1"
    load_env
    if [[ -z "${XUI_DB_DSN:-}" ]]; then
        return 0
    fi
    echo "${XUI_DB_DSN}" | sed -n "s/.*${key}=\\([^ ]*\\).*/\\1/p"
}

mask_dsn() {
    load_env
    if [[ -z "${XUI_DB_DSN:-}" ]]; then
        echo "(not set)"
        return
    fi
    echo "${XUI_DB_DSN}" | sed -E 's/password=[^ ]*/password=***/'
}

reset_ssl() {
    run_panel_cert -reset=true
    restart_panel_and_xray
    echo "Panel SSL cert/key reset."
}

set_ssl_paths() {
    local cert_file key_file
    read -rp "SSL cert file path: " cert_file
    read -rp "SSL key file path: " key_file
    run_panel_cert -webCert "${cert_file}" -webCertKey "${key_file}"
    restart_panel_and_xray
}

ssl_port_is_listening() {
    local port="$1"
    if command -v ss >/dev/null 2>&1; then
        ss -H -ltn "sport = :${port}" 2>/dev/null | grep -q .
        return $?
    fi
    if command -v netstat >/dev/null 2>&1; then
        netstat -lnt 2>/dev/null | awk -v suffix=":${port}" 'NR > 2 && $4 ~ (suffix "$" ) { found=1 } END { exit !found }'
        return $?
    fi
    return 1
}

# Run ACME while the Rafael service is stopped when its panel port or public
# HTTP port 80 is occupied by Rafael/Xray. The EXIT trap restores the service
# on success, failure, or Ctrl+C.
run_ssl_issue_with_optional_rafael_pause() {
    local pause_rafael="$1"
    local listener_port="$2"
    shift 2
    if [[ "${pause_rafael}" != "yes" ]]; then
        "$@"
        return $?
    fi

    (
        local rafael_was_stopped=0
        restore_rafael_after_ssl_issue() {
            local result=$?
            trap - EXIT
            if (( rafael_was_stopped )); then
                if ! systemctl start rafael; then
                    echo "Could not restart the Rafael service after SSL issuance; start it with: systemctl start rafael" >&2
                    result=1
                fi
            fi
            exit "${result}"
        }
        trap restore_rafael_after_ssl_issue EXIT

        rafael_was_stopped=1
        if ! systemctl stop rafael; then
            echo "Could not stop the Rafael service for HTTP-01 validation." >&2
            exit 1
        fi
        sleep 1
        if ssl_port_is_listening "${listener_port}"; then
            echo "Port ${listener_port} is still occupied after stopping Rafael." >&2
            exit 1
        fi
        "$@"
    )
}

issue_ssl_certificate() {
    local identifier http_port acme_home acme_bin installer cert_id cert_dir cert_file key_file is_ip apply_now
    local panel_settings panel_domain panel_port panel_base_path domain_choice panel_url_host panel_url_path panel_url

    if (( EUID != 0 )); then
        echo "Run the Rafael CLI as root to issue and install a panel certificate."
        return 1
    fi

    if ! panel_settings="$(run_panel_setting -show true)"; then
        echo "Could not read the current Rafael panel settings."
        return 1
    fi
    panel_domain="$(printf '%s\n' "${panel_settings}" | sed -n 's/^webDomain:[[:space:]]*//p' | head -n 1)"
    panel_port="$(printf '%s\n' "${panel_settings}" | sed -n 's/^port:[[:space:]]*//p' | head -n 1)"
    panel_base_path="$(printf '%s\n' "${panel_settings}" | sed -n 's/^webBasePath:[[:space:]]*//p' | head -n 1)"
    if [[ ! "${panel_port}" =~ ^[0-9]+$ ]] || (( panel_port < 1 || panel_port > 65535 )); then
        echo "Could not read a valid current panel port from the settings."
        return 1
    fi

    if [[ -n "${panel_domain}" ]]; then
        echo "Detected panel domain: ${panel_domain}"
        echo "1) Use the configured panel domain"
        echo "2) Enter another domain or public IP"
        read -rp "Select a domain [1]: " domain_choice
        case "${domain_choice:-1}" in
            1) identifier="${panel_domain}" ;;
            2) read -rp "Domain or public IP address: " identifier ;;
            *) echo "Invalid domain selection."; return 1 ;;
        esac
    else
        read -rp "Domain or public IP address: " identifier
    fi
    identifier="${identifier,,}"
    panel_domain="${panel_domain,,}"
    if [[ -z "${identifier}" || ! "${identifier}" =~ ^[A-Za-z0-9][A-Za-z0-9.:-]*$ ]]; then
        echo "Enter a valid domain name or IPv4/IPv6 address."
        return 1
    fi

    is_ip="false"
    if command -v python3 >/dev/null 2>&1; then
        if python3 - "${identifier}" <<'PY'
import ipaddress
import sys
try:
    ipaddress.ip_address(sys.argv[1])
except ValueError:
    sys.exit(1)
PY
        then
            is_ip="true"
        fi
    elif [[ "${identifier}" == *:* || "${identifier}" =~ ^[0-9.]+$ ]]; then
        is_ip="true"
    fi

    read -rp "Standalone HTTP listener port [80]: " http_port
    http_port="${http_port:-80}"
    if [[ ! "${http_port}" =~ ^[0-9]+$ ]] || (( http_port < 1 || http_port > 65535 )); then
        echo "Enter a TCP port between 1 and 65535."
        return 1
    fi
    if [[ "${http_port}" == "80" ]]; then
        echo "Let's Encrypt must reach this server on public TCP port 80."
    else
        echo "The selected port ${http_port} is the local ACME listener port. Forward or proxy public TCP port 80 to ${http_port}; entering a different port alone does not change Let's Encrypt's public validation port."
    fi

    panel_url_host="${identifier}"
    if [[ "${panel_url_host}" == *:* && "${panel_url_host}" != \[*\] ]]; then
        panel_url_host="[${panel_url_host}]"
    fi
    panel_url_path="${panel_base_path:-/}"
    [[ "${panel_url_path}" == /* ]] || panel_url_path="/${panel_url_path}"
    if [[ "${panel_url_path}" != "/" && "${panel_url_path}" != */ ]]; then
        panel_url_path="${panel_url_path}/"
    fi
    panel_url="https://${panel_url_host}:${panel_port}${panel_url_path}"
    echo "The panel will keep its configured port ${panel_port}; the ACME listener port is separate."
    echo "Panel URL after applying SSL: ${panel_url}"

    local pause_rafael="no"
    if ssl_port_is_listening "${http_port}"; then
        if { [[ "${http_port}" == "80" ]] || [[ "${http_port}" == "${panel_port}" ]]; } && command -v systemctl >/dev/null 2>&1 && systemctl is-active --quiet rafael; then
            echo "The selected listener port is currently occupied and may belong to Rafael/Xray."
            read -rp "Temporarily stop/restart Rafael and Xray for issuance? Active connections will be interrupted. [y/N]: " pause_answer
            case "${pause_answer,,}" in
                y|yes) pause_rafael="yes" ;;
                *) echo "SSL issuance cancelled; Rafael was not stopped."; return 1 ;;
            esac
        else
            echo "The selected local listener port ${http_port} is already in use. Choose a free port."
            if command -v ss >/dev/null 2>&1; then
                ss -ltnp "sport = :${http_port}" 2>/dev/null || true
            fi
            return 1
        fi
    fi

    if ! command -v apt-get >/dev/null 2>&1; then
        echo "This automated setup currently requires a Debian/Ubuntu server with apt-get."
        return 1
    fi
    if ! DEBIAN_FRONTEND=noninteractive apt-get update; then
        echo "Could not refresh apt package indexes."
        return 1
    fi
    if ! DEBIAN_FRONTEND=noninteractive apt-get install -y curl socat; then
        echo "Could not install curl and socat."
        return 1
    fi

    acme_home="${HOME:-/root}/.acme.sh"
    acme_bin="${acme_home}/acme.sh"
    if [[ ! -x "${acme_bin}" ]]; then
        installer="$(mktemp)" || return 1
        if ! curl -fsSL "https://get.acme.sh" -o "${installer}"; then
            rm -f "${installer}"
            echo "Could not download the acme.sh installer."
            return 1
        fi
        if ! sh "${installer}"; then
            rm -f "${installer}"
            echo "acme.sh installation failed."
            return 1
        fi
        rm -f "${installer}"
    fi
    if [[ ! -x "${acme_bin}" ]]; then
        echo "acme.sh was installed, but ${acme_bin} was not found."
        return 1
    fi

    if ! "${acme_bin}" --set-default-ca --server letsencrypt; then
        echo "Could not select Let's Encrypt as the default certificate authority."
        return 1
    fi
    if [[ "${is_ip}" == "true" ]]; then
        if ! run_ssl_issue_with_optional_rafael_pause "${pause_rafael}" "${http_port}" "${acme_bin}" --issue -d "${identifier}" --standalone --httpport "${http_port}" --server letsencrypt --certificate-profile shortlived --days 6; then
            echo "IP certificate issuance failed. Ensure public TCP port 80 reaches the local listener on ${http_port}, and check IP/DNS routing."
            return 1
        fi
    else
        if ! run_ssl_issue_with_optional_rafael_pause "${pause_rafael}" "${http_port}" "${acme_bin}" --issue -d "${identifier}" --standalone --httpport "${http_port}" --server letsencrypt; then
            echo "Domain certificate issuance failed. Ensure the domain resolves to this server and public TCP port 80 reaches the local listener on ${http_port}."
            return 1
        fi
    fi

    cert_id="${identifier//[^A-Za-z0-9._-]/_}"
    cert_dir="/etc/rafael/ssl/${cert_id}"
    cert_file="${cert_dir}/fullchain.pem"
    key_file="${cert_dir}/private.key"
    if ! install -d -m 700 "${cert_dir}"; then
        echo "Could not create the Rafael certificate directory: ${cert_dir}"
        return 1
    fi
    if ! "${acme_bin}" --install-cert -d "${identifier}" --key-file "${key_file}" --fullchain-file "${cert_file}" --reloadcmd "systemctl restart rafael"; then
        echo "Certificate was issued, but could not be installed to the Rafael certificate directory."
        return 1
    fi
    chmod 600 "${key_file}"
    chmod 644 "${cert_file}"

    echo "Certificate and key are saved for renewal at:"
    echo "  Certificate: ${cert_file}"
    echo "  Private key: ${key_file}"
    read -rp "Set this certificate for the Rafael panel now? [Y/n]: " apply_now
    case "${apply_now,,}" in
        n|no)
            echo "Certificate saved. Use SSL option 12 to apply it to the panel later."
            ;;
        *)
            if ! run_panel_cert -webCert "${cert_file}" -webCertKey "${key_file}"; then
                echo "Certificate is saved, but the panel settings could not be updated."
                return 1
            fi
            if [[ "${identifier,,}" != "${panel_domain,,}" ]]; then
                if ! run_panel_setting -webDomain "${identifier}"; then
                    echo "Certificate is saved, but the panel domain could not be updated."
                    return 1
                fi
            fi
            if ! restart_panel_and_xray; then
                echo "Certificate is configured, but the Rafael service could not be restarted."
                return 1
            fi
            echo "SSL certificate applied to the Rafael panel. Renewal will restart the service automatically."
            echo "Open the panel at: ${panel_url}"
            ;;
    esac
}

set_ssl_menu() {
    local choice
    while true; do
        clear
        echo "========== Panel SSL =========="
        echo "1) Issue Let's Encrypt certificate for a domain or public IP"
        echo "2) Set existing certificate/key paths"
        echo "0) Back"
        echo
        read -rp "Select an option: " choice
        case "${choice}" in
            1) issue_ssl_certificate; pause_screen ;;
            2) set_ssl_paths; return ;;
            0) return ;;
            *) echo "Invalid option"; pause_screen ;;
        esac
    done
}

uninstall_panel() {
    local db_name db_user
    db_name="$(parse_dsn_value "dbname")"
    db_user="$(parse_dsn_value "user")"

    echo "This will remove Rafael panel files, service, and environment file."
    read -rp "Also drop PostgreSQL database and user? [y/N]: " drop_db

    run_systemctl stop rafael || true
    run_systemctl disable rafael || true
    rm -f "/etc/systemd/system/rafael.service"
    run_systemctl daemon-reload || true

    rm -rf "${ROOT_DIR}"
    rm -f /usr/bin/rafael
    rm -f "${ENV_FILE}"

    if [[ "${drop_db}" == "y" || "${drop_db}" == "Y" ]]; then
        if [[ -n "${db_name}" ]]; then
            sudo -u postgres psql -c "DROP DATABASE IF EXISTS ${db_name};" || true
        fi
        if [[ -n "${db_user}" ]]; then
            sudo -u postgres psql -c "DROP USER IF EXISTS ${db_user};" || true
        fi
    fi

    echo "Rafael panel uninstalled."
}

pause_screen() {
    echo
    read -rp "Press Enter to continue..." _
}

show_help() {
    cat <<EOF
Usage: rafael <command>

Commands:
  start        Start service
  stop         Stop service
  restart      Restart service
  status       Show service status
  enable       Enable auto-start
  disable      Disable auto-start
  log          Follow service logs
  env          Show environment file (passwords masked)
  run          Run binary in foreground
  releases     List installed releases and the active one
  update       Release-based update from a local tar.gz (safe, atomic)
  update-local Legacy flat-layout update from a local tar.gz
  rollback     Roll back to the previous release
  uninstall    Remove panel files and service
EOF
}

show_menu() {
    local panel_version
    while true; do
        panel_version="$(get_panel_version)"
        clear
        echo "=============================="
        echo " Rafael Panel CLI ${CLI_BUILD_VERSION}"
        echo " Panel Version: ${panel_version}"
        echo "=============================="
        echo "1) Status"
        echo "2) Start"
        echo "3) Stop"
        echo "4) Restart"
        echo "5) Logs"
        echo "6) Show env (masked)"
        echo "7) Show panel settings"
        echo "8) Change panel port"
        echo "9) Change username/password"
        echo "10) Change base path"
        echo "11) Change listen IP"
        echo "12) Set SSL cert/key"
        echo "13) Reset panel SSL cert/key"
        echo "14) Enable auto-start"
        echo "15) Disable auto-start"
        echo "16) Safe release update from local tar.gz"
        echo "17) Uninstall panel"
        echo "0) Exit"
        echo
        read -rp "Select an option: " choice
        case "${choice}" in
            1) run_systemctl status rafael -l --no-pager ;;
            2) run_systemctl start rafael ;;
            3) run_systemctl stop rafael ;;
            4) run_systemctl restart rafael ;;
            5) journalctl -u rafael -n 100 --no-pager ;;
            6) mask_dsn ;;
            7) run_panel_setting -show true ;;
            8)
                read -rp "New panel port: " port
                [[ -n "${port}" ]] && run_panel_setting -port "${port}"
                restart_panel_and_xray
                ;;
            9)
                read -rp "New username: " username
                read -rsp "New password: " password
                echo
                if [[ -n "${username}" && -n "${password}" ]]; then
                    run_panel_setting -username "${username}" -password "${password}"
                    restart_panel_and_xray
                fi
                ;;
            10)
                read -rp "New base path (example: mypanel): " base_path
                run_panel_setting -webBasePath "${base_path}"
                restart_panel_and_xray
                ;;
            11)
                read -rp "New listen IP (example: 0.0.0.0): " listen_ip
                run_panel_setting -listenIP "${listen_ip}"
                restart_panel_and_xray
                ;;
            12) set_ssl_menu ;;
            13) reset_ssl ;;
            14) run_systemctl enable rafael ;;
            15) run_systemctl disable rafael ;;
            16) update_panel ;;
            17) uninstall_panel; exit 0 ;;
            0) exit 0 ;;
            *) echo "Invalid option" ;;
        esac
        pause_screen
    done
}

cmd="${1:-menu}"
case "${cmd}" in
    start)
        run_systemctl start rafael
        ;;
    stop)
        run_systemctl stop rafael
        ;;
    restart)
        run_systemctl restart rafael
        ;;
    status)
        run_systemctl status rafael -l --no-pager
        ;;
    enable)
        run_systemctl enable rafael
        ;;
    disable)
        run_systemctl disable rafael
        ;;
    log)
        journalctl -u rafael -e --no-pager -f
        ;;
    env)
        if [[ -f "${ENV_FILE}" ]]; then
            mask_dsn
            echo "(full file at ${ENV_FILE}; secrets are masked here)"
        else
            echo "Environment file not found: ${ENV_FILE}"
            exit 1
        fi
        ;;
    run)
        load_env
        exec "${APP_DIR}/rafael-panel"
        ;;
    releases)
        show_releases
        ;;
    update)
        update_panel "${2:-}"
        ;;
    update-local)
        manual_update_panel "${2:-}"
        ;;
    rollback)
        rollback_panel "${2:-}"
        ;;
    uninstall)
        uninstall_panel
        ;;
    menu)
        show_menu
        ;;
    *)
        show_help
        ;;
esac

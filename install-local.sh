#!/bin/bash

set -euo pipefail

APP_NAME="rafael"
APP_DIR="/usr/local/rafael"
SERVICE_FILE="/etc/systemd/system/rafael.service"
ENV_FILE="/etc/default/rafael"
DB_NAME="${RAFAEL_DB_NAME:=rafael_panel}"
DB_USER="${RAFAEL_DB_USER:=rafael}"
DB_PASS="${RAFAEL_DB_PASS:-$(openssl rand -hex 16)}"
GEOIP_URL="https://github.com/Loyalsoldier/v2ray-rules-dat/releases/latest/download/geoip.dat"
GEOSITE_URL="https://github.com/Loyalsoldier/v2ray-rules-dat/releases/latest/download/geosite.dat"
PANEL_PORT=""
PANEL_HOST=""
PANEL_WEB_PATH=""
PANEL_SCHEME="http"
ADMIN_USERNAME=""
ADMIN_PASSWORD=""
PANEL_URL=""
CERT_FILE=""
KEY_FILE=""
ACME_BIN=""
AUTO_SSL="false"

require_root() {
    if [[ "${EUID}" -ne 0 ]]; then
        echo "Please run as root."
        exit 1
    fi
}

install_packages() {
    apt-get update
    apt-get install -y ca-certificates curl openssl postgresql postgresql-contrib tar unzip iproute2 python3-minimal socat
}

ensure_fresh_install() {
    if [[ -e "${SERVICE_FILE}" || -e "${APP_DIR}/rafael-panel" || -e "${APP_DIR}/current" || -e "${ENV_FILE}" ]]; then
        echo "A Rafael install or configuration already exists. Use 'rafael update' for upgrades; refusing to overwrite it."
        exit 1
    fi
}

validate_database_names() {
    [[ "${DB_USER}" =~ ^[A-Za-z_][A-Za-z0-9_]{0,62}$ ]] || {
        echo "RAFAEL_DB_USER must be a PostgreSQL identifier (letters, numbers, underscores; max 63 characters)." >&2
        exit 1
    }
    [[ "${DB_NAME}" =~ ^[A-Za-z_][A-Za-z0-9_]{0,62}$ ]] || {
        echo "RAFAEL_DB_NAME must be a PostgreSQL identifier (letters, numbers, underscores; max 63 characters)." >&2
        exit 1
    }
    [[ "${DB_PASS}" =~ ^[A-Fa-f0-9]{16,128}$ ]] || {
        echo "RAFAEL_DB_PASS must be 16-128 hexadecimal characters." >&2
        exit 1
    }
}

load_existing_env() {
    if [[ -f "${ENV_FILE}" ]]; then
        # shellcheck disable=SC1090
        source "${ENV_FILE}"
        if [[ -n "${XUI_DB_DSN:-}" ]]; then
            local existing_user
            local existing_db
            local existing_pass
            existing_user="$(echo "${XUI_DB_DSN}" | sed -n 's/.*user=\([^ ]*\).*/\1/p')"
            existing_db="$(echo "${XUI_DB_DSN}" | sed -n 's/.*dbname=\([^ ]*\).*/\1/p')"
            existing_pass="$(echo "${XUI_DB_DSN}" | sed -n 's/.*password=\([^ ]*\).*/\1/p')"
            DB_USER="${RAFAEL_DB_USER:-${existing_user:-$DB_USER}}"
            DB_NAME="${RAFAEL_DB_NAME:-${existing_db:-$DB_NAME}}"
            DB_PASS="${RAFAEL_DB_PASS:-${existing_pass:-$DB_PASS}}"
        fi
    fi
}

run_psql() {
    su - postgres -c "$1"
}

setup_postgres() {
    systemctl enable postgresql
    systemctl start postgresql

    run_psql "psql -tc \"SELECT 1 FROM pg_roles WHERE rolname='${DB_USER}'\"" | grep -q 1 || \
        run_psql "psql -c \"CREATE USER ${DB_USER} WITH PASSWORD '${DB_PASS}';\""

    run_psql "psql -tc \"SELECT 1 FROM pg_database WHERE datname='${DB_NAME}'\"" | grep -q 1 || \
        run_psql "psql -c \"CREATE DATABASE ${DB_NAME} OWNER ${DB_USER};\""

    run_psql "psql -c \"GRANT ALL PRIVILEGES ON DATABASE ${DB_NAME} TO ${DB_USER};\""
}

download_geofiles() {
    local bin_dir="${APP_DIR}/bin"
    mkdir -p "${bin_dir}"
    [[ -f "${bin_dir}/geoip.dat" ]] || curl -fL "${GEOIP_URL}" -o "${bin_dir}/geoip.dat"
    [[ -f "${bin_dir}/geosite.dat" ]] || curl -fL "${GEOSITE_URL}" -o "${bin_dir}/geosite.dat"
}

install_files() {
    mkdir -p "${APP_DIR}"
    cp -rf ./* "${APP_DIR}/"
    mkdir -p "${APP_DIR}/bin"

    if [[ ! -s "${APP_DIR}/bin/xray-linux-amd64" ]]; then
        echo "The Rafael IP-limit Xray binary is missing from this release archive; refusing to install stock Xray." >&2
        exit 1
    fi
    chmod 755 "${APP_DIR}/bin/xray-linux-amd64"

    if [[ -f "${APP_DIR}/rafael-panel-linux-amd64" ]]; then
        mv -f "${APP_DIR}/rafael-panel-linux-amd64" "${APP_DIR}/rafael-panel"
    fi

    chmod +x "${APP_DIR}/rafael-panel" "${APP_DIR}/rafael.sh" "${APP_DIR}/bootstrap.sh"

    # Stable layer: release manager + launcher live OUTSIDE the release tree
    # so a broken current release can never prevent update or rollback.
    # A fresh install is FLAT (APP_DIR == /usr/local/rafael), so the updater
    # copied above is already at its stable-layer home; only the mode needs
    # fixing. (install(1) refuses source == destination, which aborted the
    # script here on every flat install.)
    if [[ -f "${APP_DIR}/rafael-updater.sh" ]]; then
        chmod 755 "${APP_DIR}/rafael-updater.sh"
    fi
    if [[ -f "${APP_DIR}/rafael-launcher.sh" ]]; then
        install -m 755 "${APP_DIR}/rafael-launcher.sh" /usr/bin/rafael
    else
        cp -f "${APP_DIR}/rafael.sh" /usr/bin/rafael
        chmod +x /usr/bin/rafael
    fi

    # A FRESH install is flat (no releases/ tree, no current symlink yet), so
    # the unit must reference the flat paths directly. The shipped
    # rafael.service.debian is current-based (release layout); derive the flat
    # variant from it. The first `rafael update` migrates to the release
    # layout and installs the current-based unit.
    sed 's|/usr/local/rafael/current|/usr/local/rafael|g' \
        "${APP_DIR}/rafael.service.debian" >"${SERVICE_FILE}"
    chmod 644 "${SERVICE_FILE}"
}

write_env() {
    local tmp="${ENV_FILE}.tmp.$$"
    {
        echo "XUI_BIN_FOLDER=${APP_DIR}/bin"
        echo "XUI_DB_TYPE=postgres"
        printf 'XUI_DB_DSN="host=127.0.0.1 user=%s password=%s dbname=%s port=5432 sslmode=disable TimeZone=Asia/Tehran"\n' "${DB_USER}" "${DB_PASS}" "${DB_NAME}"
        # Preserve operator-added variables on reinstalls.
        if [[ -f "${ENV_FILE}" ]]; then
            grep -vE '^(XUI_BIN_FOLDER|XUI_DB_TYPE|XUI_DB_DSN)=' "${ENV_FILE}" || true
        fi
    } >"${tmp}"
    chmod 600 "${tmp}"
    mv -f "${tmp}" "${ENV_FILE}"
}

run_bootstrap() {
    sed -i 's/\r$//' "${APP_DIR}/bootstrap.sh" "${APP_DIR}/rafael.sh" 2>/dev/null || true
    /bin/bash "${APP_DIR}/bootstrap.sh"
}

port_is_available() {
    local port="$1"
    ! ss -H -ltn "sport = :${port}" 2>/dev/null | grep -q .
}

random_panel_port() {
    local attempt candidate
    for attempt in {1..100}; do
        candidate="$((20000 + $(od -An -N4 -tu4 /dev/urandom | tr -d ' ') % 40000))"
        if port_is_available "${candidate}"; then
            printf '%s\n' "${candidate}"
            return 0
        fi
    done
    echo "Could not find an unused random panel port." >&2
    return 1
}

valid_port() {
    [[ "$1" =~ ^[0-9]{1,5}$ ]] && (( 10#$1 >= 1 && 10#$1 <= 65535 ))
}

normalize_host() {
    local value="$1"
    python3 - "$value" <<'PY'
import ipaddress
import re
import sys

value = sys.argv[1].strip().rstrip(".").lower()
try:
    print(ipaddress.ip_address(value).compressed)
    raise SystemExit(0)
except ValueError:
    pass
if len(value) > 253 or not value:
    raise SystemExit(1)
labels = value.split(".")
if any(not re.fullmatch(r"[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?", label) for label in labels):
    raise SystemExit(1)
print(value)
PY
}

host_is_ip() {
    python3 - "$1" <<'PY'
import ipaddress
import sys
try:
    ipaddress.ip_address(sys.argv[1])
except ValueError:
    raise SystemExit(1)
PY
}

choose_panel_port() {
    local requested
    local default_port
    default_port="$(random_panel_port)"
    while true; do
        read -rp "Panel TCP port [${default_port}]: " requested
        requested="${requested:-${default_port}}"
        if ! valid_port "${requested}"; then
            echo "Enter a port between 1 and 65535."
            continue
        fi
        if ! port_is_available "${requested}"; then
            echo "TCP port ${requested} is already in use. Choose another one."
            continue
        fi
        PANEL_PORT="$((10#${requested}))"
        return
    done
}

choose_panel_host() {
    local requested normalized
    while true; do
        read -rp "Primary domain or public IP for this panel (blank to configure later): " requested
        [[ -z "${requested}" ]] && { PANEL_HOST=""; return; }
        if normalized="$(normalize_host "${requested}")"; then
            PANEL_HOST="${normalized}"
            return
        fi
        echo "Enter only a domain or IP address, without scheme, port, or path."
    done
}

make_panel_url() {
    local host="$1"
    local scheme="$2"
    if [[ -z "${host}" ]]; then
        host="SERVER_IP"
    elif [[ "${host}" == *:* && "${host}" != \[*\] ]]; then
        host="[${host}]"
    fi
    printf '%s://%s:%s/%s/\n' "${scheme}" "${host}" "${PANEL_PORT}" "${PANEL_WEB_PATH}"
}

choose_admin_credentials() {
    local generated_user generated_password requested_user requested_password
    generated_user="rafael-$(openssl rand -hex 3)"
    generated_password="$(openssl rand -hex 18)"
    read -rp "Admin username [${generated_user}]: " requested_user
    ADMIN_USERNAME="${requested_user:-${generated_user}}"
    if [[ ! "${ADMIN_USERNAME}" =~ ^[A-Za-z0-9_.-]{3,32}$ ]]; then
        echo "Admin username must be 3-32 characters: letters, numbers, dot, underscore, or hyphen." >&2
        return 1
    fi
    read -rsp "Admin password [press Enter for a random password]: " requested_password
    echo
    if [[ -z "${requested_password}" ]]; then
        ADMIN_PASSWORD="${generated_password}"
    else
        if (( ${#requested_password} < 12 || ${#requested_password} > 128 )); then
            echo "Choose a password between 12 and 128 characters, or press Enter for a random one." >&2
            return 1
        fi
        ADMIN_PASSWORD="${requested_password}"
    fi
}

install_acme_certificate() {
    local acme_port requested_port acme_home acme_bin installer cert_id cert_dir
    local acme_args=()

    if [[ -z "${PANEL_HOST}" ]]; then
        choose_panel_host
        [[ -n "${PANEL_HOST}" ]] || { echo "A domain or public IP is required for automatic SSL." >&2; return 1; }
    fi

    read -rp "Local HTTP-01 listener port [80]: " requested_port
    acme_port="${requested_port:-80}"
    if ! valid_port "${acme_port}"; then
        echo "Enter a port between 1 and 65535." >&2
        return 1
    fi
    acme_port="$((10#${acme_port}))"
    if (( acme_port == PANEL_PORT )); then
        echo "The ACME listener and panel need different ports so certificate renewal remains available." >&2
        return 1
    fi
    if ! port_is_available "${acme_port}"; then
        echo "TCP port ${acme_port} is already in use. Choose an available ACME listener port." >&2
        return 1
    fi
    echo "Let's Encrypt must reach this server on public TCP port 80. If the local listener is not port 80, forward public port 80 to ${acme_port}."
    read -rp "Continue with automatic certificate issuance? [y/N]: " confirm
    case "${confirm,,}" in y|yes) ;; *) echo "Automatic SSL setup cancelled."; return 1 ;; esac

    export HOME=/root
    acme_home="/root/.acme.sh"
    acme_bin="${acme_home}/acme.sh"
    if [[ ! -x "${acme_bin}" ]]; then
        installer="$(mktemp)"
        if ! curl -fsSL "https://get.acme.sh" -o "${installer}"; then
            rm -f "${installer}"
            echo "Could not download acme.sh from its official HTTPS endpoint." >&2
            return 1
        fi
        if ! sh "${installer}"; then
            rm -f "${installer}"
            echo "acme.sh installation failed." >&2
            return 1
        fi
        rm -f "${installer}"
    fi
    [[ -x "${acme_bin}" ]] || { echo "acme.sh installation did not create ${acme_bin}." >&2; return 1; }
    "${acme_bin}" --set-default-ca --server letsencrypt || { echo "Could not select Let's Encrypt." >&2; return 1; }
    acme_args=(--issue -d "${PANEL_HOST}" --standalone --httpport "${acme_port}" --server letsencrypt)
    if host_is_ip "${PANEL_HOST}"; then
        acme_args+=(--certificate-profile shortlived --days 6)
    fi
    "${acme_bin}" "${acme_args[@]}" || { echo "Let's Encrypt could not issue a certificate. Check DNS and public port 80 routing." >&2; return 1; }

    cert_id="$(printf '%s' "${PANEL_HOST}" | tr -c 'A-Za-z0-9._-' '_')"
    cert_dir="/etc/rafael/ssl/${cert_id}"
    install -d -m 700 "${cert_dir}"
    CERT_FILE="${cert_dir}/fullchain.pem"
    KEY_FILE="${cert_dir}/private.key"
    "${acme_bin}" --install-cert -d "${PANEL_HOST}" --key-file "${KEY_FILE}" --fullchain-file "${CERT_FILE}" || { echo "Could not install the issued certificate." >&2; return 1; }
    chmod 600 "${KEY_FILE}"
    chmod 644 "${CERT_FILE}"
    ACME_BIN="${acme_bin}"
    AUTO_SSL="true"
    PANEL_SCHEME="https"
}

install_custom_certificate() {
    local source_cert source_key cert_public key_public cert_id cert_dir
    if [[ -z "${PANEL_HOST}" ]]; then
        choose_panel_host
    fi
    read -rp "Path to the certificate/fullchain PEM: " source_cert
    read -rp "Path to the private key PEM: " source_key
    [[ -s "${source_cert}" && -s "${source_key}" ]] || { echo "Certificate and key files must both exist and be non-empty." >&2; return 1; }
    openssl x509 -in "${source_cert}" -noout -checkend 0 >/dev/null || { echo "The certificate is invalid or expired." >&2; return 1; }
    cert_public="$(openssl x509 -in "${source_cert}" -pubkey -noout | openssl pkey -pubin -outform DER | sha256sum | awk '{print $1}')" || { echo "Could not read the certificate public key." >&2; return 1; }
    key_public="$(openssl pkey -in "${source_key}" -pubout -outform DER | sha256sum | awk '{print $1}')" || { echo "Could not read the private key." >&2; return 1; }
    [[ "${cert_public}" == "${key_public}" ]] || { echo "The certificate and private key do not match." >&2; return 1; }
    if [[ -n "${PANEL_HOST}" ]]; then
        if host_is_ip "${PANEL_HOST}"; then
            openssl x509 -in "${source_cert}" -noout -checkip "${PANEL_HOST}" >/dev/null || { echo "The certificate is not valid for ${PANEL_HOST}." >&2; return 1; }
        else
            openssl x509 -in "${source_cert}" -noout -checkhost "${PANEL_HOST}" >/dev/null || { echo "The certificate is not valid for ${PANEL_HOST}." >&2; return 1; }
        fi
    fi
    cert_id="$(printf '%s' "${PANEL_HOST:-custom}" | tr -c 'A-Za-z0-9._-' '_')"
    cert_dir="/etc/rafael/ssl/${cert_id}"
    install -d -m 700 "${cert_dir}"
    CERT_FILE="${cert_dir}/fullchain.pem"
    KEY_FILE="${cert_dir}/private.key"
    install -m 644 "${source_cert}" "${CERT_FILE}"
    install -m 600 "${source_key}" "${KEY_FILE}"
    PANEL_SCHEME="https"
}

configure_panel() {
    local ssl_choice settings_output setting_args=()
    echo
    echo "Rafael Panel first-install setup"
    choose_panel_port
    choose_panel_host
    PANEL_WEB_PATH="$(openssl rand -hex 16)"
    choose_admin_credentials

    echo
    echo "SSL options:"
    echo "  1) Let's Encrypt certificate for the configured domain/IP"
    echo "  2) Use an existing certificate and private key"
    echo "  3) HTTP only (no SSL)"
    read -rp "Choose SSL setup [3]: " ssl_choice
    case "${ssl_choice:-3}" in
        1)
            if ! install_acme_certificate; then
                read -rp "SSL setup failed. Continue with HTTP only? [y/N]: " continue_http
                case "${continue_http,,}" in y|yes) PANEL_SCHEME="http"; CERT_FILE=""; KEY_FILE="" ;; *) return 1 ;; esac
            fi
            ;;
        2)
            install_custom_certificate || return 1
            ;;
        3) PANEL_SCHEME="http" ;;
        *) echo "Invalid SSL option." >&2; return 1 ;;
    esac

    setting_args=(-port "${PANEL_PORT}" -username "${ADMIN_USERNAME}" -password "${ADMIN_PASSWORD}" -webBasePath "${PANEL_WEB_PATH}")
    [[ -z "${PANEL_HOST}" ]] || setting_args+=(-webDomain "${PANEL_HOST}")
    "${APP_DIR}/rafael-panel" setting "${setting_args[@]}"
    if [[ -n "${CERT_FILE}" && -n "${KEY_FILE}" ]]; then
        "${APP_DIR}/rafael-panel" cert -webCert "${CERT_FILE}" -webCertKey "${KEY_FILE}"
    fi

    settings_output="$("${APP_DIR}/rafael-panel" setting -show true)"
    printf '%s\n' "${settings_output}" | grep -Fxq "port: ${PANEL_PORT}" || { echo "Panel port could not be saved to the database." >&2; return 1; }
    printf '%s\n' "${settings_output}" | grep -Fxq "webBasePath: /${PANEL_WEB_PATH}/" || { echo "Panel web path could not be saved to the database." >&2; return 1; }
    if [[ -n "${PANEL_HOST}" ]]; then
        printf '%s\n' "${settings_output}" | grep -Fxq "webDomain: ${PANEL_HOST}" || { echo "Panel domain could not be saved to the database." >&2; return 1; }
    fi
    if [[ -n "${CERT_FILE}" ]]; then
        printf '%s\n' "${settings_output}" | grep -Fq "Panel is secure with SSL" || { echo "SSL certificate paths could not be saved to the database." >&2; return 1; }
    fi
    PANEL_URL="$(make_panel_url "${PANEL_HOST}" "${PANEL_SCHEME}")"
}

start_service() {
    systemctl daemon-reload
    systemctl enable rafael
    systemctl start rafael
    if ! systemctl is-active --quiet rafael; then
        echo "Rafael service did not start. Recent logs:" >&2
        journalctl -u rafael -n 30 --no-pager >&2 || true
        return 1
    fi
}

configure_acme_renewal() {
    if [[ "${AUTO_SSL}" == "true" ]]; then
        "${ACME_BIN}" --install-cert -d "${PANEL_HOST}" --key-file "${KEY_FILE}" --fullchain-file "${CERT_FILE}" --reloadcmd "systemctl restart rafael" || {
            echo "The panel is running, but the certificate renewal restart hook could not be saved." >&2
            echo "Re-run the Rafael SSL setup after checking /root/.acme.sh/acme.sh." >&2
            return 1
        }
    fi
}

print_result() {
    echo
    echo "Rafael panel installed."
    echo "Service: rafael"
    echo "App dir: ${APP_DIR}"
    echo "Env file: ${ENV_FILE}"
    echo "Database: ${DB_NAME}"
    echo "Database user: ${DB_USER}"
    echo "Panel URL: ${PANEL_URL}"
    echo "Panel port: ${PANEL_PORT}"
    echo "Web path: /${PANEL_WEB_PATH}/"
    echo "Admin username: ${ADMIN_USERNAME}"
    echo "Admin password: ${ADMIN_PASSWORD}"
    echo "Open TCP port ${PANEL_PORT} in the server firewall."
    if [[ "${PANEL_SCHEME}" == "http" ]]; then
        echo "SSL is disabled. You can configure it later from the Rafael CLI."
    fi
    echo
    echo "Commands:"
    echo "  rafael status"
    echo "  rafael log"
    echo "  rafael restart"
}

require_root
ensure_fresh_install
validate_database_names
load_existing_env
install_packages
install_files
write_env
run_bootstrap
configure_panel
start_service
configure_acme_renewal
print_result

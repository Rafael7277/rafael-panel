#!/bin/bash

set -euo pipefail

# Release-aware bootstrap. Resolves the application directory from this
# script's own location (works flat in /usr/local/rafael AND under the
# versioned release layout /usr/local/rafael/releases/vNN, whether invoked
# directly or through the /usr/local/rafael/current symlink).
SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
APP_DIR="${RAFAEL_MAIN_FOLDER:-${SCRIPT_DIR}}"
ENV_FILE="/etc/default/rafael"
DB_NAME="${RAFAEL_DB_NAME:=rafael_panel}"
DB_USER="${RAFAEL_DB_USER:=rafael}"
DB_PASS="${RAFAEL_DB_PASS:=}"
XRAY_BIN="${APP_DIR}/bin/xray-linux-amd64"
GEOIP_FILE="${APP_DIR}/bin/geoip.dat"
GEOSITE_FILE="${APP_DIR}/bin/geosite.dat"

ensure_packages() {
    if command -v apt-get >/dev/null 2>&1; then
        local packages=()
        command -v psql >/dev/null 2>&1 || packages+=(postgresql postgresql-contrib)
        command -v curl >/dev/null 2>&1 || packages+=(curl ca-certificates)
        command -v unzip >/dev/null 2>&1 || packages+=(unzip)
        command -v openssl >/dev/null 2>&1 || packages+=(openssl)
        if [[ ${#packages[@]} -gt 0 ]]; then
            apt-get update
            apt-get install -y "${packages[@]}"
        fi
    fi
}

load_env() {
    if [[ -f "${ENV_FILE}" ]]; then
        # shellcheck disable=SC1090
        source "${ENV_FILE}"
    fi
}

# write_env rewrites /etc/default/rafael with the values this script manages
# while PRESERVING any other variables already present in the file, so a
# future update can never wipe operator-added environment configuration.
write_env() {
    [[ -n "${DB_PASS}" ]] || DB_PASS="$(openssl rand -hex 16)"
    local tmp="${ENV_FILE}.tmp.$$"
    {
        echo "XUI_BIN_FOLDER=${APP_DIR}/bin"
        echo "XUI_DB_TYPE=postgres"
        printf 'XUI_DB_DSN="host=127.0.0.1 user=%s password=%s dbname=%s port=5432 sslmode=disable TimeZone=Asia/Tehran"\n' "${DB_USER}" "${DB_PASS}" "${DB_NAME}"
        if [[ -f "${ENV_FILE}" ]]; then
            grep -vE '^(XUI_BIN_FOLDER|XUI_DB_TYPE|XUI_DB_DSN)=' "${ENV_FILE}" || true
        fi
    } >"${tmp}"
    chmod 600 "${tmp}"
    mv -f "${tmp}" "${ENV_FILE}"
}

parse_dsn() {
    if [[ -n "${XUI_DB_DSN:-}" ]]; then
        local parsed_user parsed_db parsed_pass
        parsed_user="$(echo "${XUI_DB_DSN}" | sed -n 's/.*user=\([^ ]*\).*/\1/p')"
        parsed_db="$(echo "${XUI_DB_DSN}" | sed -n 's/.*dbname=\([^ ]*\).*/\1/p')"
        parsed_pass="$(echo "${XUI_DB_DSN}" | sed -n 's/.*password=\([^ ]*\).*/\1/p')"
        [[ -n "${parsed_user}" ]] && DB_USER="${parsed_user}"
        [[ -n "${parsed_db}" ]] && DB_NAME="${parsed_db}"
        [[ -n "${parsed_pass}" ]] && DB_PASS="${parsed_pass}"
    fi
    [[ -n "${DB_USER:-}" ]] || DB_USER="rafael"
    [[ -n "${DB_NAME:-}" ]] || DB_NAME="rafael_panel"
}

run_psql() {
    su - postgres -c "$1"
}

setup_postgres() {
    systemctl enable postgresql >/dev/null 2>&1 || true
    systemctl start postgresql
    run_psql "psql -tc \"SELECT 1 FROM pg_roles WHERE rolname='${DB_USER}'\"" | grep -q 1 || \
        run_psql "psql -c \"CREATE USER ${DB_USER} WITH PASSWORD '${DB_PASS}';\""
    run_psql "psql -c \"ALTER USER ${DB_USER} WITH PASSWORD '${DB_PASS}';\""
    run_psql "psql -tc \"SELECT 1 FROM pg_database WHERE datname='${DB_NAME}'\"" | grep -q 1 || \
        run_psql "psql -c \"CREATE DATABASE ${DB_NAME} OWNER ${DB_USER};\""
    run_psql "psql -c \"GRANT ALL PRIVILEGES ON DATABASE ${DB_NAME} TO ${DB_USER};\""
}

download_if_missing() {
    local url="$1"
    local dest="$2"
    [[ -f "${dest}" ]] && return 0
    curl -fL "${url}" -o "${dest}"
}

ensure_xray_assets() {
    mkdir -p "${APP_DIR}/bin"
    if [[ ! -s "${XRAY_BIN}" ]]; then
        echo "The Rafael IP-limit Xray binary is missing from this release; refusing to download stock Xray." >&2
        exit 1
    fi
    chmod 755 "${XRAY_BIN}"
    download_if_missing "https://github.com/Loyalsoldier/v2ray-rules-dat/releases/latest/download/geoip.dat" "${GEOIP_FILE}"
    download_if_missing "https://github.com/Loyalsoldier/v2ray-rules-dat/releases/latest/download/geosite.dat" "${GEOSITE_FILE}"
}

main() {
    ensure_packages
    load_env
    parse_dsn
    write_env
    ensure_xray_assets
    setup_postgres
}

main "$@"

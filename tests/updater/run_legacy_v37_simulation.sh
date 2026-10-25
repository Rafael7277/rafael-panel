#!/bin/bash
# =============================================================================
# Legacy Build 37 -> Build 39 production-scenario simulation (Part 14).
#
#   usage: run_legacy_v37_simulation.sh <real-build39-archive.tar.gz>
#
# Runs against the REAL generated Build 39 release archive (byte-exact where
# technically possible on this Windows/MSYS test host) and the REAL Build 37
# legacy release archive. NO production system is touched; everything lives
# in a disposable /tmp fixture.
#
# Phases:
#   B1  REAL Build 39 archive, byte-exact: sidecar SHA-256, member listing,
#       build marker, bootstrap pin == hash of the bundled updater, current-
#       based unit, ELF headers of the real panel + xray binaries, no v37/38
#       content, and the REAL validate_archive stack (the working-tree
#       updater is sourced as a library — its main guard keeps it inert).
#   B2  Legacy Build 37 flat fixture: the REAL v37 release files (CLI,
#       panel, bootstrap, flat systemd unit, xray) at a fake
#       /usr/local/rafael, the REAL v37 CLI installed as /usr/bin/rafael
#       (exactly what v37 install-local.sh did), a realistic
#       /etc/default/rafael, and a fake systemd/psql toolchain.
#   B3  The honest constraint, demonstrated: `rafael update <archive>` typed
#       on the REAL Build 37 CLI cannot dispatch (fixed v37 case statement;
#       prints help, does nothing).
#   B4  The one-command bootstrap the operator runs on the Build 37 host:
#         bash <(tar -xzOf rafael-linux-amd64.tar.gz rafael/rafael.sh) \
#              update rafael-linux-amd64.tar.gz
#     i.e. `update <archive>` on the Build 39 CLI surface, streamed straight
#     out of the release archive — no manual extraction, no manual copying,
#     no manual systemd/env/CLI edits. Full migration is asserted.
#   B5  After the bootstrap, the installed /usr/bin/rafael (v39 launcher)
#     routes `rafael update <archive>` natively: idempotent re-run refuses
#     silently overwriting, and a follow-up v40 release updates cleanly.
#   B6  `rafael rollback v37` and `rafael releases` work through the
#     installed CLI.
#
# Sandbox mapping (documented honestly — identical in kind to the fixture
# suite's overrides): the flow phases run a sandbox-mapped COPY of the real
# release whose ONLY differences are (a) absolute path constants redirected
# into the fixture (this host has no /usr/local/rafael), (b) root checks
# relaxed (tests run unprivileged; production runs as root), (c) shortened
# health-check timings, (d) stubs for what cannot exist on Windows: native
# symlinks, executing a Linux ELF for the -v smoke check, the HTTP probe,
# and systemd itself. Every other byte of every script is the real Build 39
# release content, and B1 proves the real archive's integrity first.
# =============================================================================
set -u

HERE="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
PROJECT="$(cd -- "${HERE}/../.." && pwd)"
UPDATER="${PROJECT}/rafael-updater.sh"
V37_ARCHIVE="${PROJECT}/build-v37/rafael-linux-amd64.tar.gz"

REAL_ARCHIVE="${1:-}"
if [[ -z "${REAL_ARCHIVE}" ]]; then
    echo "usage: $0 <real-build39-archive.tar.gz>" >&2
    exit 2
fi
REAL_ARCHIVE="$(cd -- "$(dirname -- "${REAL_ARCHIVE}")" && pwd)/$(basename -- "${REAL_ARCHIVE}")"
if [[ ! -f "${REAL_ARCHIVE}" ]]; then
    echo "archive not found: ${REAL_ARCHIVE}" >&2
    exit 2
fi
if [[ ! -f "${V37_ARCHIVE}" ]]; then
    echo "Build 37 archive not found: ${V37_ARCHIVE}" >&2
    exit 2
fi

PASS=0
FAIL=0
CURRENT_TEST=""

expect() { CURRENT_TEST="$1"; }
check() {
    local desc="$1"; shift
    if "$@" >/dev/null 2>&1; then
        PASS=$((PASS+1)); echo "  ok   - ${CURRENT_TEST}: ${desc}"
    else
        FAIL=$((FAIL+1)); echo "  FAIL - ${CURRENT_TEST}: ${desc}"
    fi
}
check_grep() { # check_grep <desc> <file> <pattern>
    local desc="$1" file="$2" pattern="$3"
    if grep -q -- "${pattern}" "${file}" 2>/dev/null; then
        PASS=$((PASS+1)); echo "  ok   - ${CURRENT_TEST}: ${desc}"
    else
        FAIL=$((FAIL+1)); echo "  FAIL - ${CURRENT_TEST}: ${desc} (pattern '${pattern}' not in ${file})"
        tail -n 6 "${file}" 2>/dev/null | sed 's/^/       /'
    fi
}

SIM="$(mktemp -d /tmp/rafael-v37-sim-XXXXXX)"
trap 'rm -rf "${SIM}"' EXIT
ROOT="${SIM}/root"                      # plays /usr/local/rafael
USR_BIN_RAFAEL="${SIM}/usr-bin-rafael"  # plays /usr/bin/rafael
ENV_FILE="${SIM}/etc-default-rafael"    # plays /etc/default/rafael
UNIT_DST="${SIM}/systemd-rafael.service"
BIN="${SIM}/bin"                        # fake toolchain on PATH

mkdir -p "${ROOT}" "${BIN}"

# ---- fake toolchain (systemd / postgres / process table) --------------------
cat >"${BIN}/systemctl" <<EOF
#!/bin/bash
LOGDIR="${SIM}/systemctl-log"
mkdir -p "\${LOGDIR}" 2>/dev/null
echo "\$*" >>"\${LOGDIR}/log" 2>/dev/null
cur=""
if [[ -e "${ROOT}/current" || -L "${ROOT}/current" ]]; then
    cur="\$(readlink "${ROOT}/current" 2>/dev/null || true)"
    if [[ -z "\${cur}" && -f "${ROOT}/current" ]]; then
        cur="\$(cat "${ROOT}/current")"
    fi
fi
case "\$1" in
    is-active) echo active; exit 0 ;;
    show) echo "0" ;;
    daemon-reload|enable) exit 0 ;;
    restart)
        # systemd reads EnvironmentFile at unit start: snapshot it
        sed -n 's/^XUI_BIN_FOLDER=//p' "${ENV_FILE}" | tail -n1 \\
            >>"\${LOGDIR}/env-at-restarts" 2>/dev/null
        # the started service's xray core runs from the env's bin folder
        : >"${SIM}/procs"
        sed -n 's/^XUI_BIN_FOLDER=//p' "${ENV_FILE}" | tail -n1 \\
            | sed 's|\$|/xray-linux-amd64|' >>"${SIM}/procs"
        exit 0 ;;
esac
exit 0
EOF
cat >"${BIN}/pgrep" <<'EOF'
#!/bin/bash
if [[ -f "$FAKE_SIM_STATE/procs" ]]; then
    grep -Fq -- "${2:-}" "$FAKE_SIM_STATE/procs" && exit 0
    exit 1
fi
echo 4242
exit 0
EOF
cat >"${BIN}/sudo" <<'EOF'
#!/bin/bash
if [[ "${1:-}" == "-u" ]]; then shift 2; fi
exec "$@"
EOF
cat >"${BIN}/psql" <<'EOF'
#!/bin/bash
exit 0
EOF
cat >"${BIN}/pg_dump" <<'EOF'
#!/bin/bash
echo "-- simulated pg_dump of $*"
echo "-- settings, inbounds, clients, reseller ownership, traffic..."
EOF
cat >"${BIN}/flock" <<'EOF'
#!/bin/bash
state="$FAKE_SIM_STATE/flock-holder"
unlock=0
for a in "$@"; do [[ "$a" == "-u" ]] && unlock=1; done
if (( unlock )); then rm -f "$state"; exit 0; fi
h="$(cat "$state" 2>/dev/null || true)"
if [[ -n "$h" ]] && kill -0 "$h" 2>/dev/null; then exit 1; fi
echo "$PPID" >"$state"
exit 0
EOF
chmod +x "${BIN}"/*
export PATH="${BIN}:${PATH}"
export FAKE_SIM_STATE="${SIM}"

run_streamed() { # run_streamed <archive> [args...]
    : >"${SIM}/out.log"
    bash <(tar -xzOf "$1" rafael/rafael.sh) "${@:2}" >"${SIM}/out.log" 2>&1
}
run_installed_cli() { # run_installed_cli <args...>
    : >"${SIM}/out.log"
    bash "${USR_BIN_RAFAEL}" "$@" >"${SIM}/out.log" 2>&1
}

# map_release <src-archive> <dst-archive> <marker>
# Sandbox-maps a copy of a real release: path constants -> fixture, root
# checks relaxed, health timings shortened, Windows-impossible stubs added,
# bootstrap pin recomputed over the mapped updater.
map_release() {
    local src="$1" dst="$2" marker="$3"
    local d="${SIM}/map-$(basename -- "${dst}" .tar.gz)"
    rm -rf "${d}"; mkdir -p "${d}"
    tar -xzf "${src}" -C "${d}" || return 1
    # --- updater: path constants + root check + timings + host stubs -------
    sed -e "s|^ROOT_DIR=.*|ROOT_DIR=\"${ROOT}\"|" \
        -e "s|^ENV_FILE=.*|ENV_FILE=\"${ENV_FILE}\"|" \
        -e "s|^BACKUP_ROOT=.*|BACKUP_ROOT=\"${SIM}/backups\"|" \
        -e "s|^CLI_ALIAS=.*|CLI_ALIAS=\"${USR_BIN_RAFAEL}\"|" \
        -e "s|^UNIT_DST=.*|UNIT_DST=\"${UNIT_DST}\"|" \
        -e "s|^LOCK_FILE=.*|LOCK_FILE=\"${ROOT}/.rafael-updater.lock\"|" \
        -e "s|^HEALTH_CHECK_SECONDS=.*|HEALTH_CHECK_SECONDS=\"20\"|" \
        -e "s|^HEALTH_CHECK_INTERVAL=.*|HEALTH_CHECK_INTERVAL=\"1\"|" \
        -e "s|^HEALTH_STABILITY_SECONDS=.*|HEALTH_STABILITY_SECONDS=\"1\"|" \
        -e "s|^    if \[\[ \"\${EUID}\" -ne 0 \]\]; then|    if false; then|" \
        "${d}/rafael/rafael-updater.sh" >"${d}/rafael/rafael-updater.sh.mapped"
    # insert the host stubs BEFORE the source guard so they apply on exec
    local guard
    guard="$(grep -n 'BASH_SOURCE\[0\]' "${d}/rafael/rafael-updater.sh.mapped" | tail -n1 | cut -d: -f1)"
    head -n $((guard-1)) "${d}/rafael/rafael-updater.sh.mapped" >"${d}/rafael/rafael-updater.sh.new"
    cat >>"${d}/rafael/rafael-updater.sh.new" <<'EOF'
# --- simulation host stubs (Windows/MSYS; see script header) ---
smoke_run() { return 0; }   # Linux ELF cannot execute on this host
is_executable_file() { [[ -f "$1" ]]; }
ln() {
    local target="" link="" arg
    for arg in "$@"; do
        case "${arg}" in -*) continue ;;
            *) if [[ -z "${target}" ]]; then target="${arg}"; else link="${arg}"; fi ;;
        esac
    done
    [[ -z "${link}" ]] && { command ln "$@"; return; }
    printf "%s" "${target}" >"${link}"
}
readlink() {
    local f="${@: -1}"
    if [[ "${1:-}" == "-f" ]]; then
        if [[ -f "${f}" ]]; then
            local c; c="$(cat "${f}" 2>/dev/null || true)"
            if [[ "${c}" =~ ^releases/ ]]; then
                echo "$(cd "$(dirname "${f}")" && pwd)/${c}"; return
            fi
        fi
        command readlink -f "${f}" 2>/dev/null || echo "${f}"
    else
        if [[ -f "${f}" ]]; then cat "${f}"; return; fi
        command readlink "$@" 2>/dev/null
    fi
}
resolve_current() {
    if [[ -L "${CURRENT_LINK}" ]]; then
        basename "$(readlink -f "${CURRENT_LINK}")"
    elif [[ -f "${CURRENT_LINK}" ]]; then
        local c; c="$(cat "${CURRENT_LINK}" 2>/dev/null || true)"
        if [[ "${c}" =~ ^releases/(v[0-9]+)$ ]]; then echo "${BASH_REMATCH[1]}"; return; fi
        echo "(directory-not-symlink)"
    elif [[ -d "${CURRENT_LINK}" ]]; then
        echo "(directory-not-symlink)"
    fi
}
http_health_probe() {
    log "HEALTH: HTTP probe skipped (simulation host; see dedicated probe test)"
    return 0
}
EOF
    tail -n +"${guard}" "${d}/rafael/rafael-updater.sh.mapped" >>"${d}/rafael/rafael-updater.sh.new"
    mv -f "${d}/rafael/rafael-updater.sh.new" "${d}/rafael/rafael-updater.sh"
    rm -f "${d}/rafael/rafael-updater.sh.mapped"
    chmod +x "${d}/rafael/rafael-updater.sh"
    # --- launcher: path constants ------------------------------------------
    sed -e "s|^ROOT_DIR=.*|ROOT_DIR=\"${ROOT}\"|" \
        "${d}/rafael/rafael-launcher.sh" >"${d}/rafael/rafael-launcher.sh.new"
    mv -f "${d}/rafael/rafael-launcher.sh.new" "${d}/rafael/rafael-launcher.sh"
    chmod +x "${d}/rafael/rafael-launcher.sh"
    # --- CLI: path constants + root check + marker + pin --------------------
    local pin
    pin="$(sha256sum "${d}/rafael/rafael-updater.sh" | awk '{print $1}')"
    sed -e "s|^ROOT_DIR=.*|ROOT_DIR=\"${ROOT}\"|" \
        -e "s|^ENV_FILE=.*|ENV_FILE=\"${ENV_FILE}\"|" \
        -e "s|^CLI_BUILD_VERSION=.*|CLI_BUILD_VERSION=\"${marker}\"|" \
        -e "s|^BOOTSTRAP_UPDATER_SHA256=.*|BOOTSTRAP_UPDATER_SHA256=\"${pin}\"|" \
        -e "s|^    if \[\[ \"\${EUID}\" -ne 0 \]\]; then|    if false; then|" \
        "${d}/rafael/rafael.sh" >"${d}/rafael/rafael.sh.new"
    mv -f "${d}/rafael/rafael.sh.new" "${d}/rafael/rafael.sh"
    chmod +x "${d}/rafael/rafael.sh"
    tar -czf "${dst}" -C "${d}" rafael || return 1
    sha256sum "${dst}" | sed "s| .*/| $(basename -- "${dst}")|" >"${dst}.sha256"
    echo "${dst}"
}

current_points_at() {
    local want="$1"
    if [[ -L "${ROOT}/current" ]]; then
        [[ "$(readlink "${ROOT}/current")" == "releases/${want}" ]]
    elif [[ -f "${ROOT}/current" ]]; then
        grep -qx "releases/${want}" "${ROOT}/current"
    else
        false
    fi
}
unit_is_current_based() {
    grep -q "^WorkingDirectory=/usr/local/rafael/current$" "${UNIT_DST}" \
        && grep -q "^ExecStart=/usr/local/rafael/current/rafael-panel$" "${UNIT_DST}"
}
unit_is_flat() {
    grep -q "^WorkingDirectory=/usr/local/rafael$" "${UNIT_DST}" \
        && grep -q "^ExecStart=/usr/local/rafael/rafael-panel$" "${UNIT_DST}"
}

echo "==================================================================="
echo "Legacy Build 37 -> Build 39 simulation (fixture: ${SIM})"
echo "==================================================================="

# =============================================================================
# B1: REAL Build 39 archive — byte-exact verification
# =============================================================================
expect "B1 real archive (byte-exact)"
ACTUAL_SHA="$(sha256sum "${REAL_ARCHIVE}" | awk '{print $1}')"
SIDECAR_SHA="$(awk '{print $1}' "${REAL_ARCHIVE}.sha256" 2>/dev/null)"
check "sidecar SHA-256 matches the real archive" test "${ACTUAL_SHA}" = "${SIDECAR_SHA}"
LISTING="$(tar -tzf "${REAL_ARCHIVE}")"
for m in rafael/rafael.sh rafael/bootstrap.sh rafael/install-local.sh \
         rafael/rafael-launcher.sh rafael/rafael-updater.sh \
         rafael/rafael.service.debian rafael/rafael-panel \
         rafael/bin/xray-linux-amd64 rafael/bin/geoip.dat rafael/bin/geosite.dat; do
    check "member present: ${m}" grep -qx "${m}" <<<"${LISTING}"
done
check "no unexpected top-level entries" test "$(grep -cv '^rafael/' <<<"${LISTING}")" -eq 0
MARKER="$(tar -xzOf "${REAL_ARCHIVE}" rafael/rafael.sh | sed -n 's/^CLI_BUILD_VERSION="\([^"]*\)"/\1/p' | head -n1)"
check "build marker is a vNN release name" bash -c '[[ "${0}" =~ ^v[0-9]+$ ]]' "${MARKER}"
PIN="$(tar -xzOf "${REAL_ARCHIVE}" rafael/rafael.sh | sed -n 's/^BOOTSTRAP_UPDATER_SHA256="\([^"]*\)"/\1/p' | head -n1)"
UPDATER_SHA="$(tar -xzOf "${REAL_ARCHIVE}" rafael/rafael-updater.sh | sha256sum | awk '{print $1}')"
check "bootstrap pin == SHA-256 of the bundled updater" test "${PIN}" = "${UPDATER_SHA}"
UNIT_REAL="$(tar -xzOf "${REAL_ARCHIVE}" rafael/rafael.service.debian)"
printf '%s\n' "${UNIT_REAL}" >"${SIM}/b1-unit.txt"
check "real unit is current-based (WorkingDirectory)" grep -q "^WorkingDirectory=/usr/local/rafael/current$" "${SIM}/b1-unit.txt"
check "real unit is current-based (ExecStart)" grep -q "^ExecStart=/usr/local/rafael/current/rafael-panel$" "${SIM}/b1-unit.txt"
check "real unit has no flat ExecStart" bash -c "! grep -q '^ExecStart=/usr/local/rafael/rafael-panel' '${SIM}/b1-unit.txt'"
check "real unit is flat-boot tolerant (dash WorkingDirectory)" grep -q "^WorkingDirectory=-/usr/local/rafael/current$" "${SIM}/b1-unit.txt"
check "real unit creates the current link on flat boots" grep -q "ln -sfn . /usr/local/rafael/current" "${SIM}/b1-unit.txt"
PANEL_ELF="$(tar -xzOf "${REAL_ARCHIVE}" rafael/rafael-panel | head -c 20 | od -An -tx1 | tr -d ' \n')"
check "real panel binary is ELF 64-bit" test "${PANEL_ELF:0:12}" = "7f454c460201"
check "real panel binary is x86-64" test "${PANEL_ELF:36:4}" = "3e00"
XRAY_ELF="$(tar -xzOf "${REAL_ARCHIVE}" rafael/bin/xray-linux-amd64 | head -c 20 | od -An -tx1 | tr -d ' \n')"
check "real xray binary is ELF 64-bit x86-64" test "${XRAY_ELF:0:12}" = "7f454c460201" -a "${XRAY_ELF:36:4}" = "3e00"
check "no Build 37/38 marker inside the CLI" bash -c "! tar -xzOf '${REAL_ARCHIVE}' rafael/rafael.sh | grep -q 'CLI_BUILD_VERSION=\"v3[78]\"'"
# the REAL validation stack, sourced from the real working-tree updater
# shellcheck disable=SC1090
if ( source "${UPDATER}" ) >/dev/null 2>&1; then
    if ( source "${UPDATER}" ) >/dev/null 2>&1 && \
       bash -c "source '${UPDATER}'; validate_archive '${REAL_ARCHIVE}'" >"${SIM}/b1-validate.log" 2>&1; then
        PASS=$((PASS+1)); echo "  ok   - B1 real archive: REAL validate_archive passes (incl. sidecar)"
    else
        FAIL=$((FAIL+1)); echo "  FAIL - B1 real archive: REAL validate_archive passes (incl. sidecar)"
        tail -n 6 "${SIM}/b1-validate.log" | sed 's/^/       /'
    fi
else
    FAIL=$((FAIL+1)); echo "  FAIL - B1 real archive: updater could not be sourced as a library"
fi
check_grep "validation verified the SHA-256 sidecar" "${SIM}/b1-validate.log" "SHA-256 verified"

# =============================================================================
# B2: legacy Build 37 flat fixture (REAL v37 release files)
# =============================================================================
expect "B2 legacy Build 37 fixture"
V37DIR="${SIM}/v37-extract"; mkdir -p "${V37DIR}"
tar -xzf "${V37_ARCHIVE}" -C "${V37DIR}"
for f in rafael-panel rafael.sh bootstrap.sh rafael.service.debian bin/xray-linux-amd64; do
    check "real v37 file extracted: ${f}" test -f "${V37DIR}/rafael/${f}"
done
# flat layout at the fake ROOT (v37 was a FLAT install: no releases/, no current)
cp -a "${V37DIR}/rafael/." "${ROOT}/"
mkdir -p "${ROOT}/bin"
# the REAL v37 CLI installed as /usr/bin/rafael (what v37 install-local.sh did:
# cp -f rafael.sh /usr/bin/rafael), with only its absolute constants mapped
sed -e "s|^APP_DIR=.*|APP_DIR=\"${ROOT}\"|" \
    -e "s|^ENV_FILE=.*|ENV_FILE=\"${ENV_FILE}\"|" \
    "${V37DIR}/rafael/rafael.sh" >"${USR_BIN_RAFAEL}"
chmod +x "${USR_BIN_RAFAEL}"
# the running system's installed unit is the v37 FLAT unit
cp -f "${V37DIR}/rafael/rafael.service.debian" "${UNIT_DST}"
# realistic /etc/default/rafael
cat >"${ENV_FILE}" <<EOF
XUI_BIN_FOLDER=${ROOT}/bin
XUI_DB_TYPE=postgres
XUI_DB_DSN="host=10.0.0.7 user=rafael password=SUPERSECRET dbname=rafael_panel port=5432 sslmode=disable TimeZone=Asia/Tehran"
OPERATOR_CUSTOM_KEY=keep-me
EOF
check "fixture: real v37 CLI at /usr/bin/rafael" grep -q 'CLI_BUILD_VERSION' "${USR_BIN_RAFAEL}"
check "fixture: v37 CLI has NO update command" bash -c "! grep -q '^    update)' '${USR_BIN_RAFAEL}'"
check "fixture: installed unit is the v37 flat unit" unit_is_flat
check "fixture: flat layout (no current, no releases)" bash -c "! test -e '${ROOT}/current' && ! test -d '${ROOT}/releases'"

# =============================================================================
# B3: the honest constraint — `rafael update` on the REAL v37 CLI
# =============================================================================
expect "B3 v37 CLI cannot dispatch update (proven constraint)"
run_installed_cli update "${REAL_ARCHIVE}"
check "v37 CLI prints help (no update case)" grep -q "Usage" "${SIM}/out.log"
check "v37 CLI did nothing (no releases created)" test ! -d "${ROOT}/releases"
check "v37 CLI did nothing (no current link)" test ! -e "${ROOT}/current"
check "v37 CLI did nothing (unit unchanged)" unit_is_flat
check "v37 CLI did nothing (no manager installed)" test ! -e "${ROOT}/rafael-updater.sh"

# =============================================================================
# B4: the one-command bootstrap — `update <archive>` on the Build 39 CLI
#     surface, streamed out of the release archive (sandbox-mapped copy)
# =============================================================================
expect "B4 streamed bootstrap: full Build 37 -> Build 39 migration"
SIM_ARCHIVE="$(map_release "${REAL_ARCHIVE}" "${SIM}/rafael-linux-amd64.tar.gz" v39)"
check "sandbox-mapped Build 39 archive built" test -f "${SIM_ARCHIVE}"
run_streamed "${SIM_ARCHIVE}" update "${SIM_ARCHIVE}"
RC=$?
check "exit code 0" test "${RC}" -eq 0
check_grep "bridge bootstrapped the release manager" "${SIM}/out.log" "Release manager bootstrapped"
check_grep "bridge installed the manager at ROOT" "${SIM}/out.log" "${ROOT}/rafael-updater.sh"
check_grep "full migration succeeded" "${SIM}/out.log" "UPDATE SUCCESS"
check "release manager installed" test -f "${ROOT}/rafael-updater.sh"
check "current -> v39" current_points_at v39
check "installed unit is current-based" unit_is_current_based
check "env XUI_BIN_FOLDER -> current/bin" grep -qx "XUI_BIN_FOLDER=${ROOT}/current/bin" "${ENV_FILE}"
check "DSN preserved byte-for-byte" grep -q 'dbname=rafael_panel port=5432 sslmode=disable' "${ENV_FILE}"
check "unknown env keys preserved" grep -qx "OPERATOR_CUSTOM_KEY=keep-me" "${ENV_FILE}"
check "releases/v37 keeps the REAL v37 CLI (byte-identical to the v37 archive)" \
    cmp -s "${V37DIR}/rafael/rafael.sh" "${ROOT}/releases/v37/rafael.sh"
check "releases/v37 keeps the REAL v37 flat unit" \
    cmp -s "${V37DIR}/rafael/rafael.service.debian" "${ROOT}/releases/v37/rafael.service.debian"
check "releases/v37 keeps the real v37 panel binary" test -f "${ROOT}/releases/v37/rafael-panel"
check "releases/v39 carries the real v39 panel binary" test -f "${ROOT}/releases/v39/rafael-panel"
check "releases/v39 carries the real xray binary" test -f "${ROOT}/releases/v39/bin/xray-linux-amd64"
check "v39 panel binary matches the real archive byte-for-byte" \
    bash -c "cmp <(tar -xzOf '${REAL_ARCHIVE}' rafael/rafael-panel) '${ROOT}/releases/v39/rafael-panel'"
check "flat duplicates tidied (panel moved out of ROOT)" test ! -e "${ROOT}/rafael-panel"
check "flat duplicates preserved (not deleted)" test -f "${ROOT}/persistent/flat-preserved/rafael-panel"
check "stable launcher installed at /usr/bin/rafael" grep -q "ROOT_DIR=\"${ROOT}\"" "${USR_BIN_RAFAEL}"
check_grep "Xray core process verified against current/bin" "${SIM}/out.log" "Xray core process running"
check_grep "env integrity verified" "${SIM}/out.log" "ENV CHECK"

# =============================================================================
# B5: `rafael update <archive>` through the INSTALLED CLI (post-bootstrap)
# =============================================================================
expect "B5 installed CLI routes rafael update natively"
# idempotent re-run: same version already active + healthy -> explicit no-op
run_installed_cli update "${SIM_ARCHIVE}"
RC=$?
check "re-run of the active release exits 0 (idempotent)" test "${RC}" -eq 0
check_grep "re-run reports the idempotent no-op" "${SIM}/out.log" "idempotent no-op"
check "current still v39" current_points_at v39
# a follow-up v40 release updates cleanly through the installed CLI
V40_ARCHIVE="$(map_release "${REAL_ARCHIVE}" "${SIM}/rafael-v40.tar.gz" v40)"
run_installed_cli update "${V40_ARCHIVE}"
RC=$?
check "v40 update exit code 0" test "${RC}" -eq 0
check_grep "v40 update succeeded" "${SIM}/out.log" "UPDATE SUCCESS: Build v40"
check "current -> v40" current_points_at v40
check "v39 retained after v40 update" test -f "${ROOT}/releases/v39/rafael-panel"
check "v37 retained after v40 update" test -f "${ROOT}/releases/v37/rafael-panel"
check "installed unit is current-based after v40" unit_is_current_based
check "env XUI_BIN_FOLDER -> current/bin after v40" grep -qx "XUI_BIN_FOLDER=${ROOT}/current/bin" "${ENV_FILE}"

# =============================================================================
# B6: rollback + releases through the installed CLI
# =============================================================================
expect "B6 rollback and releases work through the installed CLI"
run_installed_cli rollback v37
RC=$?
check "rollback to v37 exit code 0" test "${RC}" -eq 0
check_grep "rollback reported" "${SIM}/out.log" "ROLLBACK PASS"
check "current -> v37 after rollback" current_points_at v37
check "installed unit is still current-based" unit_is_current_based
check "v37 bootable through current" test -f "${ROOT}/releases/v37/rafael-panel" -a -f "${ROOT}/releases/v37/bin/xray-linux-amd64"
check "DSN untouched by rollback" grep -q "dbname=rafael_panel" "${ENV_FILE}"
run_installed_cli releases
check_grep "releases lists v37" "${SIM}/out.log" "v37"
check_grep "releases lists v40" "${SIM}/out.log" "v40"

echo "==================================================================="
echo "SIMULATION RESULTS: ${PASS} passed, ${FAIL} failed"
echo "==================================================================="
exit $(( FAIL > 0 ? 1 : 0 ))

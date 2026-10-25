#!/bin/bash
# =============================================================================
# rafael-updater test suite (fixture-based, no real production touched).
#
# Runs the REAL rafael-updater.sh logic against a sandboxed fake ROOT_DIR /
# ENV_FILE / systemd / psql / pgrep / flock. Covers the 18 test categories
# from the release architecture spec plus the post-audit fix tests:
#   1  path resolution          2  current symlink
#   3  flat migration           4  archive validation (structure)
#   5  traversal rejection      6  missing-file rejection
#   7  wrong-arch rejection     8  build-version validation
#   9  env preservation        10  staging failure
#  11  activation              12  health pass (+ 12b: xray-core dead)
#  13  health fail + auto rollback   14  rollback with broken current
#  15  retention               16  locking (concurrency)
#  17  idempotency             18  unit paths (no version-specific paths)
#  19  (F1) injected failure at EVERY migration boundary (11 boundaries)
#  20  (F2) XUI_BIN_FOLDER rewritten before every restart
#  21  (F2) set_env_bin_folder semantics + systemd EnvironmentFile ordering
#  22-24e (F3) malicious archives: symlink / hardlink / symlink+child escape
#        / absolute path / ../ traversal / unexpected top-level / mixed —
#        all rejected BEFORE extraction
#  25  flat fixture uses the REAL Build 37 flat unit; v37 stays faithful
#  26  static source audit (no flat systemd paths in the migration)
#  27-33 (bootstrap gap) `rafael update <archive>` on a host with NO release
#        manager: bridges from the archive (pin-verified updater + REAL
#        validate_archive, then full migration) / pin mismatch fails closed
#        / pin-matching malicious archive rejected by the real validation
#        stack / empty pin fails closed / update-local rejects release-layout
#        archives with guidance / update-local still serves legacy flat
#        archives / update without manager+archive prints guidance only
#  34-37 (failure injection completion) migration-time v37 health failure
#        (dedicated flat-restore path + retry self-heal) / v39 restart
#        failure rollback / stale Xray core (process on the OLD path —
#        pattern-aware fake pgrep) / interrupted v37 copy re-copied on retry
#
# Environment shims (documented honestly):
#   - This host (Git Bash / MSYS) cannot create native symlinks and has no
#     real flock. When SYMLINKS_OK=0 the sandboxed updater gets function
#     overrides for ln / readlink / resolve_current (text-link files) so the
#     update FLOW is exercised; symlink atomicity itself is Linux-only and
#     is verified by code inspection, not here.
#   - The panel binary is a fake ELF header, so the runtime -v smoke check
#     is stubbed; the ELF header check itself IS exercised (T7).
#   - pgrep is faked (xray core "runs" unless the xray-down flag is set by
#     fake systemctl in FAKE_SVC_MODE=xray-down-v39), so the REAL
#     xray_process_check logic runs in the flow tests.
#   - fake systemctl snapshots the env file's XUI_BIN_FOLDER at every
#     restart (models systemd's EnvironmentFile load at unit start, BEFORE
#     ExecStartPre) — T20 asserts on those snapshots.
#   - http_health_probe gets a dedicated real-logic test (P-test) using a
#     runnable fake panel script and fake curl.
# =============================================================================
set -u

HERE="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
PROJECT="$(cd -- "${HERE}/../.." && pwd)"
UPDATER="${PROJECT}/rafael-updater.sh"
UNIT_TEMPLATE="${PROJECT}/rafael.service.debian"
LAUNCHER_TEMPLATE="${PROJECT}/rafael-launcher.sh"

if [[ ! -f "${UPDATER}" ]]; then
    echo "rafael-updater.sh not found at ${UPDATER}" >&2
    exit 1
fi

PASS=0
FAIL=0
CURRENT_TEST=""

SBROOT="$(mktemp -d /tmp/rafael-updater-tests-XXXXXX)"
trap 'rm -rf "${SBROOT}"' EXIT

SB=""
FAKE_ROOT=""
ENV_FILE=""

# --- detect native symlink support -------------------------------------------
SYMLINKS_OK=1
probe="${SBROOT}/symprobe"
rm -rf "${probe}"; mkdir -p "${probe}"
if ln -sfn "target" "${probe}/link" 2>/dev/null; then
    [[ -L "${probe}/link" ]] || SYMLINKS_OK=0
else
    SYMLINKS_OK=0
fi
rm -rf "${probe}"
if (( SYMLINKS_OK )); then
    echo "symlinks: native support detected"
else
    echo "symlinks: NOT supported on this host (MSYS) - using text-link shims"
fi

sandbox() {
    SB="${SBROOT}/${SBID}"
    rm -rf "${SB}"
    mkdir -p "${SB}"

    # fake root layout (plays /usr/local/rafael)
    FAKE_ROOT="${SB}/root"
    mkdir -p "${FAKE_ROOT}"

    # fake env file
    ENV_FILE="${SB}/etc-default-rafael"
    cat >"${ENV_FILE}" <<EOF
XUI_BIN_FOLDER=${FAKE_ROOT}/bin
XUI_DB_TYPE=postgres
XUI_DB_DSN="host=127.0.0.1 user=rafael password=SECRETPW dbname=rafael_panel port=5432 sslmode=disable TimeZone=Asia/Tehran"
EOF
}

# ---- fake commands on PATH ---------------------------------------------------
mkdir -p "${SBROOT}/bin"

# fake systemctl:
#   FAKE_SVC_MODE=healthy         (default) service always active
#   FAKE_SVC_MODE=crash-v39       service dies whenever current points at v39
#   FAKE_SVC_MODE=xray-down-v39   panel up on v39 but the Xray CORE is dead
#                                 (F2 false-positive model: web UI answers,
#                                  xray process absent -> pgrep flag set)
#   FAKE_SVC_MODE=down            service always inactive
# Resolves the "current" link through BOTH real symlinks and text-links.
# On every `restart` the fake snapshots the env file's XUI_BIN_FOLDER into
# env-at-restarts — this models systemd reading EnvironmentFile at unit
# start, BEFORE ExecStartPre (bootstrap.sh) can rewrite it. T20 asserts the
# value systemd would have loaded at every restart.
cat >"${SBROOT}/bin/systemctl" <<'EOF'
#!/bin/bash
LOGDIR="${FAKE_TEST_ROOT:-/tmp}/systemctl-log"
mkdir -p "${LOGDIR}" 2>/dev/null
echo "$*" >>"${LOGDIR}/log" 2>/dev/null
mode="${FAKE_SVC_MODE:-healthy}"
cur=""
if [[ -e "${FAKE_TEST_ROOT}/current" || -L "${FAKE_TEST_ROOT}/current" ]]; then
    cur="$(readlink "${FAKE_TEST_ROOT}/current" 2>/dev/null || true)"
    if [[ -z "${cur}" && -f "${FAKE_TEST_ROOT}/current" ]]; then
        cur="$(cat "${FAKE_TEST_ROOT}/current")"
    fi
fi
case "$1" in
    is-active)
        case "${mode}" in
            down) echo inactive; exit 3 ;;
            crash-v39)
                [[ -f "${FAKE_TEST_STATE}/svc-down" ]] && { echo inactive; exit 3; }
                echo active; exit 0 ;;
            *) echo active; exit 0 ;;
        esac ;;
    show)
        echo "0" ;;
    restart)
        # systemd EnvironmentFile snapshot: what the service would load NOW
        if [[ -n "${FAKE_TEST_ENV:-}" && -f "${FAKE_TEST_ENV}" ]]; then
            sed -n 's/^XUI_BIN_FOLDER=//p' "${FAKE_TEST_ENV}" | tail -n1 \
                >>"${LOGDIR}/env-at-restarts" 2>/dev/null
        fi
        if [[ "${mode}" == "crash-v39" ]]; then
            if [[ "${cur}" == "releases/v39" ]]; then
                touch "${FAKE_TEST_STATE}/svc-down"
            else
                rm -f "${FAKE_TEST_STATE}/svc-down"
            fi
        fi
        if [[ "${mode}" == "xray-down-v39" ]]; then
            if [[ "${cur}" == "releases/v39" ]]; then
                touch "${FAKE_TEST_STATE}/xray-down"
            else
                rm -f "${FAKE_TEST_STATE}/xray-down"
            fi
        fi
        # stale-xray-v39: on v39 the restarted service keeps the OLD (flat)
        # xray core running — the process table records the stale command
        # line, so the updater's pgrep -f <current/bin>/xray must miss it.
        if [[ "${mode}" == "stale-xray-v39" ]]; then
            : >"${FAKE_TEST_STATE}/procs"
            if [[ "${cur}" == "releases/v39" ]]; then
                echo "${FAKE_TEST_ROOT}/bin/xray-linux-amd64" >>"${FAKE_TEST_STATE}/procs"
            else
                sed -n 's/^XUI_BIN_FOLDER=//p' "${FAKE_TEST_ENV}" | tail -n1 \
                    | sed 's|$|/xray-linux-amd64|' >>"${FAKE_TEST_STATE}/procs"
            fi
        fi
        if [[ "${mode}" == "restart-fail-v39" && "${cur}" == "releases/v39" ]]; then
            exit 1
        fi
        exit 0 ;;
esac
exit 0
EOF

# fake pgrep: honours the xray-down flag; when a process table exists it is
# PATTERN-AWARE (models `pgrep -f <path>` against recorded command lines —
# this is what catches a stale xray core started from an old path). Without
# a table the old behaviour (always found) keeps the non-xray tests simple.
cat >"${SBROOT}/bin/pgrep" <<'EOF'
#!/bin/bash
if [[ -f "${FAKE_TEST_STATE:-/nonexistent}/xray-down" ]]; then
    exit 1
fi
if [[ -f "${FAKE_TEST_STATE:-/nonexistent}/procs" ]]; then
    pattern="${2:-}"
    grep -Fq -- "${pattern}" "${FAKE_TEST_STATE}/procs" && exit 0
    exit 1
fi
echo 4242
exit 0
EOF

# fake sudo: drops "-u postgres"
cat >"${SBROOT}/bin/sudo" <<'EOF'
#!/bin/bash
if [[ "${1:-}" == "-u" ]]; then shift 2; fi
"$@"
EOF

# fake psql: settings table backed by a flat file
cat >"${SBROOT}/bin/psql" <<'EOF'
#!/bin/bash
# emulates: psql -t -A -d <db> -c "SELECT value FROM settings WHERE key='<k>'"
cmd="${*: -1}"
key="$(printf '%s' "${cmd}" | sed -n "s/.*WHERE key='\([^']*\)'.*/\1/p")"
f="${FAKE_TEST_SETTINGS}"
if [[ -z "${key}" || ! -f "${f}" ]]; then exit 0; fi
grep "^${key}=" "${f}" | head -n1 | cut -d= -f2-
EOF

# fake pg_dump: non-empty dump
cat >"${SBROOT}/bin/pg_dump" <<'EOF'
#!/bin/bash
echo "-- fake pg_dump of $1"
echo "-- settings table, inbounds, clients..."
EOF

# fake flock: lock state file with PID liveness (no /proc dependency).
#   flock -n <fd>  -> exit 1 if held by a live process, else take it
#   flock -u <fd>  -> release
cat >"${SBROOT}/bin/flock" <<'EOF'
#!/bin/bash
nonblock=0
unlock=0
while [[ $# -gt 0 ]]; do
    case "$1" in
        -n) nonblock=1 ;;
        -u) unlock=1 ;;
        -*) : ;;
    esac
    shift
done
state="${FAKE_TEST_STATE}/flock-holder"
if (( unlock )); then
    rm -f "${state}" 2>/dev/null
    exit 0
fi
holder="$(cat "${state}" 2>/dev/null || true)"
if [[ -n "${holder}" ]] && kill -0 "${holder}" 2>/dev/null; then
    exit 1   # held by a live process
fi
echo "${PPID}" >"${state}"   # holder is the calling (updater) shell
exit 0
EOF

chmod +x "${SBROOT}/bin/"*
export PATH="${SBROOT}/bin:${PATH}"

# fake settings table: no certs configured by default
echo -n >"${SBROOT}/settings-table"

# ---- fixtures ------------------------------------------------------------------

# fake ELF x86-64 binary (header only; not actually runnable code)
make_fake_elf() {
    local out="$1"
    printf '\x7fELF\x02' >"${out}"
    head -c 13 /dev/zero >>"${out}"
    printf '\x3e\x00' >>"${out}"     # e_machine = 0x3e (x86-64), LE
    head -c 64 /dev/zero >>"${out}"
    chmod +x "${out}"
}

# vNN release archive
make_archive() {
    local version="$1"
    local dir="${SBROOT}/archive-${version}"
    rm -rf "${dir}"
    mkdir -p "${dir}/rafael/bin"
    make_fake_elf "${dir}/rafael/rafael-panel"
    make_fake_elf "${dir}/rafael/bin/xray-linux-amd64"
    {
        echo '#!/bin/bash'
        echo "CLI_BUILD_VERSION=\"${version}\""
    } >"${dir}/rafael/rafael.sh"
    echo '#!/bin/bash' >"${dir}/rafael/bootstrap.sh"
    cp "${UNIT_TEMPLATE}" "${dir}/rafael/rafael.service.debian"
    cp "${UPDATER}" "${dir}/rafael/rafael-updater.sh"
    cp "${LAUNCHER_TEMPLATE}" "${dir}/rafael/rafael-launcher.sh"
    tar -czf "${SBROOT}/rafael-${version}.tar.gz" -C "${dir}" rafael
    # refresh the sidecar for THIS build: gzip embeds the build time, so a
    # rebuild at the same path never matches an older sidecar, and a stale
    # one left by an earlier test would fail validation for the wrong
    # reason (tests that need a bad sidecar overwrite it afterwards).
    sha256sum "${SBROOT}/rafael-${version}.tar.gz" | sed "s| .*/| rafael-${version}.tar.gz|" >"${SBROOT}/rafael-${version}.tar.gz.sha256"
    echo "${SBROOT}/rafael-${version}.tar.gz"
}

# fake flat Build NN install. The systemd unit is the REAL Build 37 flat unit
# (verbatim from the deployed git-HEAD rafael.service.debian: WorkingDirectory
# and ExecStart reference /usr/local/rafael DIRECTLY, not current/). It is
# also installed at UNIT_DST the way the real production system has it — a
# flat install has no current symlink yet, so a current-based unit would not
# boot. This fixture is what makes the migration tests catch a migration that
# installs the flat unit (audit finding F1) or forgets the env rewrite (F2).
FLAT_UNIT_V37='[Unit]
Description=Rafael Panel Service
After=network.target postgresql.service
Wants=network.target

[Service]
EnvironmentFile=-/etc/default/rafael
Environment="XRAY_VMESS_AEAD_FORCED=false"
Type=simple
WorkingDirectory=/usr/local/rafael
ExecStartPre=/bin/bash -lc '"'"'sed -i "s/\r$//" /usr/local/rafael/bootstrap.sh /usr/local/rafael/rafael.sh 2>/dev/null || true; exec /bin/bash /usr/local/rafael/bootstrap.sh'"'"'
ExecStart=/usr/local/rafael/rafael-panel
ExecReload=/bin/kill -USR1 $MAINPID
Restart=on-failure
RestartSec=5s

[Install]
WantedBy=multi-user.target'

make_flat_install() {
    local version="$1"
    mkdir -p "${FAKE_ROOT}/bin"
    make_fake_elf "${FAKE_ROOT}/rafael-panel"
    make_fake_elf "${FAKE_ROOT}/bin/xray-linux-amd64"
    {
        echo '#!/bin/bash'
        echo "CLI_BUILD_VERSION=\"${version}\""
    } >"${FAKE_ROOT}/rafael.sh"
    echo '#!/bin/bash' >"${FAKE_ROOT}/bootstrap.sh"
    # the flat release dir carries the faithful Build 37 FLAT unit...
    printf '%s\n' "${FLAT_UNIT_V37}" >"${FAKE_ROOT}/rafael.service.debian"
    # ...and the running system's installed unit is the same flat unit
    printf '%s\n' "${FLAT_UNIT_V37}" >"${SB}/systemd-rafael.service"
    touch "${FAKE_ROOT}/install-local.sh"
    touch "${FAKE_ROOT}/operator-notes.txt"   # unknown file: must be preserved
}

# ---- malicious archive builder (F3 tests) --------------------------------------
# Builds tar archives containing symlink / hardlink / absolute / traversal
# members BY HAND: tar headers are 512-byte blocks full of NUL bytes, which
# bash strings cannot hold, so this is perl. Members are valid ustar; GNU tar
# reads and would extract them (symlink members resolve during extraction of
# later members — the escape the updater must reject before extracting).
cat >"${SBROOT}/mkevil.pl" <<'PERLEOF'
#!/usr/bin/perl
# mkevil.pl <output.tar> <mode> [linktarget]
use strict; use warnings;
my ($out, $mode, $linktarget) = @ARGV;
$linktarget = "/etc" unless defined $linktarget;

my $ELF = "\x7fELF\x02" . ("\0" x 13) . "\x3e\x00" . ("\0" x 64);
my $UNIT = "[Unit]\nDescription=Rafael Panel Service\n\n[Service]\n" .
    "WorkingDirectory=/usr/local/rafael/current\n" .
    "ExecStart=/usr/local/rafael/current/rafael-panel\n";

sub header {
    my ($name, $type, $size, $link) = @_;
    my $h = "";
    $h .= pack("a100", $name);
    $h .= pack("a8", "0000755\0");
    $h .= pack("a8", "0000000\0");
    $h .= pack("a8", "0000000\0");
    $h .= pack("a12", sprintf("%011o", $size) . "\0");
    $h .= pack("a12", sprintf("%011o", 0) . "\0");
    $h .= "        ";              # checksum placeholder while summing
    $h .= $type;
    $h .= pack("a100", defined($link) ? $link : "");
    $h .= pack("a8", "ustar\0" . "00");
    $h .= pack("a32", "root");
    $h .= pack("a32", "root");
    $h .= pack("a8", "0000000\0");
    $h .= pack("a8", "0000000\0");
    $h .= pack("a155", "");
    $h .= pack("a12", "");
    my $sum = 0; $sum += $_ for unpack("C*", $h);
    substr($h, 148, 8) = sprintf("%06o\0 ", $sum);
    return $h;
}
sub content {
    my ($data) = @_;
    return "" unless length($data);
    return $data . ("\0" x (512 - length($data) % 512));
}
sub regular {
    my ($name, $data) = @_;
    return header($name, "0", length($data)) . content($data);
}
sub symlink_member {
    my ($name, $target) = @_;
    return header($name, "2", 0, $target);
}

my @members;
if ($mode eq "symlink") {
    push @members, symlink_member("rafael/x", $linktarget);
} elsif ($mode eq "symlink-child") {
    push @members, symlink_member("rafael/x", $linktarget);
    push @members, regular("rafael/x/evil", "pwned\n");
} elsif ($mode eq "hardlink") {
    push @members, regular("rafael/rafael-panel", $ELF);
    push @members, header("rafael/hard", "1", 0, "rafael/rafael-panel");
} elsif ($mode eq "absolute") {
    push @members, regular("/etc/evil", "pwned\n");
} elsif ($mode eq "traversal") {
    push @members, regular("../pwned", "pwned\n");
} elsif ($mode eq "toplevel") {
    push @members, regular("other/file", "pwned\n");
} elsif ($mode eq "mixed") {
    # an otherwise-VALID v39 release (all required files, valid build
    # marker, x86-64 ELFs) plus ONE symlink member — proving a single
    # malicious member poisons an archive that passes every other check.
    push @members, regular("rafael/rafael-panel", $ELF);
    push @members, regular("rafael/rafael.sh", "#!/bin/bash\nCLI_BUILD_VERSION=\"v39\"\n");
    push @members, regular("rafael/bootstrap.sh", "#!/bin/bash\n");
    push @members, regular("rafael/rafael.service.debian", $UNIT);
    push @members, regular("rafael/rafael-updater.sh", "#!/bin/bash\n");
    push @members, regular("rafael/rafael-launcher.sh", "#!/bin/bash\n");
    push @members, regular("rafael/bin/xray-linux-amd64", $ELF);
    push @members, symlink_member("rafael/x", $linktarget);
} elsif ($mode eq "release-symlink") {
    # Same as "mixed", but rafael/rafael-updater.sh carries the content of
    # the file given as the 4th argument (the sandboxed, PIN-MATCHING
    # updater). Used by the CLI bootstrap bridge test: the bridge extracts
    # and pin-verifies the updater member, then must still reject the whole
    # archive through the real validate_archive because of the symlink.
    my $updater_content = "#!/bin/bash\n";
    if (defined $ARGV[3]) {
        open my $uf, "<", $ARGV[3] or die "open $ARGV[3]: $!\n";
        binmode $uf; local $/; $updater_content = <$uf>; close $uf;
    }
    push @members, regular("rafael/rafael-panel", $ELF);
    push @members, regular("rafael/rafael.sh", "#!/bin/bash\nCLI_BUILD_VERSION=\"v39\"\n");
    push @members, regular("rafael/bootstrap.sh", "#!/bin/bash\n");
    push @members, regular("rafael/rafael.service.debian", $UNIT);
    push @members, regular("rafael/rafael-updater.sh", $updater_content);
    push @members, regular("rafael/rafael-launcher.sh", "#!/bin/bash\n");
    push @members, regular("rafael/bin/xray-linux-amd64", $ELF);
    push @members, symlink_member("rafael/x", $linktarget);
} else {
    die "mkevil.pl: unknown mode: $mode\n";
}
open(my $fh, ">", $out) or die "open $out: $!\n";
binmode $fh;
for my $m (@members) { print $fh $m; }
print $fh ("\0" x 512) x 2;
close $fh or die "close $out: $!\n";
PERLEOF

# make_evil_tar <output.tar.gz> <mode> [linktarget] [updater-file]
make_evil_tar() {
    local out="$1" mode="$2" target="${3:-/etc}" updfile="${4:-}"
    rm -f "${out}" "${out%.gz}"
    perl "${SBROOT}/mkevil.pl" "${out%.gz}" "${mode}" "${target}" "${updfile}"
    gzip -f "${out%.gz}"
    # fixture sanity: the host tar must be able to read the hand-built
    # archive, otherwise the rejection test would be testing nothing.
    tar -tzf "${out}" >/dev/null 2>&1 || {
        echo "make_evil_tar: host tar cannot read ${out} (bad fixture)" >&2
        exit 2
    }
}

# point "current" at a release (works with and without native symlinks)
set_current() {
    local target="$1"   # e.g. v39
    rm -rf "${FAKE_ROOT}/current"
    if (( SYMLINKS_OK )); then
        ln -sfn "releases/${target}" "${FAKE_ROOT}/current"
    else
        printf 'releases/%s' "${target}" >"${FAKE_ROOT}/current"
    fi
}

current_points_at() {
    local want="$1"
    if [[ -L "${FAKE_ROOT}/current" ]]; then
        [[ "$(readlink "${FAKE_ROOT}/current")" == "releases/${want}" ]]
    elif [[ -f "${FAKE_ROOT}/current" ]]; then
        grep -qx "releases/${want}" "${FAKE_ROOT}/current"
    else
        false
    fi
}

# ---- sandboxed updater ----------------------------------------------------------

# Overrides appended to the sandboxed updater. On symlink-capable hosts only
# smoke_run is stubbed; otherwise the text-link shims are added too.
SANDBOX_OVERRIDES='
# --- sandbox stubs ---
smoke_run() { return 0; }  # fake ELF cannot execute; header check still runs
# MSYS emulates the exec bit by file CONTENT, so test -x is false on the fake
# ELF even after chmod 755. File-existence is the shim; on Linux the real
# is_executable_file (test -x) runs untouched.
is_executable_file() { [[ -f "$1" ]]; }
'
if (( ! SYMLINKS_OK )); then
    SANDBOX_OVERRIDES+='
# --- sandbox text-link shims (host lacks native symlink support) ---
ln() {
    local target="" link="" arg
    for arg in "$@"; do
        case "${arg}" in
            -*) continue ;;
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
            local c
            c="$(cat "${f}" 2>/dev/null || true)"
            if [[ "${c}" =~ ^releases/ ]]; then
                echo "$(cd "$(dirname "${f}")" && pwd)/${c}"
                return
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
        local c
        c="$(cat "${CURRENT_LINK}" 2>/dev/null || true)"
        if [[ "${c}" =~ ^releases/(v[0-9]+)$ ]]; then
            echo "${BASH_REMATCH[1]}"
            return
        fi
        echo "(directory-not-symlink)"
    elif [[ -d "${CURRENT_LINK}" ]]; then
        echo "(directory-not-symlink)"
    fi
}
http_health_probe() {
    log "HEALTH: HTTP probe skipped (sandbox: no native symlinks; see dedicated probe test)"
    return 0
}
'
fi

# Failure injection (T19): a LITERAL text injection into the updater source,
# applied before the sandbox overrides. Anchors are matched as literal text
# (no regex metacharacters) and must be unique, so a refactor that moves or
# renames a boundary line fails the test loudly instead of silently skipping
# the injection. Tests inject `die "INJECTED: ..."` at an exact migration
# boundary and then assert the system is still bootable / recoverable.
#   INJECT_MODE   "after" (insert after anchor) or "replace"
#   INJECT_ANCHOR literal anchor text (include the leading newline + exact
#                 indentation so sibling lines cannot match)
#   INJECT_TEXT   text to insert / replace with
# patched_updater resets all three after applying.
INJECT_MODE=""
INJECT_ANCHOR=""
INJECT_TEXT=""

inject_literal() {
    perl -0 -e '
        my ($mode, $anchor, $text, $in, $out) = @ARGV;
        local $/;
        open my $fh, "<", $in or die "open $in: $!\n";
        my $s = <$fh>; close $fh;
        my $pos = index($s, $anchor);
        die "INJECT anchor not found: $anchor\n" if $pos < 0;
        die "INJECT anchor not unique: $anchor\n" if index($s, $anchor, $pos + 1) >= 0;
        if ($mode eq "after") {
            substr($s, $pos + length($anchor), 0) = $text;
        } elsif ($mode eq "replace") {
            substr($s, $pos, length($anchor), $text);
        } else {
            die "bad INJECT mode: $mode\n";
        }
        open my $o, ">", $out or die "open $out: $!\n";
        print $o $s; close $o;
    ' "${INJECT_MODE}" "${INJECT_ANCHOR}" "${INJECT_TEXT}" \
        "${SB}/updater.raw.sh" "${SB}/updater.raw2.sh" || exit 2
}

patched_updater() {
    sed -e "s|^ROOT_DIR=.*|ROOT_DIR=\"${FAKE_ROOT}\"|" \
        -e "s|^ENV_FILE=.*|ENV_FILE=\"${ENV_FILE}\"|" \
        -e "s|^BACKUP_ROOT=.*|BACKUP_ROOT=\"${SB}/backups\"|" \
        -e "s|^CLI_ALIAS=.*|CLI_ALIAS=\"${SB}/usr-bin-rafael\"|" \
        -e "s|^UNIT_DST=.*|UNIT_DST=\"${SB}/systemd-rafael.service\"|" \
        -e "s|^LOCK_FILE=.*|LOCK_FILE=\"${FAKE_ROOT}/.rafael-updater.lock\"|" \
        -e "s|^HEALTH_CHECK_SECONDS=.*|HEALTH_CHECK_SECONDS=\"30\"|" \
        -e "s|^HEALTH_CHECK_INTERVAL=.*|HEALTH_CHECK_INTERVAL=\"1\"|" \
        -e "s|^HEALTH_STABILITY_SECONDS=.*|HEALTH_STABILITY_SECONDS=\"2\"|" \
        -e "s|^    if \[\[ \"\${EUID}\" -ne 0 \]\]; then|    if false; then|" \
        "${UPDATER}" >"${SB}/updater.raw.sh"
    if [[ -n "${INJECT_MODE}" ]]; then
        inject_literal
        mv -f "${SB}/updater.raw2.sh" "${SB}/updater.raw.sh"
        INJECT_MODE=""
        INJECT_ANCHOR=""
        INJECT_TEXT=""
    fi
    # insert overrides before the source-guard at the bottom of the updater
    # (the guard makes the updater sourceable as a library by the CLI
    # bootstrap bridge; overrides must sit OUTSIDE the guard so they apply
    # when the file is run as a script)
    local guard_line
    guard_line="$(grep -n 'BASH_SOURCE\[0\]' "${SB}/updater.raw.sh" | tail -n1 | cut -d: -f1)"
    [[ -n "${guard_line}" ]] || { echo "patched_updater: source guard not found (updater layout changed?)" >&2; exit 2; }
    head -n $(( guard_line - 1 )) "${SB}/updater.raw.sh" >"${SB}/updater.sh"
    printf '%s\n' "${SANDBOX_OVERRIDES}" >>"${SB}/updater.sh"
    tail -n +"${guard_line}" "${SB}/updater.raw.sh" >>"${SB}/updater.sh"
    rm -f "${SB}/updater.raw.sh"
    chmod +x "${SB}/updater.sh"
}

run_sb_updater() {
    export FAKE_TEST_ROOT="${FAKE_ROOT}"
    export FAKE_TEST_STATE="${SB}"
    export FAKE_TEST_SETTINGS="${SBROOT}/settings-table"
    export FAKE_TEST_ENV="${ENV_FILE}"
    : >"${SB}/out.log"
    bash "${SB}/updater.sh" "$@" >"${SB}/out.log" 2>"${SBROOT}/stderr.log"
}

# ---- assertions -------------------------------------------------------------------

expect() {
    CURRENT_TEST="$1"
}

check() {
    # check <desc> <cmd...>
    local desc="$1"; shift
    if "$@" >/dev/null 2>&1; then
        PASS=$((PASS+1))
        echo "  ok   - ${CURRENT_TEST}: ${desc}"
    else
        FAIL=$((FAIL+1))
        echo "  FAIL - ${CURRENT_TEST}: ${desc}"
        if [[ -s "${SBROOT}/stderr.log" ]]; then
            tail -n 3 "${SBROOT}/stderr.log" 2>/dev/null | sed 's/^/       /'
        fi
    fi
}

check_log() {
    # check_log <desc> <pattern>  (greps the updater stdout log)
    local desc="$1" pattern="$2"
    if grep -q "${pattern}" "${SB}/out.log" 2>/dev/null; then
        PASS=$((PASS+1))
        echo "  ok   - ${CURRENT_TEST}: ${desc}"
    else
        FAIL=$((FAIL+1))
        echo "  FAIL - ${CURRENT_TEST}: ${desc} (pattern '${pattern}' not in out.log)"
        tail -n 8 "${SB}/out.log" 2>/dev/null | sed 's/^/       /'
    fi
}

check_no_log() {
    local desc="$1" pattern="$2"
    if ! grep -q "${pattern}" "${SB}/out.log" 2>/dev/null; then
        PASS=$((PASS+1))
        echo "  ok   - ${CURRENT_TEST}: ${desc}"
    else
        FAIL=$((FAIL+1))
        echo "  FAIL - ${CURRENT_TEST}: ${desc} (unexpected pattern '${pattern}')"
        grep "${pattern}" "${SB}/out.log" 2>/dev/null | head -3 | sed 's/^/       /'
    fi
}

check_stderr() {
    local desc="$1" pattern="$2"
    if grep -q "${pattern}" "${SBROOT}/stderr.log" 2>/dev/null; then
        PASS=$((PASS+1))
        echo "  ok   - ${CURRENT_TEST}: ${desc}"
    else
        FAIL=$((FAIL+1))
        echo "  FAIL - ${CURRENT_TEST}: ${desc} (pattern '${pattern}' not in stderr)"
        tail -n 3 "${SBROOT}/stderr.log" 2>/dev/null | sed 's/^/       /'
    fi
}

# ---- F1 boundary-test helpers ---------------------------------------------------

# the installed systemd unit references ONLY current/-based paths
unit_is_current_based() {
    grep -q "^WorkingDirectory=/usr/local/rafael/current$" "${SB}/systemd-rafael.service" \
        && grep -q "^ExecStart=/usr/local/rafael/current/rafael-panel$" "${SB}/systemd-rafael.service" \
        && grep -q "ExecStartPre=.* /usr/local/rafael/current/bootstrap.sh" "${SB}/systemd-rafael.service"
}

# the installed systemd unit references the FLAT paths (Build 37 layout)
unit_is_flat() {
    grep -q "^WorkingDirectory=/usr/local/rafael$" "${SB}/systemd-rafael.service" \
        && grep -q "^ExecStart=/usr/local/rafael/rafael-panel$" "${SB}/systemd-rafael.service"
}

# current points at <v> AND every path the current-based unit references
# (WorkingDirectory, ExecStartPre bootstrap, ExecStart panel, bin/xray for
# the env) resolves — i.e. systemd would boot it.
current_is_bootable() {
    local want="$1"
    current_points_at "${want}" \
        && test -f "${FAKE_ROOT}/releases/${want}/rafael-panel" \
        && test -f "${FAKE_ROOT}/releases/${want}/bootstrap.sh" \
        && test -f "${FAKE_ROOT}/releases/${want}/bin/xray-linux-amd64"
}

# the preserved v37 release is complete enough to run and roll back to
v37_recoverable() {
    test -f "${FAKE_ROOT}/releases/v37/rafael-panel" \
        && test -f "${FAKE_ROOT}/releases/v37/bootstrap.sh" \
        && test -f "${FAKE_ROOT}/releases/v37/bin/xray-linux-amd64"
}

# boundary_run: set up a flat Build 37 fixture + v39 archive, inject a die at
# the given migration boundary (see INJECT_* above), and run the update.
boundary_run() {
    local sbid="$1"
    SBID="${sbid}"; sandbox
    make_flat_install v37
    A39="$(make_archive v39)"
    patched_updater      # applies INJECT_MODE/INJECT_ANCHOR/INJECT_TEXT
    run_sb_updater update "${A39}"
}

# after a boundary failure, `rafael rollback v37` must still work
rollback_to_v37_works() {
    run_sb_updater rollback v37
    grep -qE "ROLLBACK PASS|already active" "${SB}/out.log" 2>/dev/null
}

# no flat-path ExecStart in the installed unit
no_flat_unit_installed() {
    ! grep -q "ExecStart=/usr/local/rafael/rafael-panel" "${SB}/systemd-rafael.service"
}

echo "==================================================================="
echo "rafael-updater.sh fixture test suite"
echo "==================================================================="

# =============================================================================
# T1/T2: path resolution + current symlink (status on release layout)
# =============================================================================
expect "T1/T2 path-resolution + current symlink (status)"
SBID="status-release"; sandbox
mkdir -p "${FAKE_ROOT}/releases/v37" "${FAKE_ROOT}/releases/v39"
touch "${FAKE_ROOT}/releases/v37/rafael.sh" "${FAKE_ROOT}/releases/v39/rafael.sh"
set_current v39
patched_updater
run_sb_updater status
check_log "status lists v37" "v37"
check_log "status lists v39" "v39"
if (( SYMLINKS_OK )); then
    check_log "status shows active release" "current: v39"
fi

# =============================================================================
# T3: flat migration (copy-then-switch, unknown files preserved)
# =============================================================================
expect "T3 flat migration preserves everything"
SBID="flat-migration"; sandbox
make_flat_install v37
A39="$(make_archive v39)"
patched_updater
run_sb_updater update "${A39}"
check "migration preserves old build under releases/v37" test -f "${FAKE_ROOT}/releases/v37/rafael-panel"
check "unknown flat file preserved" test -f "${FAKE_ROOT}/operator-notes.txt" -o -f "${FAKE_ROOT}/persistent/flat-preserved/operator-notes.txt"
check "current points at v39" current_points_at v39
check "flat duplicates tidied (rafael-panel moved out of root)" test ! -e "${FAKE_ROOT}/rafael-panel"
check "env DSN still contains rafael_panel" grep -q "dbname=rafael_panel" "${ENV_FILE}"
check_no_log "no password leaked into output" "SECRETPW"
check_log "UPDATE SUCCESS reported" "UPDATE SUCCESS"

# =============================================================================
# T3b: flat install that has booted once — the unit's ExecStartPre created
# the self-pointing "current -> ." boot shim. The updater must still
# recognize the install as FLAT (not a release) and migrate it cleanly.
# =============================================================================
expect "T3b flat install with self-pointing current shim migrates"
SBID="flat-shim"; sandbox
make_flat_install v37
# model the unit's ExecStartPre: ln -sfn . /usr/local/rafael/current
# (the text-link shim writes "." into the file on this MSYS host)
printf '%s' "." >"${FAKE_ROOT}/current"
A39="$(make_archive v39)"
patched_updater
run_sb_updater preflight
check_log "preflight still detects FLAT install" "FLAT"
run_sb_updater update "${A39}"
check "migration preserves old build under releases/v37" test -f "${FAKE_ROOT}/releases/v37/rafael-panel"
check "current points at v39 (shim replaced)" current_points_at v39
check "flat duplicates tidied (rafael-panel moved out of root)" test ! -e "${FAKE_ROOT}/rafael-panel"
check "env DSN still contains rafael_panel" grep -q "dbname=rafael_panel" "${ENV_FILE}"
check_log "UPDATE SUCCESS reported" "UPDATE SUCCESS"

# =============================================================================
# T4: archive validation - required structure
# =============================================================================
expect "T4 archive structure validation"
SBID="bad-structure"; sandbox
patched_updater
run_sb_updater preflight "${SBROOT}/nonexistent.tar.gz"
check_log "nonexistent archive rejected" "PREFLIGHT FAIL"
mkdir -p "${SBROOT}/partial/rafael"
make_fake_elf "${SBROOT}/partial/rafael/rafael-panel"
tar -czf "${SBROOT}/partial.tar.gz" -C "${SBROOT}/partial" rafael
run_sb_updater preflight "${SBROOT}/partial.tar.gz"
check_log "incomplete archive rejected" "PREFLIGHT FAIL"
check_log "missing file named in output" "missing required file"

# =============================================================================
# T5: traversal rejection
# =============================================================================
expect "T5 path traversal rejection"
SBID="traversal"; sandbox
mkdir -p "${SBROOT}/evil/rafael" "${SBROOT}/evil/escape"
make_fake_elf "${SBROOT}/evil/rafael/rafael-panel"
echo "pwned" >"${SBROOT}/evil/escape/pwned"
tar -czf "${SBROOT}/evil.tar.gz" -C "${SBROOT}/evil" --transform 's|^escape|..|' rafael escape 2>/dev/null
patched_updater
run_sb_updater preflight "${SBROOT}/evil.tar.gz"
check_log "traversal archive rejected" "PREFLIGHT FAIL"
check_log "traversal reason reported" "outside 'rafael/'"

# =============================================================================
# T6: missing required file rejection
# =============================================================================
expect "T6 missing required file rejection"
SBID="missing-file"; sandbox
mkdir -p "${SBROOT}/nofile/rafael/bin"
make_fake_elf "${SBROOT}/nofile/rafael/rafael-panel"
make_fake_elf "${SBROOT}/nofile/rafael/bin/xray-linux-amd64"
{ echo '#!/bin/bash'; echo 'CLI_BUILD_VERSION="v39"'; } >"${SBROOT}/nofile/rafael/rafael.sh"
tar -czf "${SBROOT}/nofile.tar.gz" -C "${SBROOT}/nofile" rafael   # bootstrap.sh missing
patched_updater
run_sb_updater preflight "${SBROOT}/nofile.tar.gz"
check_log "archive missing bootstrap.sh rejected" "PREFLIGHT FAIL"
check_log "missing file named in output" "rafael/bootstrap.sh"

# =============================================================================
# T7: wrong-architecture rejection (service untouched)
# =============================================================================
expect "T7 wrong architecture rejection"
SBID="wrong-arch"; sandbox
make_flat_install v37
mkdir -p "${SBROOT}/arm/rafael/bin"
printf '\x7fELF\x01' >"${SBROOT}/arm/rafael/rafael-panel"     # 32-bit ELF
head -c 64 /dev/zero >>"${SBROOT}/arm/rafael/rafael-panel"
make_fake_elf "${SBROOT}/arm/rafael/bin/xray-linux-amd64"
{ echo '#!/bin/bash'; echo 'CLI_BUILD_VERSION="v39"'; } >"${SBROOT}/arm/rafael/rafael.sh"
echo '#!/bin/bash' >"${SBROOT}/arm/rafael/bootstrap.sh"
cp "${UNIT_TEMPLATE}" "${SBROOT}/arm/rafael/rafael.service.debian"
cp "${UPDATER}" "${SBROOT}/arm/rafael/rafael-updater.sh"
cp "${LAUNCHER_TEMPLATE}" "${SBROOT}/arm/rafael/rafael-launcher.sh"
tar -czf "${SBROOT}/arm.tar.gz" -C "${SBROOT}/arm" rafael
patched_updater
run_sb_updater update "${SBROOT}/arm.tar.gz"
check_log "wrong-arch binary rejected during validation" "VALIDATION FAIL"
check "no v39 release dir created" test ! -e "${FAKE_ROOT}/releases/v39"
check "flat install untouched" test -f "${FAKE_ROOT}/rafael-panel"
check "current untouched (still flat)" test ! -e "${FAKE_ROOT}/current"

# =============================================================================
# T8: build-version validation (missing/invalid marker)
# =============================================================================
expect "T8 build-version marker validation"
SBID="bad-marker"; sandbox
mkdir -p "${SBROOT}/nom/rafael/bin"
make_fake_elf "${SBROOT}/nom/rafael/rafael-panel"
make_fake_elf "${SBROOT}/nom/rafael/bin/xray-linux-amd64"
echo '#!/bin/bash' >"${SBROOT}/nom/rafael/rafael.sh"           # no marker
echo '#!/bin/bash' >"${SBROOT}/nom/rafael/bootstrap.sh"
cp "${UNIT_TEMPLATE}" "${SBROOT}/nom/rafael/rafael.service.debian"
cp "${UPDATER}" "${SBROOT}/nom/rafael/rafael-updater.sh"
cp "${LAUNCHER_TEMPLATE}" "${SBROOT}/nom/rafael/rafael-launcher.sh"
tar -czf "${SBROOT}/nom.tar.gz" -C "${SBROOT}/nom" rafael
patched_updater
run_sb_updater preflight "${SBROOT}/nom.tar.gz"
check_log "archive without build marker rejected" "PREFLIGHT FAIL"

mkdir -p "${SBROOT}/inj/rafael/bin"
make_fake_elf "${SBROOT}/inj/rafael/rafael-panel"
make_fake_elf "${SBROOT}/inj/rafael/bin/xray-linux-amd64"
{ echo '#!/bin/bash'; echo 'CLI_BUILD_VERSION="v39; rm -rf /"'; } >"${SBROOT}/inj/rafael/rafael.sh"
echo '#!/bin/bash' >"${SBROOT}/inj/rafael/bootstrap.sh"
cp "${UNIT_TEMPLATE}" "${SBROOT}/inj/rafael/rafael.service.debian"
cp "${UPDATER}" "${SBROOT}/inj/rafael/rafael-updater.sh"
cp "${LAUNCHER_TEMPLATE}" "${SBROOT}/inj/rafael/rafael-launcher.sh"
tar -czf "${SBROOT}/inj.tar.gz" -C "${SBROOT}/inj" rafael
run_sb_updater preflight "${SBROOT}/inj.tar.gz"
check_log "injection-style marker rejected (vNN only)" "PREFLIGHT FAIL"

# =============================================================================
# T9: env preservation (unknown keys kept, managed keys rewritten)
# =============================================================================
expect "T9 env preservation on flat migration"
SBID="env-preserve"; sandbox
echo "OPERATOR_FLAG=yes" >>"${ENV_FILE}"
make_flat_install v37
A39="$(make_archive v39)"
patched_updater
run_sb_updater update "${A39}"
check "operator env key preserved" grep -q "^OPERATOR_FLAG=yes$" "${ENV_FILE}"
check "XUI_DB_TYPE still postgres" grep -q "^XUI_DB_TYPE=postgres$" "${ENV_FILE}"
check "XUI_DB_DSN still points at rafael_panel" grep -q "dbname=rafael_panel" "${ENV_FILE}"
check "XUI_BIN_FOLDER present" grep -q "^XUI_BIN_FOLDER=" "${ENV_FILE}"

# =============================================================================
# T10: staging/validation failure (service untouched)
# =============================================================================
expect "T10 staging failure leaves service untouched"
SBID="stage-fail"; sandbox
make_flat_install v37
A39="$(make_archive v39)"
head -c 200 "${A39}" >"${SBROOT}/truncated.tar.gz"
patched_updater
run_sb_updater update "${SBROOT}/truncated.tar.gz"
check_log "truncated archive fails" "UPDATE FAILED"
check "no release dir created" test ! -e "${FAKE_ROOT}/releases/v39"
check "flat install untouched" test -f "${FAKE_ROOT}/rafael-panel"
check "current untouched (still flat)" test ! -e "${FAKE_ROOT}/current"

# =============================================================================
# T11: activation (atomic swap; unit paths; stable layer)
# =============================================================================
expect "T11 atomic activation + unit paths"
SBID="activation"; sandbox
make_flat_install v37
A39="$(make_archive v39)"
patched_updater
run_sb_updater update "${A39}"
check "current points at v39" current_points_at v39
check "installed unit uses current/ paths" grep -q "/usr/local/rafael/current/rafael-panel" "${SB}/systemd-rafael.service"
check "unit has no version-specific paths" bash -c "! grep -qE 'releases/v[0-9]+' '${SB}/systemd-rafael.service'"
check "CLI launcher installed" test -x "${SB}/usr-bin-rafael"
check "release manager installed outside release tree" test -f "${FAKE_ROOT}/rafael-updater.sh"
check "staging dir cleaned up" bash -c "! test -e '${FAKE_ROOT}'/.rafael-stage-*"

# =============================================================================
# T12: health check pass path
# =============================================================================
expect "T12 health check pass"
SBID="health-pass"; sandbox
make_flat_install v37
A39="$(make_archive v39)"
patched_updater
run_sb_updater update "${A39}"
check_log "HEALTH CHECK PASS logged" "HEALTH CHECK PASS"
check_log "UPDATE SUCCESS logged" "UPDATE SUCCESS"

# =============================================================================
# T13: health check fail -> automatic rollback
# =============================================================================
expect "T13 health fail triggers automatic rollback"
SBID="health-fail"; sandbox
make_flat_install v37
A39="$(make_archive v39)"
patched_updater
export FAKE_SVC_MODE="crash-v39"
run_sb_updater update "${A39}"
unset FAKE_SVC_MODE
check_log "health check failed" "HEALTH CHECK FAIL"
check_log "UPDATE FAILED reported" "UPDATE FAILED"
check "rolled back to v37" current_points_at v37
check "v39 retained for inspection" test -d "${FAKE_ROOT}/releases/v39"
check_log "ROLLBACK PASS logged" "ROLLBACK PASS"

# =============================================================================
# T14: rollback when current release is broken
# =============================================================================
expect "T14 rollback works when current release is broken"
SBID="broken-current"; sandbox
mkdir -p "${FAKE_ROOT}/releases/v37/bin" "${FAKE_ROOT}/releases/v39broken"
make_fake_elf "${FAKE_ROOT}/releases/v37/rafael-panel"
make_fake_elf "${FAKE_ROOT}/releases/v37/bin/xray-linux-amd64"
touch "${FAKE_ROOT}/releases/v37/rafael.sh" "${FAKE_ROOT}/releases/v37/bootstrap.sh"
set_current v39broken   # gutted release: no files inside
patched_updater
run_sb_updater rollback
check "rollback switched current to v37" current_points_at v37
check_log "ROLLBACK PASS logged" "ROLLBACK PASS"

# =============================================================================
# T15: release retention
# =============================================================================
expect "T15 retention prunes old, never active"
SBID="retention"; sandbox
mkdir -p "${FAKE_ROOT}/releases"
for v in v30 v31 v32 v33 v39; do
    mkdir -p "${FAKE_ROOT}/releases/${v}"
    touch "${FAKE_ROOT}/releases/${v}/rafael.sh"
done
set_current v39
patched_updater
bash -c "
    $(sed -n '/^resolve_current()/,/^}/p;/^prune_releases()/,/^}/p;/^is_valid_release_name()/,/^}/p;/^validate_path_under_root()/,/^}/p;/^log()/,/^}/p' "${SB}/updater.sh")
    ROOT_DIR='${FAKE_ROOT}'
    RELEASES_DIR='${FAKE_ROOT}/releases'
    CURRENT_LINK='${FAKE_ROOT}/current'
    KEEP_RELEASES=3
    prune_releases
" >/dev/null 2>&1
check "v30 pruned" test ! -d "${FAKE_ROOT}/releases/v30"
check "v31 pruned" test ! -d "${FAKE_ROOT}/releases/v31"
check "newest 3 kept (v32)" test -d "${FAKE_ROOT}/releases/v32"
check "v33 kept" test -d "${FAKE_ROOT}/releases/v33"
check "active kept (v39)" test -d "${FAKE_ROOT}/releases/v39"

# =============================================================================
# T16: locking (concurrent runs)
# =============================================================================
expect "T16 concurrent update locking"
SBID="locking"; sandbox
make_flat_install v37
patched_updater
run_sb_updater status   # first run: takes the lock, releases it on exit
check_log "first run succeeds" "Rafael release status"
# hold the lock the way a live concurrent updater would: live PID in holder
echo $$ >"${SB}/flock-holder"
run_sb_updater status
check_stderr "second concurrent run refused" "Another rafael-updater process"
rm -f "${SB}/flock-holder"
run_sb_updater status
check_log "run succeeds after lock released" "Rafael release status"

# =============================================================================
# T17: idempotency (same version already active + healthy)
# =============================================================================
expect "T17 idempotent update no-op"
SBID="idempotent"; sandbox
mkdir -p "${FAKE_ROOT}/releases/v39"
touch "${FAKE_ROOT}/releases/v39/rafael.sh"
set_current v39
A39="$(make_archive v39)"
patched_updater
run_sb_updater update "${A39}"
check_log "idempotent no-op reported" "idempotent no-op"
check "no duplicate release created" bash -c "! test -e '${FAKE_ROOT}/releases/v39.replaced-*'"
check_no_log "no pg_dump on no-op" "pg_dump"

# =============================================================================
# T18: unit template has no version-specific paths (static check)
# =============================================================================
expect "T18 unit template static check"
SBID="unit-static"; sandbox
check "unit ExecStart uses current symlink" grep -q "ExecStart=/usr/local/rafael/current/rafael-panel" "${UNIT_TEMPLATE}"
check "unit WorkingDirectory uses current" grep -q "WorkingDirectory=/usr/local/rafael/current" "${UNIT_TEMPLATE}"
check "unit has no releases/vNN path" bash -c "! grep -qE 'releases/v[0-9]+' '${UNIT_TEMPLATE}'"
check "launcher routes control cmds to manager" grep -q "rafael-updater.sh" "${LAUNCHER_TEMPLATE}"
check "launcher lives outside release tree in unit flow" grep -q "ROOT_DIR=\"/usr/local/rafael\"" "${LAUNCHER_TEMPLATE}"

# =============================================================================
# T12b (F2 hardening): panel up but the Xray CORE process dead -> the health
# check must FAIL (web-UI-only probing is a false positive) and roll back.
# =============================================================================
expect "T12b health fail (xray core dead) triggers automatic rollback"
SBID="xray-down"; sandbox
make_flat_install v37
A39="$(make_archive v39)"
patched_updater
export FAKE_SVC_MODE="xray-down-v39"
run_sb_updater update "${A39}"
unset FAKE_SVC_MODE
check_log "xray process failure detected" "Xray core process is not running"
check_log "health check failed" "HEALTH CHECK FAIL"
check_log "UPDATE FAILED reported" "UPDATE FAILED"
check "rolled back to v37" current_points_at v37
check_log "ROLLBACK PASS logged" "ROLLBACK PASS"

# =============================================================================
# T19 (F1): injected failure at EVERY migration boundary. The migration must
# never install the flat unit; after the flat files move, the installed unit
# must be current-based and current must resolve to a bootable release; v37
# must stay recoverable and rollback must work at every point.
# Boundaries: after v37 copy / after current->v37 / after current-based unit
# install / after env rewrite / after daemon-reload / after migration restart
# / during tidy / after tidy / during stable-layer install / during v39
# stable-layer refresh / during v39 activation.
# =============================================================================

expect "T19b1 failure right after v37 copy"
INJECT_MODE="after"
INJECT_ANCHOR='
        migrate_flat_install "${flat_marker}" || die "flat-install migration failed."'
INJECT_TEXT='
        die "INJECTED: after v37 copy"'
boundary_run "b1-after-copy"
check_log "injected failure hit" "INJECTED: after v37 copy"
check "no current link yet (flat layout still running)" test ! -e "${FAKE_ROOT}/current"
check "installed unit still the flat Build 37 unit" unit_is_flat
check "flat install still complete (boots as-is)" test -f "${FAKE_ROOT}/rafael-panel" -a -f "${FAKE_ROOT}/bin/xray-linux-amd64"
check "v37 preserved under releases/" v37_recoverable
check "rollback to v37 still works" rollback_to_v37_works

expect "T19b2 failure right after current->v37"
INJECT_MODE="after"
INJECT_ANCHOR='
        activate_release "${flat_marker}" || die "could not point current at preserved ${flat_marker}."'
INJECT_TEXT='
        die "INJECTED: after current->v37"'
boundary_run "b2-after-activate"
check_log "injected failure hit" "INJECTED: after current->v37"
check "current points at v37" current_points_at v37
check "unit still flat-based (not yet switched)" unit_is_flat
check "flat files still in place (flat unit still bootable)" test -f "${FAKE_ROOT}/rafael-panel" -a -f "${FAKE_ROOT}/bin/xray-linux-amd64"
check "v37 bootable through current" current_is_bootable v37
check "rollback to v37 still works" rollback_to_v37_works

expect "T19b3 failure right after current-based unit install"
INJECT_MODE="after"
INJECT_ANCHOR='
        install_unit_from_release "${staged}"'
INJECT_TEXT='
        die "INJECTED: after current-based unit install"'
boundary_run "b3-after-unit"
check_log "injected failure hit" "INJECTED: after current-based unit install"
check "current points at v37" current_points_at v37
check "installed unit is the current-based unit" unit_is_current_based
check "no flat-path unit remains installed" no_flat_unit_installed
check "systemd target bootable (current -> v37)" current_is_bootable v37
check "v37 recoverable" v37_recoverable
check "rollback to v37 still works" rollback_to_v37_works

expect "T19b4 failure right after XUI_BIN_FOLDER rewrite"
INJECT_MODE="after"
INJECT_ANCHOR='
        set_env_bin_folder "${ROOT_DIR}/current/bin" || die "could not update XUI_BIN_FOLDER in ${ENV_FILE} before restart."'
INJECT_TEXT='
        die "INJECTED: after env rewrite"'
boundary_run "b4-after-env"
check_log "injected failure hit" "INJECTED: after env rewrite"
check "current points at v37" current_points_at v37
check "installed unit is the current-based unit" unit_is_current_based
check "env already points at current/bin" grep -qx "XUI_BIN_FOLDER=${FAKE_ROOT}/current/bin" "${ENV_FILE}"
check "systemd target bootable (current -> v37)" current_is_bootable v37
check "v37 recoverable" v37_recoverable
check "rollback to v37 still works" rollback_to_v37_works

expect "T19b5 failure right after migration daemon-reload"
INJECT_MODE="after"
INJECT_ANCHOR='
        systemctl daemon-reload'
INJECT_TEXT='
        die "INJECTED: after migration daemon-reload"'
boundary_run "b5-after-daemon-reload"
check_log "injected failure hit" "INJECTED: after migration daemon-reload"
check "current points at v37" current_points_at v37
check "installed unit is the current-based unit" unit_is_current_based
check "env already points at current/bin" grep -qx "XUI_BIN_FOLDER=${FAKE_ROOT}/current/bin" "${ENV_FILE}"
check "systemd target bootable (current -> v37)" current_is_bootable v37
check "v37 recoverable" v37_recoverable
check "rollback to v37 still works" rollback_to_v37_works

expect "T19b6 failure right after the v37 migration restart"
INJECT_MODE="replace"
INJECT_ANCHOR='
        tidy_flat_leftovers'
INJECT_TEXT='
        die "INJECTED: after migration restart (before tidy)"'
boundary_run "b6-after-migration-restart"
check_log "injected failure hit" "INJECTED: after migration restart"
check "current points at v37" current_points_at v37
check "installed unit is the current-based unit" unit_is_current_based
check "env points at current/bin" grep -qx "XUI_BIN_FOLDER=${FAKE_ROOT}/current/bin" "${ENV_FILE}"
check "systemd target bootable (current -> v37)" current_is_bootable v37
check "flat duplicates not yet tidied (belt still on)" test -f "${FAKE_ROOT}/rafael-panel"
check "v37 recoverable" v37_recoverable
check "rollback to v37 still works" rollback_to_v37_works

expect "T19b7 failure DURING tidy (flat bin already moved)"
# Simulates a crash inside tidy_flat_leftovers: the flat bin directory has
# been moved away, so a flat-path unit could NO LONGER boot. The system must
# still be bootable — which is only true because the current-based unit and
# current->v37 were installed BEFORE tidy started.
INJECT_MODE="replace"
INJECT_ANCHOR='
        tidy_flat_leftovers'
INJECT_TEXT='
        mv "${ROOT_DIR}/bin" "${ROOT_DIR}/persistent/flat-preserved/.bin-mid-tidy" 2>/dev/null || true
        die "INJECTED: during tidy (flat bin moved mid-tidy)"'
boundary_run "b7-during-tidy"
check_log "injected failure hit" "INJECTED: during tidy"
check "installed unit is the current-based unit (flat unit could not boot: bin moved)" unit_is_current_based
check "no flat-path unit remains" no_flat_unit_installed
check "current -> v37 and everything the unit references resolves" current_is_bootable v37
check "env points at current/bin" grep -qx "XUI_BIN_FOLDER=${FAKE_ROOT}/current/bin" "${ENV_FILE}"
check "v37 recoverable" v37_recoverable
check "rollback to v37 still works" rollback_to_v37_works

expect "T19b8 failure right after tidy completed"
INJECT_MODE="replace"
INJECT_ANCHOR='
        tidy_flat_leftovers'
INJECT_TEXT='
        tidy_flat_leftovers
        die "INJECTED: after tidy"'
boundary_run "b8-after-tidy"
check_log "injected failure hit" "INJECTED: after tidy"
check "installed unit is the current-based unit" unit_is_current_based
check "no flat-path unit remains" no_flat_unit_installed
check "current -> v37 and everything the unit references resolves" current_is_bootable v37
check "flat bin preserved (not deleted)" test -f "${FAKE_ROOT}/persistent/flat-preserved/bin/xray-linux-amd64"
check "env points at current/bin" grep -qx "XUI_BIN_FOLDER=${FAKE_ROOT}/current/bin" "${ENV_FILE}"
check "v37 recoverable" v37_recoverable
check "rollback to v37 still works" rollback_to_v37_works

expect "T19b9 failure during stable-layer installation (migration block)"
INJECT_MODE="replace"
INJECT_ANCHOR='
        install_stable_layer "${staged}"'
INJECT_TEXT='
        install -m 755 "${staged}/${UPDATER_NAME}" "${ROOT_DIR}/${UPDATER_NAME}"
        die "INJECTED: during stable-layer installation"'
boundary_run "b9-during-stable-layer"
check_log "injected failure hit" "INJECTED: during stable-layer installation"
check "current points at v37" current_points_at v37
check "installed unit is the current-based unit" unit_is_current_based
check "env points at current/bin" grep -qx "XUI_BIN_FOLDER=${FAKE_ROOT}/current/bin" "${ENV_FILE}"
check "systemd target bootable (current -> v37)" current_is_bootable v37
check "release manager present at root (control commands work)" test -f "${FAKE_ROOT}/rafael-updater.sh"
check "rollback to v37 still works" rollback_to_v37_works

expect "T19b10 failure during v39 stable-layer refresh (step 7)"
INJECT_MODE="replace"
INJECT_ANCHOR='
    install_stable_layer "${staged}"'
INJECT_TEXT='
    install -m 755 "${staged}/${UPDATER_NAME}" "${ROOT_DIR}/${UPDATER_NAME}"
    die "INJECTED: during v39 stable-layer refresh"'
boundary_run "b10-v39-stable-layer"
check_log "injected failure hit" "INJECTED: during v39 stable-layer refresh"
check "current still points at v37 (v39 NOT activated)" current_points_at v37
check "installed unit is the current-based unit" unit_is_current_based
check "env points at current/bin" grep -qx "XUI_BIN_FOLDER=${FAKE_ROOT}/current/bin" "${ENV_FILE}"
check "v39 never reached releases/ (die fired before the move; staging cleaned)" bash -c "! test -e '${FAKE_ROOT}/releases/v39' && ! test -e '${FAKE_ROOT}'/.rafael-stage-*"
check "systemd target bootable (current -> v37)" current_is_bootable v37
check "rollback to v37 still works" rollback_to_v37_works

expect "T19b11 failure during v39 activation (atomic swap)"
# Inject BEFORE step 8: v39 is staged into releases/ and the migration has
# completed (current -> v37), but the activation swap has not run yet. (The
# then-branch of "if ! activate_release" only runs on FAILURE, so injecting
# inside it would be dead code on the success path; patching activate_release
# itself would hit its first caller — the migration — instead.)
INJECT_MODE="after"
INJECT_ANCHOR='
    # --- 8. atomic activation ------------------------------------------------------'
INJECT_TEXT='
    die "INJECTED: during v39 activation (after staging, before swap)"'
boundary_run "b11-v39-activation"
check_log "injected failure hit" "INJECTED: during v39 activation"
check_log "UPDATE FAILED reported" "UPDATE FAILED"
check "current still points at v37" current_points_at v37
check "installed unit is the current-based unit" unit_is_current_based
check "v39 retained for inspection" test -d "${FAKE_ROOT}/releases/v39"
check "env points at current/bin" grep -qx "XUI_BIN_FOLDER=${FAKE_ROOT}/current/bin" "${ENV_FILE}"
check "systemd target bootable (current -> v37)" current_is_bootable v37
check "rollback to v37 still works" rollback_to_v37_works

# =============================================================================
# T20 (F2 regression): XUI_BIN_FOLDER must be current/bin BEFORE the first
# restart that can occur after the flat bin directory moves. The fake
# systemctl snapshots the env file's XUI_BIN_FOLDER at every restart —
# exactly what systemd loads from EnvironmentFile at unit start. With the
# pre-fix updater, the migration restart would load the FLAT bin path.
# =============================================================================
expect "T20 env rewritten before every restart (F2 regression)"
SBID="env-before-restart"; sandbox
make_flat_install v37
A39="$(make_archive v39)"
patched_updater
run_sb_updater update "${A39}"
check_log "UPDATE SUCCESS reported" "UPDATE SUCCESS"
check "final env points at current/bin" grep -qx "XUI_BIN_FOLDER=${FAKE_ROOT}/current/bin" "${ENV_FILE}"
check "migration restart loaded current/bin (not the flat bin)" \
    test "$(sed -n '1p' "${FAKE_ROOT}/systemctl-log/env-at-restarts" 2>/dev/null)" = "${FAKE_ROOT}/current/bin"
check "v39 activation restart loaded current/bin" \
    test "$(sed -n '2p' "${FAKE_ROOT}/systemctl-log/env-at-restarts" 2>/dev/null)" = "${FAKE_ROOT}/current/bin"
check "no restart ever loaded the flat bin path" \
    bash -c "! grep -qx '${FAKE_ROOT}/bin' '${FAKE_ROOT}/systemctl-log/env-at-restarts'"

# =============================================================================
# T21 (F2 model): set_env_bin_folder semantics + systemd EnvironmentFile
# ordering. systemd captures EnvironmentFile BEFORE ExecStartPre runs, so a
# bootstrap.sh rewrite only takes effect on the NEXT start — the failure mode
# the updater-side write eliminates. The fixture's bootstrap.sh is a stub
# that writes NOTHING, so any correct env value observed in the flow tests
# was written by the updater itself.
# =============================================================================
expect "T21 set_env_bin_folder + EnvironmentFile ordering (F2 model)"
SBID="env-ordering"; sandbox
ENVTEST="${SB}/envtest"
mkdir -p "${ENVTEST}"
cp "${ENV_FILE}" "${ENVTEST}/env"
echo "OPERATOR_FLAG=yes" >>"${ENVTEST}/env"
# run the REAL set_env_bin_folder (extracted from the updater source) against
# a throwaway copy of the env file
cat >"${SB}/envfn.sh" <<'ENVEOF'
#!/bin/bash
set -euo pipefail
ENV_FILE_TARGET="$1"
eval "$(sed -n '/^log()/,/^}/p;/^set_env_bin_folder()/,/^}/p' "${UPDATER_SRC}")"
ENV_FILE="${ENV_FILE_TARGET}"
set_env_bin_folder "/usr/local/rafael/current/bin"
ENVEOF
UPDATER_SRC="${UPDATER}" bash "${SB}/envfn.sh" "${ENVTEST}/env"
check "set_env_bin_folder rewrites XUI_BIN_FOLDER" grep -qx "XUI_BIN_FOLDER=/usr/local/rafael/current/bin" "${ENVTEST}/env"
check "set_env_bin_folder preserves operator keys" grep -qx "OPERATOR_FLAG=yes" "${ENVTEST}/env"
check "set_env_bin_folder preserves the DSN" grep -q "dbname=rafael_panel" "${ENVTEST}/env"
check "set_env_bin_folder leaves no temp file behind" bash -c "! test -e '${ENVTEST}'/env.tmp.*"

# ordering model, pre-fix behavior: ExecStartPre-only correction is captured
# one start too late (this documents WHY the updater must write the env).
cp "${ENV_FILE}" "${ENVTEST}/env2"
captured_at_start="$(sed -n 's/^XUI_BIN_FOLDER=//p' "${ENVTEST}/env2" | tail -n1)"   # what systemd loads
printf 'XUI_BIN_FOLDER=/usr/local/rafael/current/bin\n' >"${ENVTEST}/env2.new"        # what bootstrap writes
grep -vE '^XUI_BIN_FOLDER=' "${ENVTEST}/env2" >>"${ENVTEST}/env2.new"
mv -f "${ENVTEST}/env2.new" "${ENVTEST}/env2"
check "model: pre-fix, systemd captured the FLAT path despite the bootstrap rewrite" \
    test "${captured_at_start}" != "/usr/local/rafael/current/bin"
# ordering model, fixed behavior: the updater writes the env BEFORE the
# restart, so what systemd captures is already current/bin.
cp "${ENV_FILE}" "${ENVTEST}/env3"
UPDATER_SRC="${UPDATER}" bash "${SB}/envfn.sh" "${ENVTEST}/env3"
captured_fixed="$(sed -n 's/^XUI_BIN_FOLDER=//p' "${ENVTEST}/env3" | tail -n1)"
check "model: updater-first write means systemd captures current/bin" \
    test "${captured_fixed}" = "/usr/local/rafael/current/bin"

# =============================================================================
# T22-T24 + extras (F3): malicious archives must be rejected BEFORE any
# extraction — no staging dir, no extracted file, service never touched.
# =============================================================================
expect "T22 malicious archive: symlink member rejected (F3)"
SBID="evil-symlink"; sandbox
make_flat_install v37
make_evil_tar "${SBROOT}/evil-sym.tar.gz" symlink
patched_updater
run_sb_updater update "${SBROOT}/evil-sym.tar.gz"
check_log "symlink archive rejected" "VALIDATION FAIL"
check_log "rejection names non-regular members" "non-regular members"
check "nothing extracted (no staging dir)" bash -c "! test -e '${FAKE_ROOT}'/.rafael-stage-*"
check "flat install untouched" test -f "${FAKE_ROOT}/rafael-panel"
check "current untouched (still flat)" test ! -e "${FAKE_ROOT}/current"
check "service never restarted" bash -c "! grep -q restart '${FAKE_ROOT}/systemctl-log/log' 2>/dev/null"

expect "T23 malicious archive: hardlink member rejected (F3)"
SBID="evil-hardlink"; sandbox
make_flat_install v37
make_evil_tar "${SBROOT}/evil-hard.tar.gz" hardlink
patched_updater
run_sb_updater update "${SBROOT}/evil-hard.tar.gz"
check_log "hardlink archive rejected" "VALIDATION FAIL"
check_log "rejection names non-regular members" "non-regular members"
check "nothing extracted (no staging dir)" bash -c "! test -e '${FAKE_ROOT}'/.rafael-stage-*"
check "service never restarted" bash -c "! grep -q restart '${FAKE_ROOT}/systemctl-log/log' 2>/dev/null"

expect "T24 malicious archive: symlink + child-file escape attempt (F3)"
SBID="evil-symlink-child"; sandbox
make_flat_install v37
mkdir -p "${SB}/escape-target"
echo "sentinel" >"${SB}/escape-target/sentinel.txt"
# the symlink points OUTSIDE the future staging dir; if extraction followed
# it, rafael/x/evil would land in the escape target
make_evil_tar "${SBROOT}/evil-symchild.tar.gz" symlink-child "${SB}/escape-target"
patched_updater
run_sb_updater update "${SBROOT}/evil-symchild.tar.gz"
check_log "symlink+child archive rejected" "VALIDATION FAIL"
check_log "rejection names non-regular members" "non-regular members"
check "nothing extracted (no staging dir)" bash -c "! test -e '${FAKE_ROOT}'/.rafael-stage-*"
check "no file escaped into the symlink target" test ! -e "${SB}/escape-target/evil"
check "sentinel file untouched" test -f "${SB}/escape-target/sentinel.txt"
check "service never restarted" bash -c "! grep -q restart '${FAKE_ROOT}/systemctl-log/log' 2>/dev/null"

expect "T22e malicious archive: absolute path rejected before extraction"
SBID="evil-absolute"; sandbox
make_flat_install v37
make_evil_tar "${SBROOT}/evil-abs.tar.gz" absolute
patched_updater
run_sb_updater update "${SBROOT}/evil-abs.tar.gz"
check_log "absolute-path archive rejected" "VALIDATION FAIL"
check "nothing extracted (no staging dir)" bash -c "! test -e '${FAKE_ROOT}'/.rafael-stage-*"
check "service never restarted" bash -c "! grep -q restart '${FAKE_ROOT}/systemctl-log/log' 2>/dev/null"

expect "T23e malicious archive: ../ traversal rejected before extraction"
SBID="evil-traversal"; sandbox
make_flat_install v37
make_evil_tar "${SBROOT}/evil-trav.tar.gz" traversal
patched_updater
run_sb_updater update "${SBROOT}/evil-trav.tar.gz"
check_log "traversal archive rejected" "VALIDATION FAIL"
check "nothing extracted (no staging dir)" bash -c "! test -e '${FAKE_ROOT}'/.rafael-stage-*"
check "no file written outside staging" test ! -e "${SBROOT}/pwned"
check "service never restarted" bash -c "! grep -q restart '${FAKE_ROOT}/systemctl-log/log' 2>/dev/null"

expect "T24e malicious archive: unexpected top-level path rejected"
SBID="evil-toplevel"; sandbox
make_flat_install v37
make_evil_tar "${SBROOT}/evil-top.tar.gz" toplevel
patched_updater
run_sb_updater update "${SBROOT}/evil-top.tar.gz"
check_log "unexpected top-level archive rejected" "VALIDATION FAIL"
check "nothing extracted (no staging dir)" bash -c "! test -e '${FAKE_ROOT}'/.rafael-stage-*"
check "service never restarted" bash -c "! grep -q restart '${FAKE_ROOT}/systemctl-log/log' 2>/dev/null"

expect "T25e malicious archive: valid release + one symlink member (mixed)"
SBID="evil-mixed"; sandbox
make_flat_install v37
make_evil_tar "${SBROOT}/evil-mixed.tar.gz" mixed
patched_updater
run_sb_updater update "${SBROOT}/evil-mixed.tar.gz"
check_log "mixed valid+malicious archive rejected" "VALIDATION FAIL"
check_log "rejection names non-regular members" "non-regular members"
check "nothing extracted despite otherwise-valid structure" bash -c "! test -e '${FAKE_ROOT}'/.rafael-stage-*"
check "no v39 release dir created" test ! -e "${FAKE_ROOT}/releases/v39"
check "service never restarted" bash -c "! grep -q restart '${FAKE_ROOT}/systemctl-log/log' 2>/dev/null"

# =============================================================================
# T25: the flat fixture uses the REAL Build 37 flat unit (the fixture defect
# that previously masked F1), and migration keeps releases/v37 faithful.
# =============================================================================
expect "T25 flat fixture uses the REAL Build 37 flat unit"
SBID="fixture-real-unit"; sandbox
make_flat_install v37
# The faithful reference is the unit inside the immutable build-v37 release
# archive (the deployed Build 37 artifact), NOT git HEAD — the working-tree
# unit has since gained the flat-boot-tolerant v40 changes, so comparing
# against HEAD would fail on every unit change by design.
V37_UNIT_ARCHIVE="${PROJECT}/build-v37/rafael-linux-amd64.tar.gz"
check "fixture unit is byte-identical to the real Build 37 release unit (archive)" \
    bash -c "tar -xzOf '${V37_UNIT_ARCHIVE}' rafael/rafael.service.debian | cmp -s - '${FAKE_ROOT}/rafael.service.debian'"
check "installed fixture unit is flat-based" unit_is_flat
check "fixture unit has no current/ references" bash -c "! grep -q '/usr/local/rafael/current' '${SB}/systemd-rafael.service'"
check "installed unit == flat release unit file" cmp -s "${SB}/systemd-rafael.service" "${FAKE_ROOT}/rafael.service.debian"
A39="$(make_archive v39)"
patched_updater
run_sb_updater update "${A39}"
check_log "UPDATE SUCCESS reported" "UPDATE SUCCESS"
check "releases/v37 keeps the faithful Build 37 flat unit (byte-identical)" \
    bash -c "tar -xzOf '${V37_UNIT_ARCHIVE}' rafael/rafael.service.debian | cmp -s - '${FAKE_ROOT}/releases/v37/rafael.service.debian'"
check "the FLAT unit was never installed as the systemd unit (current-based one is)" unit_is_current_based

# =============================================================================
# T26: static source audit — the migration path must never install the unit
# from the preserved flat release, and no flat systemd paths may be wired in.
# =============================================================================
expect "T26 static source audit (flat systemd paths)"
SBID="static-audit"; sandbox
check "no install_unit_from_release call on the flat release dir" \
    bash -c "! grep -qF 'install_unit_from_release \"\${RELEASES_DIR}/\${flat_marker}\"' '${UPDATER}'"
check "exactly two unit installs: staged (migration) + target_dir (activation)" \
    bash -c "test \"\$(grep -cF 'install_unit_from_release \"\${' '${UPDATER}')\" -eq 2"
check "migration installs the unit from the STAGED (validated) release" \
    grep -qF 'install_unit_from_release "${staged}"' "${UPDATER}"
check "no flat ExecStart path anywhere in the updater" \
    bash -c "! grep -qF 'ExecStart=/usr/local/rafael/rafael-panel' '${UPDATER}'"
check "set_env_bin_folder is called in the migration block" \
    grep -qF 'set_env_bin_folder "${ROOT_DIR}/current/bin"' "${UPDATER}"
check "check_no_special_members is wired into validate_archive" \
    grep -qF 'check_no_special_members "${archive}"' "${UPDATER}"
check "install-local.sh derives the flat unit for fresh installs (sed, not cp)" \
    grep -qF "sed 's|/usr/local/rafael/current|/usr/local/rafael|g'" "${PROJECT}/install-local.sh"

# =============================================================================
# SHA-256 manifest verification
# =============================================================================
expect "SHA-256 manifest (wrong hash rejected)"
SBID="sha256-wrong"; sandbox
make_flat_install v37
A39="$(make_archive v39)"
echo "0000000000000000000000000000000000000000000000000000000000000000  rafael-v39.tar.gz" >"${A39}.sha256"
patched_updater
run_sb_updater preflight "${A39}"
check_log "wrong SHA-256 rejected" "PREFLIGHT FAIL"
check_log "SHA-256 mismatch reported" "SHA-256 mismatch"

expect "SHA-256 manifest (correct hash passes)"
SBID="sha256-ok"; sandbox
make_flat_install v37
A39="$(make_archive v39)"
sha256sum "${A39}" | sed "s| .*/| rafael-v39.tar.gz|" >"${A39}.sha256"
patched_updater
run_sb_updater preflight "${A39}"
check_log "correct SHA-256 verified" "SHA-256 verified"
check_log "preflight passed" "PREFLIGHT PASS"

# =============================================================================
# P: dedicated http_health_probe test (REAL function, runnable fake panel)
#    Not reachable via the flow tests on this host, so exercise the actual
#    function body directly: port/basePath parsing, scheme detection, code
#    acceptance, code rejection.
# =============================================================================
expect "P http_health_probe real-logic test"
SBID="probe"; sandbox
mkdir -p "${SB}/current"
# runnable fake panel: prints settings like the real `setting` subcommand
cat >"${SB}/current/rafael-panel" <<'EOF'
#!/bin/bash
case "$*" in
    *"setting -show true"*)
        echo "port: 1234"
        echo "webBasePath: /abc/" ;;
    *"-getCert true"*)
        echo "cert: /etc/ssl/panel.pem"
        echo "key: /etc/ssl/panel.key" ;;
    *) : ;;
esac
EOF
chmod +x "${SB}/current/rafael-panel"

# fake curl: exit code from file
cat >"${SBROOT}/bin/curl" <<'EOF'
#!/bin/bash
cat "${FAKE_CURL_CODE_FILE:-/dev/null}"
EOF
chmod +x "${SBROOT}/bin/curl"

cat >"${SB}/curlcode" <<<"200"

probe_run() {
    FAKE_CURL_CODE_FILE="${SB}/curlcode" bash -c "
        CURRENT_LINK='${SB}/current'
        ENV_FILE='${ENV_FILE}'
        ROOT_DIR='${FAKE_ROOT}'
        $(sed -n '/^log()/,/^}/p;/^mask_dsn()/,/^}/p;/^load_panel_env()/,/^}/p;/^is_executable_file()/,/^}/p;/^http_health_probe()/,/^}/p' "${UPDATER}")
        http_health_probe
        exit \$?
    " 2>&1
}

OUT="$(probe_run)"; RC=$?
check "probe returns 0 on HTTP 200" test "${RC}" -eq 0
check "probe built https URL with port+basepath" grep -q "probing https://127.0.0.1:1234/abc/" <<<"${OUT}"

# 404 also acceptable
printf '404' >"${SB}/curlcode"
OUT="$(probe_run)"; RC=$?
check "probe returns 0 on HTTP 404" test "${RC}" -eq 0

# 500 rejected
printf '500' >"${SB}/curlcode"
OUT="$(probe_run)"; RC=$?
check "probe returns 1 on HTTP 500" test "${RC}" -eq 1

# no cert configured -> http scheme
cat >"${SB}/current/rafael-panel" <<'EOF'
#!/bin/bash
case "$*" in
    *"setting -show true"*)
        echo "port: 1234" ;;
    *) : ;;
esac
EOF
chmod +x "${SB}/current/rafael-panel"
printf '200' >"${SB}/curlcode"
OUT="$(probe_run)"; RC=$?
check "probe uses http scheme without cert" grep -q "probing http://127.0.0.1:1234" <<<"${OUT}"

# =============================================================================
# T27-T33: CLI bootstrap bridge — `rafael update <archive>` on a host with NO
#   release manager (legacy Build 37 flat box / fresh flat install).
#   The v39 rafael.sh must bootstrap the manager FROM the archive itself:
#   pin-verify the bundled updater, run the REAL validate_archive on the
#   whole archive, then install the manager and delegate to it.
# =============================================================================

# sandboxed copy of the REAL rafael.sh: ROOT_DIR/ENV_FILE redirected, root
# check relaxed (tests run unprivileged), bootstrap pin set to the hash of
# the sandboxed updater (what the build injects for the bundled one).
patched_cli() {
    local pin="$1"
    sed -e "s|^ROOT_DIR=.*|ROOT_DIR=\"${FAKE_ROOT}\"|" \
        -e "s|^ENV_FILE=.*|ENV_FILE=\"${ENV_FILE}\"|" \
        -e "s|^BOOTSTRAP_UPDATER_SHA256=\"\"|BOOTSTRAP_UPDATER_SHA256=\"${pin}\"|" \
        -e "s|^    if \[\[ \"\${EUID}\" -ne 0 \]\]; then|    if false; then|" \
        "${PROJECT}/rafael.sh" >"${SB}/cli.sh"
    chmod +x "${SB}/cli.sh"
}

# release archive whose rafael/rafael-updater.sh member IS the sandboxed
# updater (run patched_updater first). The bridge installs + execs THIS
# member, so it must carry the sandbox paths.
make_patched_archive() {
    local version="$1"
    [[ -f "${SB}/updater.sh" ]] || { echo "make_patched_archive: patched_updater not run" >&2; exit 2; }
    local dir="${SBROOT}/archive-${SBID}-${version}"
    rm -rf "${dir}"
    mkdir -p "${dir}/rafael/bin"
    make_fake_elf "${dir}/rafael/rafael-panel"
    make_fake_elf "${dir}/rafael/bin/xray-linux-amd64"
    {
        echo '#!/bin/bash'
        echo "CLI_BUILD_VERSION=\"${version}\""
    } >"${dir}/rafael/rafael.sh"
    echo '#!/bin/bash' >"${dir}/rafael/bootstrap.sh"
    cp "${UNIT_TEMPLATE}" "${dir}/rafael/rafael.service.debian"
    cp "${SB}/updater.sh" "${dir}/rafael/rafael-updater.sh"
    cp "${LAUNCHER_TEMPLATE}" "${dir}/rafael/rafael-launcher.sh"
    tar -czf "${SBROOT}/rafael-patched-${version}.tar.gz" -C "${dir}" rafael
    echo "${SBROOT}/rafael-patched-${version}.tar.gz"
}

# legacy FLAT archive (Build 38-era): NO updater/launcher members — the only
# kind manual_update_panel may still process.
make_flat_archive() {
    local version="$1"
    local dir="${SBROOT}/flatarchive-${SBID}-${version}"
    rm -rf "${dir}"
    mkdir -p "${dir}/rafael/bin"
    make_fake_elf "${dir}/rafael/rafael-panel"
    make_fake_elf "${dir}/rafael/bin/xray-linux-amd64"
    {
        echo '#!/bin/bash'
        echo "CLI_BUILD_VERSION=\"${version}\""
    } >"${dir}/rafael/rafael.sh"
    echo '#!/bin/bash' >"${dir}/rafael/bootstrap.sh"
    printf '%s\n' "${FLAT_UNIT_V37}" >"${dir}/rafael/rafael.service.debian"
    tar -czf "${SBROOT}/rafael-flat-${version}.tar.gz" -C "${dir}" rafael
    echo "${SBROOT}/rafael-flat-${version}.tar.gz"
}

run_cli() {
    export FAKE_TEST_ROOT="${FAKE_ROOT}"
    export FAKE_TEST_STATE="${SB}"
    export FAKE_TEST_SETTINGS="${SBROOT}/settings-table"
    export FAKE_TEST_ENV="${ENV_FILE}"
    : >"${SB}/out.log"
    bash "${SB}/cli.sh" "$@" >"${SB}/out.log" 2>&1
}

# --- T27: full bridge migration on a legacy flat install -------------------
expect "T27 CLI bridge: rafael update <archive> bootstraps + migrates flat v37"
SBID="bridge-success"; sandbox
make_flat_install v37
patched_updater
A39="$(make_patched_archive v39)"
PIN="$(sha256sum "${SB}/updater.sh" | awk '{print $1}')"
patched_cli "${PIN}"
run_cli update "${A39}"
RC=$?
check "exit code 0" test "${RC}" -eq 0
check_log "bridge announced the bootstrap" "Release manager bootstrapped"
check_log "bridge delegated to the release manager" "UPDATE SUCCESS"
check "release manager installed by the bridge" test -f "${FAKE_ROOT}/rafael-updater.sh"
check "installed manager is the pinned (sandboxed) updater" cmp -s "${FAKE_ROOT}/rafael-updater.sh" "${SB}/updater.sh"
check "flat v37 preserved as releases/v37" test -f "${FAKE_ROOT}/releases/v37/rafael-panel"
check "current -> v39" current_points_at v39
check "installed unit is the current-based unit" unit_is_current_based
check "env points at current/bin" grep -qx "XUI_BIN_FOLDER=${FAKE_ROOT}/current/bin" "${ENV_FILE}"
check "DSN untouched (dbname preserved)" grep -q "dbname=rafael_panel" "${ENV_FILE}"
check "launcher installed at the CLI alias" test -f "${SB}/usr-bin-rafael"
check "flat bootstrap.sh no longer at ROOT (tidied)" test ! -e "${FAKE_ROOT}/bootstrap.sh"

# --- T28: pin mismatch -> fail closed before anything runs -----------------
expect "T28 CLI bridge refuses an updater that does not match the pin"
SBID="bridge-pin"; sandbox
make_flat_install v37
patched_updater
A39="$(make_patched_archive v39)"
PIN="$(sha256sum "${SB}/updater.sh" | awk '{print $1}')"
# tamper ONLY the updater member, re-pack
TD="${SBROOT}/tamper-${SBID}"; rm -rf "${TD}"; mkdir -p "${TD}"
tar -xzf "${A39}" -C "${TD}"
printf '# tampered\n' >>"${TD}/rafael/rafael-updater.sh"
EVIL="${SBROOT}/rafael-tampered-updater.tar.gz"
tar -czf "${EVIL}" -C "${TD}" rafael
patched_cli "${PIN}"
run_cli update "${EVIL}"
RC=$?
check "refused (non-zero exit)" test "${RC}" -ne 0
check_log "pin-mismatch refusal stated" "Bootstrap refused: bundled updater SHA-256"
check "no release manager installed" test ! -e "${FAKE_ROOT}/rafael-updater.sh"
check "no migration happened (no releases dir)" test ! -e "${FAKE_ROOT}/releases"
check "flat install untouched" test -f "${FAKE_ROOT}/rafael-panel"
check "flat unit still installed" unit_is_flat
check "current still absent (flat)" test ! -e "${FAKE_ROOT}/current"

# --- T29: malicious archive rejected through the REAL validation path ------
expect "T29 CLI bridge: pin-matching archive with a symlink member rejected"
SBID="bridge-evil"; sandbox
make_flat_install v37
patched_updater
make_evil_tar "${SBROOT}/evil-bridge.tar.gz" release-symlink /etc "${SB}/updater.sh"
PIN="$(sha256sum "${SB}/updater.sh" | awk '{print $1}')"
patched_cli "${PIN}"
run_cli update "${SBROOT}/evil-bridge.tar.gz"
RC=$?
check "refused (non-zero exit)" test "${RC}" -ne 0
check_log "rejection came from the real updater validation stack" "Bootstrap refused: archive failed the release updater's validation"
check_log "special-member reason reported" "non-regular members"
check "no release manager installed" test ! -e "${FAKE_ROOT}/rafael-updater.sh"
check "no migration happened" test ! -e "${FAKE_ROOT}/releases"
check "flat install untouched" test -f "${FAKE_ROOT}/rafael-panel"
check "current still absent (flat)" test ! -e "${FAKE_ROOT}/current"

# --- T30: empty pin (unbundled working-tree CLI) fails CLOSED --------------
expect "T30 CLI bridge with no pin refuses to bootstrap (fail closed)"
SBID="bridge-nopin"; sandbox
make_flat_install v37
patched_updater
A39="$(make_patched_archive v39)"
patched_cli ""
run_cli update "${A39}"
RC=$?
check "refused (non-zero exit)" test "${RC}" -ne 0
check_log "no-pin refusal stated" "no updater pin"
check "no release manager installed" test ! -e "${FAKE_ROOT}/rafael-updater.sh"
check "no migration happened" test ! -e "${FAKE_ROOT}/releases"

# --- T31: update-local rejects release-layout archives ----------------------
expect "T31 update-local rejects release-layout archives with guidance"
SBID="updlocal-reject"; sandbox
make_flat_install v37
patched_updater
A39="$(make_patched_archive v39)"
patched_cli "0000000000000000000000000000000000000000000000000000000000000000"
run_cli update-local "${A39}"
RC=$?
check "rejected (non-zero exit)" test "${RC}" -ne 0
check_log "rejection reason stated" "release-layout archive"
check_log "operator pointed at the safe command" "rafael update"
check "no flat overlay happened (rafael.sh still v37)" grep -q 'CLI_BUILD_VERSION="v37"' "${FAKE_ROOT}/rafael.sh"
check "no backup dir created" test -z "$(ls -d "${SB}"/*.backup-* "${FAKE_ROOT}".backup-* 2>/dev/null)"
check "flat unit still installed" unit_is_flat

# --- T32: update-local still serves LEGACY flat archives --------------------
expect "T32 update-local still serves legacy flat archives (prompt, not reject)"
SBID="updlocal-flat"; sandbox
make_flat_install v37
patched_updater
A38="$(make_flat_archive v38)"
patched_cli "0000000000000000000000000000000000000000000000000000000000000000"
run_cli update-local "${A38}" <<<"n"
RC=$?
check "legacy flow entered (build comparison ran, not rejected as release layout)" grep -q "Current CLI build:" "${SB}/out.log"
check "cancelled cleanly at the prompt" grep -q "Update cancelled." "${SB}/out.log"
check "flat install untouched" test -f "${FAKE_ROOT}/rafael-panel"
check "flat unit still installed" unit_is_flat

# --- T33: update with no manager and no archive gives guidance -------------
expect "T33 update without manager/archive prints guidance, does nothing"
SBID="upd-noguidance"; sandbox
make_flat_install v37
patched_cli "0000000000000000000000000000000000000000000000000000000000000000"
run_cli update
RC=$?
check "non-zero exit" test "${RC}" -ne 0
check_log "guidance mentions the explicit archive form" "rafael update /path/to/rafael-linux-amd64.tar.gz"
check "no migration happened" test ! -e "${FAKE_ROOT}/releases"

# =============================================================================
# T34-T37: remaining failure-injection boundaries (Part 15 completion):
#   v37 health failure during migration (flat-restore path) / v39 restart
#   failure / stale Xray core process (old path still running) / interrupted
#   v37 copy re-copied on retry.
# =============================================================================

# --- T34: migration-time v37 health failure -> dedicated flat restore -------
expect "T34 migration v37 health failure restores the flat layout"
SBID="v37-health-fail"; sandbox
make_flat_install v37
A39="$(make_archive v39)"
patched_updater
export FAKE_SVC_MODE="down"
run_sb_updater update "${A39}"
unset FAKE_SVC_MODE
check_log "flat-restore path taken" "restoring flat unit, env and layout"
check_log "UPDATE FAILED reported" "UPDATE FAILED"
check_no_log "no false success" "UPDATE SUCCESS"
check "flat layout restored (no current link)" test ! -e "${FAKE_ROOT}/current"
check "flat unit restored" unit_is_flat
check "env restored to the flat bin folder" grep -qx "XUI_BIN_FOLDER=${FAKE_ROOT}/bin" "${ENV_FILE}"
check "flat install intact (boots as-is)" test -f "${FAKE_ROOT}/rafael-panel" -a -f "${FAKE_ROOT}/bin/xray-linux-amd64"
check "DSN preserved through the restore" grep -q "dbname=rafael_panel" "${ENV_FILE}"
check "no v39 release activated" test ! -e "${FAKE_ROOT}/releases/v39"
# retry after the restore must succeed (self-healing)
patched_updater
run_sb_updater update "${A39}"
check_log "retry after flat restore succeeds" "UPDATE SUCCESS"
check "retry migrated cleanly" current_points_at v39
check "v37 preserved as a release after retry" v37_recoverable

# --- T35: v39 restart failure -> rollback to preserved v37 ------------------
expect "T35 v39 restart failure rolls back to the preserved v37"
SBID="v39-restart-fail"; sandbox
make_flat_install v37
A39="$(make_archive v39)"
patched_updater
export FAKE_SVC_MODE="restart-fail-v39"
run_sb_updater update "${A39}"
unset FAKE_SVC_MODE
check_log "restart failure hit" "RESTART FAIL"
check_log "UPDATE FAILED reported" "UPDATE FAILED"
check_no_log "no false success" "UPDATE SUCCESS"
check "rolled back to v37" current_points_at v37
check "installed unit is the current-based unit" unit_is_current_based
check "v37 bootable through current" current_is_bootable v37
check "env points at current/bin" grep -qx "XUI_BIN_FOLDER=${FAKE_ROOT}/current/bin" "${ENV_FILE}"
check "v39 retained for inspection" test -f "${FAKE_ROOT}/releases/v39/rafael-panel"
check "DSN preserved" grep -q "dbname=rafael_panel" "${ENV_FILE}"
check "rollback to v37 still works" rollback_to_v37_works

# --- T36: stale Xray core (process still on the OLD path) --------------------
expect "T36 stale Xray process (old path) fails health and rolls back"
SBID="stale-xray"; sandbox
make_flat_install v37
A39="$(make_archive v39)"
patched_updater
export FAKE_SVC_MODE="stale-xray-v39"
run_sb_updater update "${A39}"
unset FAKE_SVC_MODE
check_log "stale core detected (current bin path not running)" "Xray core process is not running"
check_log "health check failed" "HEALTH CHECK FAIL"
check_log "UPDATE FAILED reported" "UPDATE FAILED"
check_no_log "no false success" "UPDATE SUCCESS"
check "rolled back to v37" current_points_at v37
check "v37 bootable through current" current_is_bootable v37
check "installed unit is the current-based unit" unit_is_current_based
check "DSN preserved" grep -q "dbname=rafael_panel" "${ENV_FILE}"

# --- T37: interrupted v37 copy -> re-copied on retry -------------------------
expect "T37 interrupted v37 copy is re-copied (never trusted half-built)"
SBID="midcopy"; sandbox
make_flat_install v37
A39="$(make_archive v39)"
INJECT_MODE="after"
INJECT_ANCHOR='
            cp -a "${ROOT_DIR}/${item}" "${release_dir}/"'
INJECT_TEXT='
            die "INJECTED: mid-copy"'
patched_updater
run_sb_updater update "${A39}"
check_log "injected failure hit" "INJECTED: mid-copy"
check "flat install untouched" test -f "${FAKE_ROOT}/rafael-panel" -a -f "${FAKE_ROOT}/bin/xray-linux-amd64"
check "no current link" test ! -e "${FAKE_ROOT}/current"
check "installed unit still flat" unit_is_flat
check "partial release dir left behind" test -d "${FAKE_ROOT}/releases/v37"
check "partial copy incomplete (xray missing)" test ! -f "${FAKE_ROOT}/releases/v37/bin/xray-linux-amd64"
# retry WITHOUT the injection: the incomplete dir must be detected + re-copied
patched_updater
run_sb_updater update "${A39}"
check_log "incomplete copy detected on retry" "incomplete (interrupted copy?)"
check_log "retry succeeds" "UPDATE SUCCESS"
check "current -> v39 after retry" current_points_at v39
check "v37 release complete after retry" v37_recoverable
check "installed unit is the current-based unit" unit_is_current_based

# --- T38: REAL smoke_run (unstubbed) — regression for the staging 0700 bug --
# The suite stubs smoke_run everywhere (a fake ELF cannot execute), which is
# exactly how a 2026-09-09 production failure hid: the mktemp staging dir is
# 0700, so `su nobody` could never traverse into it and EVERY real-host
# smoke check failed. These tests run the REAL smoke_run against an
# executable script standing in for the panel, in the same 0700 stage layout.
expect "T38 real smoke_run opens the staging path and validates execution"
SBID="smoke-real"; sandbox
# sandboxed updater with the REAL smoke_run (strip ONLY the smoke stub from
# the overrides; the is_executable_file / text-link shims stay)
sed -e "s|^ROOT_DIR=.*|ROOT_DIR=\"${FAKE_ROOT}\"|" \
    -e "s|^ENV_FILE=.*|ENV_FILE=\"${ENV_FILE}\"|" \
    -e "s|^BACKUP_ROOT=.*|BACKUP_ROOT=\"${SB}/backups\"|" \
    -e "s|^CLI_ALIAS=.*|CLI_ALIAS=\"${SB}/usr-bin-rafael\"|" \
    -e "s|^UNIT_DST=.*|UNIT_DST=\"${SB}/systemd-rafael.service\"|" \
    -e "s|^LOCK_FILE=.*|LOCK_FILE=\"${FAKE_ROOT}/.rafael-updater.lock\"|" \
    -e "s|^    if \[\[ \"\${EUID}\" -ne 0 \]\]; then|    if false; then|" \
    "${UPDATER}" >"${SB}/real-smoke.raw.sh"
smoke_guard_line="$(grep -n 'BASH_SOURCE\[0\]' "${SB}/real-smoke.raw.sh" | tail -n1 | cut -d: -f1)"
[[ -n "${smoke_guard_line}" ]] || { echo "T38: source guard not found" >&2; exit 2; }
{
    head -n $(( smoke_guard_line - 1 )) "${SB}/real-smoke.raw.sh"
    printf '%s\n' "${SANDBOX_OVERRIDES}" | grep -v '^smoke_run()'
    tail -n +"${smoke_guard_line}" "${SB}/real-smoke.raw.sh"
} >"${SB}/real-smoke.sh"
rm -f "${SB}/real-smoke.raw.sh"
grep -q 'falling back to a root run' "${SB}/real-smoke.sh"
check "real-smoke updater carries the REAL smoke_run (with root fallback)" test $? -eq 0
check "updater opens the staging dir by pattern (mktemp 0700 regression)" grep -q '\*/\.rafael-stage-\*' "${UPDATER}"
# realistic layout: 0700 stage dir (as mktemp + explicit chmod leave it)
SMOKE_STAGE="${FAKE_ROOT}/.rafael-stage-T38TEST"
mkdir -p "${SMOKE_STAGE}/rafael"
printf '#!/bin/sh\ncase "$1" in -v) echo v39; exit 0;; esac\nexit 0\n' >"${SMOKE_STAGE}/rafael/rafael-panel"
chmod 700 "${SMOKE_STAGE}"
chmod 755 "${SMOKE_STAGE}/rafael/rafael-panel"
smoke_rc=0
( source "${SB}/real-smoke.sh"; smoke_run "${SMOKE_STAGE}/rafael/rafael-panel" ) || smoke_rc=$?
check "real smoke_run passes for an executable staged panel (0700 stage dir)" test "${smoke_rc}" -eq 0
printf '#!/bin/sh\nexit 3\n' >"${SMOKE_STAGE}/rafael/rafael-panel"
chmod 755 "${SMOKE_STAGE}/rafael/rafael-panel"
smoke_rc=0
( source "${SB}/real-smoke.sh"; smoke_run "${SMOKE_STAGE}/rafael/rafael-panel" ) || smoke_rc=$?
check "real smoke_run FAILS for a broken staged panel" test "${smoke_rc}" -ne 0

# --- T39: flat v39 -> same-marker v39 archive needs NO FORCE -----------------
# 2026-09-09 production failure (incredible-beige): a flat Build 39 install
# updated by a FIXED Build 39 archive. The flat migration creates
# releases/v39 from the old flat files, then the install step collides with
# the very dir it just created ("release v39 already exists") — the update
# dies after a healthy migration and the new binary is never installed.
# The fix: a release dir created by THIS run's migration is moved aside
# automatically; FORCE is only required for pre-existing releases.
expect "T39 flat v39 updated by a same-marker v39 archive succeeds without FORCE"
SBID="flat-same-marker"; sandbox
make_flat_install v39
A39FIX="$(make_archive v39)"
patched_updater
run_sb_updater update "${A39FIX}"
check_log "migrated old build auto-moved aside" "migrated v39 (old flat build) moved aside"
check_no_log "no collision failure" "already exists"
check_log "update completes" "UPDATE SUCCESS"
check "current -> v39" current_points_at v39
check "old flat build preserved as .replaced-" test -d "${FAKE_ROOT}"/releases/v39.replaced-*
check "installed unit is the current-based unit" unit_is_current_based
check "env points at current/bin" grep -qx "XUI_BIN_FOLDER=${FAKE_ROOT}/current/bin" "${ENV_FILE}"

echo "==================================================================="
echo "RESULTS: ${PASS} passed, ${FAIL} failed (symlinks native: $(( SYMLINKS_OK )))"
echo "==================================================================="
exit $(( FAIL > 0 ? 1 : 0 ))
exit $(( FAIL > 0 ? 1 : 0 ))

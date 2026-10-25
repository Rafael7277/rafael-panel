#!/bin/bash

# Stable Rafael launcher, installed at /usr/bin/rafael by the release
# manager. Lives OUTSIDE the release tree, so a broken current release can
# never block an update or rollback.
#
# - Release-control commands (update / rollback / releases) always route to
#   the stable release manager at /usr/local/rafael/rafael-updater.sh.
# - Everything else runs through the current release's rafael.sh.
set -e

ROOT_DIR="/usr/local/rafael"
RELEASE_MANAGER="${ROOT_DIR}/rafael-updater.sh"

cmd="${1:-menu}"
case "${cmd}" in
    update|update-local)
        # update-local (legacy flat updater) must run from a real release.
        if [[ "${cmd}" == "update-local" ]]; then
            if [[ -x "${ROOT_DIR}/current/rafael.sh" ]]; then
                exec bash "${ROOT_DIR}/current/rafael.sh" "$@"
            fi
            echo "update-local requires a runnable current release; use 'rafael update' instead." >&2
            exit 1
        fi
        if [[ ! -x "${RELEASE_MANAGER}" ]]; then
            echo "Release manager not found: ${RELEASE_MANAGER}" >&2
            exit 1
        fi
        exec bash "${RELEASE_MANAGER}" update "${2:-}"
        ;;
    rollback)
        if [[ ! -x "${RELEASE_MANAGER}" ]]; then
            echo "Release manager not found: ${RELEASE_MANAGER}" >&2
            exit 1
        fi
        exec bash "${RELEASE_MANAGER}" rollback "${2:-}"
        ;;
    releases)
        if [[ ! -x "${RELEASE_MANAGER}" ]]; then
            echo "Release manager not found: ${RELEASE_MANAGER}" >&2
            exit 1
        fi
        exec bash "${RELEASE_MANAGER}" status
        ;;
    *)
        if [[ -x "${ROOT_DIR}/current/rafael.sh" ]]; then
            exec bash "${ROOT_DIR}/current/rafael.sh" "$@"
        elif [[ -x "${ROOT_DIR}/rafael.sh" ]]; then
            # Flat install (pre-migration): fall back to the flat CLI.
            exec bash "${ROOT_DIR}/rafael.sh" "$@"
        fi
        echo "Rafael: no runnable release under ${ROOT_DIR}." >&2
        echo "Control commands remain available:" >&2
        echo "  rafael update <archive>   rafael rollback [version]   rafael releases" >&2
        exit 1
        ;;
esac

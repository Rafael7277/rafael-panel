# Rafael Panel v63 Checklist

Carry forward unresolved v62 issues and add every new v63 issue here before changing code. Check a source item only after its implementation is in this tree; keep browser, device, SSL, and clean-server items open until they are actually verified. Carry this file into the release package and update it whenever a new issue is reported.

## Release and GitHub preparation

- [x] Replace the inherited Sanaei `install.sh` with a Rafael release installer.
- [x] Keep the same one-command GitHub installer entry point and preserve a flat `/usr/local/rafael` layout for clean installs.
- [x] Confirm the updater parser accepts generic numeric `vNN` markers without pinning an exact panel version; the v62-to-v63 server upgrade remains open.
- [x] User confirmed a v58 installation updated to v59 through the Rafael CLI updater. Isolated-server migration and service-health checks remain separate acceptance items.
- [x] Download the Rafael Linux amd64 archive and require a matching SHA-256 sidecar before extraction or upgrade.
- [x] Route existing managed installations through the Rafael updater; refuse unsafe overwrite when install files exist without the managed service.
- [x] Add a guided clean-install setup for a selected/random port, generated web path, administrator credentials, optional domain/IP, and optional SSL.
- [x] Support Let's Encrypt, existing certificate/key files, and explicit HTTP-only setup; validate certificate/key match before enabling TLS.
- [x] Verify stored port, path, domain, and TLS settings before starting the service; keep database password out of installer output.
- [x] Build the matching Rafael Xray binary directly from `third_party/xray-core`; include the source archive and checksums so the package does not depend on a separate Xray download.
- [ ] Build and upload the Linux amd64 panel package on pushes to main; publish a versioned GitHub Release for tag v63.
- [x] Prepare package metadata, notices, install docs, workflow, and a v63 release builder.
- [x] Include the inherited GPL-3.0 license and third-party NOTICE in the repository, install archive, and direct GitHub Release assets.
- [x] Rebuild the local Linux amd64 v59 archive with the Rafael fork binary and verify both archive contents and checksums (the earlier artifact used stock Xray and is superseded).
- [x] Vendor the Rafael IP-limit Xray source snapshot based on fork commit `780167948dbbac8cd9fbeacf0b5c1a0c4243c525`, including the three recorded working-tree changes, and pin its full source-tree fingerprint.
- [x] Publish the forked Xray source and matching Linux amd64 binary from the separate Rafael Xray Core repository with SHA-256 sidecars.
- [x] Make fresh install and bootstrap fail closed instead of substituting official Xray when the Rafael fork binary is missing.
- [x] Stop the in-panel Xray version switch from downloading upstream binaries that do not include Rafael's IP-limit hook.
- [x] Build the v63 panel and forked Xray from the pinned vendored source; verify archive contents and SHA-256 sidecars.
- [ ] Confirm that the installed fork's version is shown in the modal, upstream versions are not offered, and the API rejects attempts to replace it.
- [ ] On an isolated server, confirm the running Xray child receives `XRAY_IPLIMIT_FILE` and enforces a configured IP limit; source inspection alone does not prove runtime enforcement.
- [x] Configure the public GitHub repository path as Rafael7277/rafael-panel in the installer and install documentation.
- [ ] Complete a legal review of GPL obligations and verify every bundled dependency's redistribution terms.
- [ ] Review the final source changes against v57 and confirm no prior fixes were lost.
- [x] User authorized public source publication and v59 release to Rafael7277/rafael-panel.

## v63 traffic coefficient and multi-link IP limit

- [x] Keep nominal client quota in inbound settings and persist the current effective byte allowance in the existing client traffic record; no schema migration is required.
- [x] Apply the coefficient only to newly added quota and resets; coefficient-only edits preserve the already granted effective allowance.
- [x] Apply effective quota semantics to client creation, bulk creation, edits, reset actions, periodic renewals, group resets, and multi-link edits.
- [x] Use effective quota values for reseller assignment limits and multi-link quota displays/guards.
- [x] Group aliases by multi-link SubID in the Xray policy and share their active IP counter inside one Xray process.
- [x] Bind the client coefficient through modal-owned reactive state and explicitly copy it to the saved client before serialization.
- [x] Reopen client edit forms with the coefficient read from the exact persisted client entry when the reconstructed model omits it.
- [x] Show a per-client coefficient tag in desktop and mobile inbound views; missing/zero coefficient displays the normal 1:1 factor.
- [ ] Verify coefficient persistence and display after reload for admin, reseller-owned, and multi-link clients.
- [x] Show IP-limit enable and value controls while creating a multi-link client, even when the general form setting hides them.
- [x] Add an enable switch and conditional maximum-IP input to the multi-link edit form; preserve the entered maximum while toggling the switch off and on.
- [x] Build and inspect the v63 Linux amd64 archive and matching Xray source archive; verify version marker, package contents, and SHA-256 sidecars.
- [ ] Verify quota increases, coefficient-only edits, resets, expiry renewals, and reseller limits with representative existing and new clients before calling the behavior production-accepted.
- [ ] Cross-node/global IP-limit sharing is not implemented: distinct Xray processes maintain separate live IP counters. A central coordinator is needed before promising one cap across separate nodes.

## Install, SSL, and web-path security

- [x] Stop redirecting `/` to the configured admin web path; an unknown root request must not reveal the private path.
- [x] Keep the admin panel at its configured web path, or at `/` when the path is unset.
- [x] Support the default reseller route `/reseller/{username}/` and the custom route `/reseller/{username}/{webPath}/`.
- [x] Apply reseller routes consistently to login, pages, APIs, logout, assets, and WebSocket connections.
- [x] Preserve prior reseller paths as aliases that route to the current reseller address.
- [x] Reject reserved/colliding admin and reseller paths.
- [ ] On a clean Debian/Ubuntu server, install v63 with SSL skipped and confirm generated URL, credentials, service, and firewall guidance.
- [ ] On a clean server, issue a domain certificate with public port 80 and confirm HTTPS, path routing, and renewal setup.
- [ ] Confirm a user-selected panel port is reflected in the URL and in the service listener.
- [ ] Confirm a custom certificate with matching key is accepted, mismatched key is rejected, and certificate host validation works.
- [ ] Confirm failure to obtain Let's Encrypt does not silently enable HTTPS; verify the explicit HTTP fallback and abort choices.
- [ ] Confirm the main and reseller paths work on the primary domain with and without a custom web path.
- [ ] Confirm `/` does not reveal the admin web path and a configured `webDomain` rejects an unrelated Host header.
- [ ] Upgrade a v62 installation using the v63 release archive and confirm users, database, settings, certificates, and reseller paths remain intact.

## v63 quota consistency, enforcement, and performance visibility

- [x] Use the persisted effective client allowance consistently for desktop/mobile quota labels, remaining traffic, progress, warning colors, and depleted state; derive the initial effective quota only when the traffic record is unavailable.
- [x] Sample Xray traffic every 2 seconds and check requested restarts every second so quota enforcement does not wait for the previous 30-second restart interval.
- [x] Add privacy-safe slow HTTP route and traffic-stage diagnostics, configurable with `RAFAEL_PERF_SLOW_MS` (default 500 ms; `0` disables logging).
- [x] Replace nested client-to-traffic scans in reconciliation with indexed lookup maps.
- [ ] Observe representative slow-operation logs and server load on a production-like server; tune `RAFAEL_PERF_SLOW_MS` and sampling cadence from measurements.
- [ ] Verify a new 0.2 GB allocation at coefficient 2 displays and stops at the effective allowance; verify increase, coefficient-only edit, reset, expiry renewal, and reseller-owned clients.
- [ ] Confirm the remaining cutoff overrun is within an acceptable measured bound with existing connections and real traffic; periodic accounting cannot promise byte-perfect cutoff.
- [x] Build and inspect the v63 Linux amd64 archive and matching Xray source archive; verify marker, package contents, and SHA-256 sidecars.
- [ ] Confirm the installer stops safely when a checksum is missing, malformed, or incorrect.

## Critical Inbounds issues

- [ ] Confirm Add Inbound opens the Rafael-themed form every time, keeps reactive controls working, saves, and shows the new inbound.
- [ ] Show an actionable error if the Add Inbound modal cannot mount; no silent click failure.
- [ ] Match the Add Inbound form to Rafael dark and warm-light themes.
- [ ] Render readable inbound cards on phones instead of squeezing the desktop table.
- [ ] Expand/collapse each mobile inbound to show its clients.
- [ ] Show and persist the client enabled/disabled switch on mobile.
- [ ] Provide mobile client QR, normal connection link, and available subscription links, including display and copy actions.
- [ ] Keep mobile client edit, info, and delete actions available and usable.
- [ ] Give total, off, online, and ended client counts distinct readable colors in both themes.
- [ ] Keep the Inbounds page and forms within phone width without page-level horizontal scrolling.
- [ ] Confirm the mobile sidebar uses Rafael styling and is not the legacy v48 sidebar.

## Xray Configs and responsive layout

- [ ] Fit every Xray Configs form/dialog to phone screens with readable controls and internal vertical scrolling.
- [ ] Show all Xray sections/tabs in a wrapping or collapsible navigation that does not require sideways scrolling.
- [ ] Fit Xray content to the remaining width after both app and section sidebars at phone, tablet, and desktop sizes.
- [ ] Remove page-level and table horizontal scrolling from Xray forms/tables; keep every field reachable in compact views.
- [ ] Confirm Routing Rules and Outbounds at 320 px, 390 px, tablet, and desktop widths, including all fields in expanded details.
- [ ] Keep section navigation collapsed by default and allow opening all sections.
- [ ] Remove the unnecessary Validation Results, Xray Service Status, and Quick Actions right-side panels.

## Other reported visual issues

- [ ] Style the login submit button to match Rafael violet theme and remove legacy teal styling.
- [ ] Warm and repair the light theme across pages; remove dark cards and improve text, border, and control contrast.
- [ ] Correct reseller summary-card sizes/alignment; prevent collisions with Reseller Management and keep table actions readable.
- [ ] Align Overview CPU and Usage cards with adjacent cards and keep their row heights consistent.
- [ ] Restore the Xray Service icon on Overview.
- [ ] Align Live system signals with Connection Stats and match card heights.
- [ ] Keep live upload/download values together by the Network Traffic graph and make grid lines finer and lighter.
- [ ] Show Overview Usage values and align them within the card.
- [ ] Review all pages, tabs, dialogs, and mobile navigation in both dark and warm light themes.

## Required verification before calling v63 ready to install

- [ ] Run the installer on a clean, isolated Debian/Ubuntu server with HTTP only.
- [ ] Run the installer on a clean, isolated domain with Let's Encrypt and verify automatic renewal configuration.
- [ ] Verify a custom certificate install and a failed-certificate path.
- [ ] Verify a v62-to-v63 upgrade with a database backup and confirm existing state is preserved.
- [ ] In a browser, create an inbound and confirm no Vue/runtime errors; use only an isolated test database.
- [ ] At phone widths, verify client expand/collapse, QR, normal/subscription links, copy, enable/disable, edit/info, and delete.
- [ ] At phone and desktop widths, verify Xray sections/forms, both sidebars, no page-level horizontal scrolling, and dark/light themes.
- [ ] Confirm generated main/reseller URLs, login, page/API requests, logout, and WebSockets on the primary domain.
- [x] Build the v63 Linux amd64 panel and Xray source archives; verify package files, release marker, updater pin, fork source fingerprint, and SHA-256 sidecars.
- [x] Do not launch the local panel or run local browser/runtime checks, as requested.

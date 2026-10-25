# Rafael Panel v59 — release notes

- Added a Rafael installer that downloads public Linux amd64 releases and verifies their SHA-256 sidecar.
- Added a guided first-install flow for random/chosen port, generated private web path, admin credentials, optional domain/IP, Let's Encrypt, existing certificate/key, or HTTP-only mode.
- Routed managed upgrades through the Rafael updater to preserve the existing database and panel configuration. The user confirmed successful v58-to-v59 CLI upgrade.
- Added GitHub Actions packaging on every push to main and versioned release publishing for tag v59.
- Bundled GeoIP/GeoSite data and Xray built from the pinned Rafael IP-limit fork; included the standalone binary, matching source archive, and checksums.
- Made installation fail closed when the Rafael fork binary is missing. Upstream Xray replacement is disabled because it does not contain the Rafael IP-limit hook.
- Documented Rafael's extension of the 3x-ui v2.9.2 database and feature set, including client IP limits, reseller access and quotas, and Adaptive Smart outbound health management.
- Adaptive Smart temporarily quarantines unhealthy outbounds and restores recovered members to reduce routing failures; no zero packet-loss guarantee is made.
- Published latest and pinned v59 install commands for Rafael7277/rafael-panel.

## Remaining acceptance

Clean-server install, Let's Encrypt issuance/renewal, custom certificate, browser/mobile, and additional IP-limit runtime checks remain open in BUILD_V59_CHECKLIST.md. The local panel was not launched for this release work.
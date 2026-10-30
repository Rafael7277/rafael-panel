# Rafael Panel v63 Linux amd64 package

Rafael Panel is an enhanced fork based on the MHSanaei/3x-ui v2.9.2 codebase. It extends the panel's database and management features while keeping the upstream GPL-3.0 license. The matching Rafael Xray fork is compiled from the source included in this repository and bundled in the Linux package; users do not need to fetch a separate Xray binary.

## v63 changes

- Inbound client quota labels, remaining traffic, progress bars, warning colors, and depleted badges use the same effective allowance that the traffic record and cutoff logic use. For a fresh 0.2 GB allocation with coefficient 2, the effective display is about 0.1 GB; older allocations remain as granted until their quota is increased or reset, matching the documented coefficient policy.
- Quota traffic is sampled every 2 seconds, and requested Xray restarts are checked every second. This shortens the old 30-second wait to remove existing sessions after quota exhaustion; already in-flight traffic and the polling window can still cause a small overrun.
- Slow HTTP routes and traffic-processing stages emit privacy-safe `PERF` warning records when they exceed 500 ms. Set `RAFAEL_PERF_SLOW_MS` to another non-negative millisecond threshold, or `0` to disable these logs. Recorded HTTP routes use route templates and omit query values and raw URLs.
- Replace nested client/traffic matching in the traffic reconciliation path with indexed lookups to reduce work as client counts grow.
- Client traffic coefficients scale new quota allocations and traffic resets. The configured traffic amount remains the nominal amount. Changing only the coefficient preserves the already granted effective allowance. For example, a new 1 GB allocation with coefficient 2 starts with 500 MB; raising the nominal quota from 1 GB to 2 GB with coefficient 2 adds 500 MB to the existing allowance. Resetting starts a fresh 1 GB allowance at coefficient 2.
- Reseller quota accounting uses the effective client allowance, including clients in a multi-link group.
- IP limits are shared by the aliases of a multi-link group when their inbounds use the same Xray process. Independent Xray processes on separate servers keep independent live-connection counters; v63 does not provide a global cross-node IP counter.
- Client edit forms restore the traffic coefficient from the stored client settings even if the reconstructed UI model has a default value. Desktop and mobile client lists show `Factor ×N`; a missing or zero coefficient is displayed as `Factor ×1`.
- Multi-link create/edit forms expose an IP-limit switch with a maximum-IP value.
- The package contains the matching Xray binary, source archive, install/update scripts, GeoIP/GeoSite data, license texts, and checksums.

The existing per-client runtime IP limit, reseller access scopes, portal paths, lifecycle controls, and Adaptive Smart outbound health management remain included. Adaptive Smart can remove unhealthy outbounds from selection and restore recovered ones; it cannot guarantee zero packet loss.

## Install from GitHub

Run as `root` (or enter a root shell with `sudo -i`). The installer verifies the archive SHA-256 and guides clean setup through port, web path, domain/IP, and optional TLS.

Install the latest public release:

```bash
bash <(curl -Ls https://raw.githubusercontent.com/Rafael7277/rafael-panel/main/install.sh)
```

Install or upgrade to v63:

```bash
bash <(curl -Ls https://raw.githubusercontent.com/Rafael7277/rafael-panel/main/install.sh) v63
```

## Flat install from the release archive

As `root`, download the self-contained Linux amd64 package and checksum, verify it, then run the guided setup:

```bash
curl -fL -o rafael-linux-amd64.tar.gz https://github.com/Rafael7277/rafael-panel/releases/download/v63/rafael-linux-amd64.tar.gz
curl -fL -o rafael-linux-amd64.tar.gz.sha256 https://github.com/Rafael7277/rafael-panel/releases/download/v63/rafael-linux-amd64.tar.gz.sha256
sha256sum --check rafael-linux-amd64.tar.gz.sha256
tar -xzf rafael-linux-amd64.tar.gz
cd rafael
bash ./install-local.sh
```

Clean installs use `/usr/local/rafael`. Existing Rafael installations should use the updater to preserve the current database and configuration.

## Source and legal notices

- Rafael Panel: [GNU GPL-3.0](https://github.com/Rafael7277/rafael-panel/blob/v63/LICENSE)
- Bundled Rafael Xray fork: [MPL-2.0](https://github.com/Rafael7277/rafael-panel/blob/v63/third_party/xray-core/LICENSE)
- Third-party notices: [NOTICE](https://github.com/Rafael7277/rafael-panel/blob/v63/NOTICE)
- The panel release includes the matching Xray source archive and SHA-256 sidecars.

See `RELEASE_NOTES_v63.md` for implementation details and `BUILD_V63_CHECKLIST.md` for source, packaging, and operational acceptance status.

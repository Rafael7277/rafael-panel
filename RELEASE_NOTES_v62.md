# Rafael Panel v62

Rafael Panel v62 is based on the MHSanaei/3x-ui v2.9.2 codebase with expanded database and management features. It bundles the matching Rafael Xray fork compiled from the source in this repository.

## Client traffic coefficients

Each client stores its nominal traffic amount in its inbound settings and its current effective allowance in the existing client traffic record. The coefficient applies to newly added quota and to a reset. Editing the coefficient alone leaves the current effective allowance unchanged, so previously consumed traffic is never retroactively recalculated. A coefficient of `0` or `1` means normal 1:1 allocation.

The client edit modal binds the coefficient through explicit modal state and includes it in the saved client payload. On reopen, it now prefers the coefficient from the exact stored client entry, avoiding a default value on the reconstructed UI model. Desktop and mobile inbound client lists show a `Factor ×N` tag; a missing or zero coefficient is shown as `Factor ×1`. The server database was inspected read-only for the reported client and contained coefficient `2`; browser/server runtime verification of this UI fix remains open in the checklist.

Example: a new 1 GB client with coefficient `2` receives 500 MB. If the client has used that 500 MB and the nominal quota is raised from 1 GB to 2 GB with coefficient `2`, the effective cap increases by 500 MB, making another 500 MB available. Resetting starts a new cycle from 2 GB / 2, which is 1 GB. Reseller assignment checks and multi-link quota displays use the effective allowance.

## Multi-link IP limit

The forked Xray process tracks active IPs by logical client identity. Client aliases with the same multi-link `SubID` share one counter and the strictest positive IP cap across the aliases. This works when the related inbounds run in the same Xray process. Separate Xray processes, including inbounds hosted on different nodes, do not share live session state; the limit is therefore per process/node and is not a global cross-node device cap.

Multi-link creation and editing now expose an IP-limit enable switch and maximum-IP value. Turning the switch off sends an unlimited value while retaining the entered maximum for when it is turned back on.

## Linux amd64 install

Install/upgrade from the GitHub installer:

```bash
bash <(curl -Ls https://raw.githubusercontent.com/Rafael7277/rafael-panel/main/install.sh) v62
```

Flat install from the self-contained archive:

```bash
curl -fL -o rafael-linux-amd64.tar.gz https://github.com/Rafael7277/rafael-panel/releases/download/v62/rafael-linux-amd64.tar.gz
curl -fL -o rafael-linux-amd64.tar.gz.sha256 https://github.com/Rafael7277/rafael-panel/releases/download/v62/rafael-linux-amd64.tar.gz.sha256
sha256sum --check rafael-linux-amd64.tar.gz.sha256
tar -xzf rafael-linux-amd64.tar.gz
cd rafael
bash ./install-local.sh
```

The package includes the panel binary, the matching forked Xray binary, the matching Xray source archive, GeoIP/GeoSite data, installer/updater scripts, notices, licenses, and SHA-256 sidecars. No separate Xray download is required.

## Licenses

- Rafael Panel: GNU GPL-3.0 (`LICENSE`)
- Bundled Rafael Xray fork: MPL-2.0 (`LICENSE-XRAY-MPL-2.0`)
- Third-party component notices: `NOTICE`

Read `BUILD_V62_CHECKLIST.md` for remaining visual, device, installer, and multi-node acceptance work. The package build does not claim browser or server-runtime verification.

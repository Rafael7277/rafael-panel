# Rafael Panel

An enhanced Xray management panel derived from [MHSanaei/3x-ui](https://github.com/MHSanaei/3x-ui). The upstream base version in `config/version` is **2.9.2**. Rafael extends its database model and panel features while retaining GPL-3.0; see [NOTICE](./NOTICE) and [LICENSE](./LICENSE).

## Key features

- **Real client IP limits:** Enforced at runtime by Rafael's bundled, pinned Xray fork, with active-IP grace handling and history. Rafael's package includes this matching Xray binary.
- **Reseller panels:** Reseller accounts, assigned-inbound scope, configurable section access, user/traffic quotas, private portal paths, and integrated lifecycle controls.
- **Adaptive Smart balancer:** Monitors outbound health and success, temporarily removes unhealthy members from runtime selection, and returns recovered members. It is designed to reduce routing failures and packet loss; network conditions prevent a zero-loss guarantee.
- **Expanded data model:** Adds reseller, client IP-limit, lifecycle, quota, and panel-management data and controls beyond the 2.9.2 base.
- **Guided setup:** Choose a panel port, generate a private web path, configure a domain/IP, and select Let's Encrypt, an existing certificate/key, or HTTP without SSL.

## Install

Run as root on supported Debian/Ubuntu Linux amd64. The panel archive already bundles Xray; no separate Xray download is needed.

Latest public release:

```bash
bash <(curl -Ls https://raw.githubusercontent.com/Rafael7277/rafael-panel/main/install.sh)
```

Pin to v59:

```bash
bash <(curl -Ls https://raw.githubusercontent.com/Rafael7277/rafael-panel/main/install.sh) v59
```

The installer verifies the release SHA-256. A clean install uses the flat /usr/local/rafael layout. Existing Rafael installs use the Rafael CLI updater and preserve their database and configuration; the user confirmed a successful v58-to-v59 CLI upgrade.

The guided setup offers a random or chosen port, optional domain/IP, private web path, and optional SSL. Let's Encrypt HTTP-01 requires public TCP 80 to reach its challenge listener.

## Releases

Every push to main builds the Linux amd64 panel bundle with the pinned forked Xray and uploads it as a GitHub Actions artifact. Pushing tag v59 also publishes the versioned GitHub Release. The release includes the panel package, standalone Xray, matching Xray source archive, and SHA-256 sidecars.

Build locally on Windows with Go and tar installed: `./build-release-v59.ps1`

See [install and release guide](./docs/GITHUB_INSTALL.md), [v59 notes](./RELEASE_NOTES_v59.md), [checklist](./BUILD_V59_CHECKLIST.md), and [notices](./NOTICE).
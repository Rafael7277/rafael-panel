# Rafael Panel v59

Rafael is an enhanced Xray/VPN management panel based on the MHSanaei/3x-ui v2.9.2 codebase. This fork expands the panel's database model and management features while keeping its upstream GPL-3.0 license. The bundled Rafael Xray fork is separately licensed under MPL-2.0; both license texts and third-party notices are included below and inside the installation archive.

## What is included

- Runtime client IP limits enforced by Rafael's own Xray fork, with active-IP tracking. The matching Xray binary is bundled in the panel archive, so installation does not require a separate Xray download.
- Reseller accounts with scoped inbound access, configurable section permissions, client/traffic quotas, private portal paths, and lifecycle controls.
- Adaptive Smart outbound health management that can temporarily remove unhealthy outbounds from selection and restore recovered ones. It is designed to reduce routing failures; it cannot guarantee zero packet loss.
- A Rafael installer and updater, expanded database/panel features, and bundled GeoIP/GeoSite data. The v58-to-v59 CLI upgrade was confirmed by the project owner.

## Install from GitHub

Run the following commands as `root` (or enter a root shell with `sudo -i` first). The installer verifies the release archive's SHA-256 checksum and guides a clean installation through port, web path, domain/IP, and optional TLS setup.

Install the latest release:

```bash
bash <(curl -Ls https://raw.githubusercontent.com/Rafael7277/rafael-panel/main/install.sh)
```

Install or upgrade to a pinned release:

```bash
bash <(curl -Ls https://raw.githubusercontent.com/Rafael7277/rafael-panel/main/install.sh) v59
```

## Flat install from the release archive

As `root`, download the self-contained Linux amd64 package and checksum, verify it, then run its guided first-install script:

```bash
curl -fL -o rafael-linux-amd64.tar.gz https://github.com/Rafael7277/rafael-panel/releases/download/v59/rafael-linux-amd64.tar.gz
curl -fL -o rafael-linux-amd64.tar.gz.sha256 https://github.com/Rafael7277/rafael-panel/releases/download/v59/rafael-linux-amd64.tar.gz.sha256
sha256sum --check rafael-linux-amd64.tar.gz.sha256
tar -xzf rafael-linux-amd64.tar.gz
cd rafael
bash ./install-local.sh
```

The archive includes the panel executable, Xray fork binary, checksums, service and install/update scripts, license texts, notices, and GeoIP/GeoSite data. Existing Rafael installs should use the updater; the clean-install script refuses to overwrite an existing managed installation or database.

## Source and legal notices

- Rafael Panel source and distribution: [GNU GPL-3.0](https://github.com/Rafael7277/rafael-panel/blob/v59/LICENSE)
- Bundled Rafael Xray fork: [MPL-2.0](https://github.com/Rafael7277/rafael-panel/blob/v59/third_party/xray-core/LICENSE)
- Xray fork source, binary, provenance, and checksums: [Rafael Xray Core v1.260327.0-rafael.1](https://github.com/Rafael7277/rafael-xray-core/releases/tag/v1.260327.0-rafael.1)
- Additional component notices: [NOTICE](https://github.com/Rafael7277/rafael-panel/blob/v59/NOTICE)

Review each license and notice before redistributing modified or combined copies. Remaining operational acceptance checks are tracked in `BUILD_V59_CHECKLIST.md`.

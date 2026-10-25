# Rafael Panel v59 Linux amd64 package

The archive contains the panel, first-install and update scripts, systemd service, GeoIP/GeoSite data, notices, and `bin/xray-linux-amd64` built from Rafael's pinned IP-limit Xray fork. Xray is bundled in the panel archive; no separate Xray download is required. Matching standalone Xray and source archives are provided with SHA-256 sidecars.

Rafael extends the MHSanaei/3x-ui v2.9.2 base with expanded database and panel features: runtime client IP limits, reseller access and quotas, portal paths, lifecycle controls, and Adaptive Smart outbound health management. Adaptive Smart can quarantine unhealthy members and restore recovered ones to reduce routing failures; it cannot guarantee zero packet loss.

## Install from GitHub

Latest public release:

```bash
bash <(curl -Ls https://raw.githubusercontent.com/Rafael7277/rafael-panel/main/install.sh)
```

Pinned v59:

```bash
bash <(curl -Ls https://raw.githubusercontent.com/Rafael7277/rafael-panel/main/install.sh) v59
```

The installer verifies the archive checksum. On a clean server, extract the archive and run `bash ./install-local.sh` as root. Guided setup configures port, web path, domain/IP, and optional Let's Encrypt, existing certificate/key, or HTTP. Clean installs use /usr/local/rafael. Existing Rafael installs go through the updater and keep their database/configuration. The user confirmed v58-to-v59 CLI update success.

Further clean-server, SSL, and runtime acceptance items are listed in `BUILD_V59_CHECKLIST.md`.
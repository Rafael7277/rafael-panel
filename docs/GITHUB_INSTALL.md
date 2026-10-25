# GitHub install and release guide

The public repository is `Rafael7277/rafael-panel`. Rafael is based on MHSanaei/3x-ui v2.9.2 (`config/version`) and keeps the inherited GPL-3.0 license. See `NOTICE` and `LICENSE`.

## Install

Latest public release:

```bash
bash <(curl -Ls https://raw.githubusercontent.com/Rafael7277/rafael-panel/main/install.sh)
```

Pinned v59:

```bash
bash <(curl -Ls https://raw.githubusercontent.com/Rafael7277/rafael-panel/main/install.sh) v59
```

Run as root on supported Debian/Ubuntu Linux amd64. The installer downloads the release and SHA-256 sidecar over HTTPS and verifies the package. A clean install uses the flat /usr/local/rafael layout. Existing managed installs use the Rafael updater and preserve their database/configuration. The user confirmed a successful v58-to-v59 CLI update.

The wizard offers a random or chosen port, optional domain/IP, generated private web path, admin credentials, Let's Encrypt, an existing certificate/key pair, or HTTP without SSL. Let's Encrypt HTTP-01 requires public TCP 80 to reach the challenge listener.

The panel package contains the matching pinned IP-limit Xray binary and GeoIP/GeoSite data. Users do not download Xray separately. Standalone Xray and matching source assets also include SHA-256 files.

## Build and release

Each push to main builds the Linux amd64 panel and Rafael Xray fork bundle and uploads it as a downloadable Actions artifact. A version tag builds the same package and publishes a GitHub Release; this workflow publishes v59 for tag v59.

Local Windows build (Go and tar required):

```powershell
./build-release-v59.ps1
```

The bundle is build-v59/rafael-linux-amd64.tar.gz and contains rafael/bin/xray-linux-amd64. The release also contains the standalone Xray binary, pinned Xray source archive, and SHA-256 sidecars.

Clean-server installs, certificate issuance/renewal, and additional IP-limit runtime acceptance remain open in BUILD_V59_CHECKLIST.md.
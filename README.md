# Rafael Panel

Rafael Panel is an enhanced Xray management panel based on [MHSanaei/3x-ui](https://github.com/MHSanaei/3x-ui) v2.9.2. It adds database-backed client controls, reseller management, and an IP-limit capable Xray fork that is bundled with each Linux release.

[Latest release](https://github.com/Rafael7277/rafael-panel/releases/latest) · [Installation guide](docs/GITHUB_INSTALL.md) · [License](LICENSE) · [Third-party notices](NOTICE)

## Features

- **Client IP limits:** Enforced by the bundled Rafael Xray fork, with client identity tracking and active-IP handling.
- **Traffic coefficients:** A coefficient scales newly granted quota and resets. Existing effective quota is preserved when only the coefficient changes. A blank or zero coefficient means 1:1.
- **Multi-link management:** Group client aliases and manage inbound selection, traffic, and IP-limit settings. Live IP counts are shared only among aliases served by the same Xray process; they are not global across separate servers.
- **Reseller access:** Create reseller panels with assigned-inbound scopes, section permissions, user and traffic quotas, and individual portal paths.
- **Adaptive Smart balancing:** Temporarily removes unhealthy outbounds from selection and restores recovered ones. Network conditions mean it cannot guarantee zero packet loss.
- **Guided installation:** Choose a panel port and web path, configure a domain or IP, and select optional TLS during setup.
- **Self-contained Linux package:** The matching Rafael Xray binary, GeoIP/GeoSite data, installer, updater, licenses, and checksums are included. A separate Xray download is not required.

## Quick install

Run as `root` on Debian or Ubuntu Linux amd64. The installer verifies the release checksum and guides a clean installation:

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/Rafael7277/rafael-panel/main/install.sh)
```

To install or upgrade to a specific release:

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/Rafael7277/rafael-panel/main/install.sh) v63
```

For an existing managed Rafael installation, use the Rafael CLI updater so its database and configuration are backed up and preserved.

## Flat install from the release archive

```bash
curl -fL -o rafael-linux-amd64.tar.gz https://github.com/Rafael7277/rafael-panel/releases/download/v63/rafael-linux-amd64.tar.gz
curl -fL -o rafael-linux-amd64.tar.gz.sha256 https://github.com/Rafael7277/rafael-panel/releases/download/v63/rafael-linux-amd64.tar.gz.sha256
sha256sum --check rafael-linux-amd64.tar.gz.sha256
tar -xzf rafael-linux-amd64.tar.gz
cd rafael
bash ./install-local.sh
```

## Release notes

See [v63 release notes](docs/releases/v63.md) for quota behavior, performance diagnostics, and compatibility details. Installation options, TLS prerequisites, and upgrade guidance are in the [installation guide](docs/GITHUB_INSTALL.md).

## Licensing

Rafael Panel is distributed under [GNU GPL-3.0](LICENSE). The bundled Xray fork retains its [MPL-2.0 license](third_party/xray-core/LICENSE). See [NOTICE](NOTICE) for third-party attributions.

# Installation

Rafael Panel currently publishes Linux amd64 packages. Run the installer as `root` on a supported Debian or Ubuntu server.

## Install the latest release

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/Rafael7277/rafael-panel/main/install.sh)
```

To select a release explicitly, pass its tag:

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/Rafael7277/rafael-panel/main/install.sh) v63
```

The installer guides you through the panel port, optional TLS certificate setup, and panel web path. Keep a copy of your database and configuration before making manual changes to an existing installation. For upgrades of a managed installation, use the Rafael CLI updater so it can validate the package and preserve the existing configuration and database.

## Flat installation from a release package

Download the package and its checksum from the matching [GitHub release](https://github.com/Rafael7277/rafael-panel/releases/latest), then verify and install it:

```bash
curl -fL -o rafael-linux-amd64.tar.gz https://github.com/Rafael7277/rafael-panel/releases/download/v63/rafael-linux-amd64.tar.gz
curl -fL -o rafael-linux-amd64.tar.gz.sha256 https://github.com/Rafael7277/rafael-panel/releases/download/v63/rafael-linux-amd64.tar.gz.sha256
sha256sum --check rafael-linux-amd64.tar.gz.sha256
tar -xzf rafael-linux-amd64.tar.gz
cd rafael
bash ./install-local.sh
```

The archive bundles the panel, its matching Rafael Xray binary, GeoIP/GeoSite data, installation and update scripts, licenses, and notices. No separate Xray download is needed.

## TLS and network access

TLS is optional during setup. If enabled, provide a domain that resolves to the server and allow the selected panel port through the server firewall. If TLS is not configured, use HTTP and restrict access to the panel port with a firewall or trusted network.

## Upgrading

For an installed Rafael Panel, run the updater from the panel CLI and select the desired release. The updater validates the archive and checksum and stages the new release before activation. If health checks fail, it attempts to restore the previous release. Keep the database backup created by the updater until the upgraded panel has been confirmed healthy.

## Support information

When reporting an installation issue, include the Rafael Panel version, Linux distribution and architecture, the installer/updater step, and relevant redacted logs. Never post passwords, private keys, access tokens, database dumps, or unredacted client data.

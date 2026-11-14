# patai node for the Jetson AGX Xavier

A patai node: a Jetson AGX Xavier 32 GB on a Connect Tech Rogue (AGX101) carrier, running an OS image with A/B
updates and rollback, its own web console, and apps such as an OpenAI-compatible AI API. It's complete on a LAN with
no internet and no cloud account; [patai_cloud](https://github.com/patrickwasp/patai_cloud), when it exists, is an
optional client of the node's API.

| | |
| --- | --- |
| Software | Jetson Linux **R35.6.2** (JetPack 5.1.5, the last release for Xavier) and **CTI-L4T AGX-35.6.2-V001** |
| Board config | `cti/xavier/rogue/base` |
| Storage | 32 GB eMMC on the module (the OS, A/B); an NVMe in an M.2 M-key slot (`/data`: apps, models) |
| Login | `admin`, SSH key only, given at provisioning or on the console's Access page |

## Layout

```
overlay/          the OS's own files, applied by docker/bin/build-rootfs
console/          the node's console and API (Go, with a Next.js UI embedded), and the update channel's checker
bundle/hook       RAUC's hook: points a slot at its own root partition, keeps the node's machine-id
apps/             what an owner turns on: each app's manifest, naming its APIs and the services behind them
workloads/        the services: Compose recipes, each with the manifest pinning its image and models
docker/           the Ubuntu 20.04 build image, and the tools that run inside it
scripts/          what you run on the PC
tests/            tests for the Python in overlay/
vendor/MANIFEST   third-party files the build fetches, by hash
docs/             how it works
```

## Build

On an x86-64 Ubuntu PC with Docker. The Ubuntu 20.04 image carries NVIDIA's and Connect Tech's tools.

```bash
scripts/fetch-vendor                          # NVIDIA's and Connect Tech's BSPs, by hash
scripts/build-image                           # the 20.04 build image
scripts/build-product 2026.11.14-1            # rootfs, factory images and bundle image in out/<version>/
PATAI_RAUC_KEYS=<dir> scripts/build-bundle 2026.11.14-1   # the signed update
```

`<dir>` holds the fleet's signing key (`scripts/new-fleet-key` makes one, once). Keep it in a secret store and on
tmpfs only while it signs.

## Flash a node

Put the node in Force Recovery (`docs/hardware.md`), with J3 cabled to the PC:

```bash
scripts/new-unit 2026.11.14-1 <provision-dir>
```

It flashes the eMMC, writes `<provision-dir>` to the node's persistent partition, and prints a handoff card with the
console's address, a one-time claim code and the certificate's fingerprint. The owner claims the node at
`https://<name>.local/`. `scripts/flash-product` says what `<provision-dir>` can hold: SSH keys, a hostname, the apps
to turn on, and the update channel.

## Release

```bash
PATAI_RAUC_KEYS=<dir> scripts/sign-latest <channel> <version> [folder]
```

Publish the bundle first and the channel's signed `latest.json` last (`docs/updates.md`).

## Docs

| | |
| --- | --- |
| [architecture](docs/architecture.md) | The design: storage, the OS, updates, apps, distribution |
| [hardware](docs/hardware.md) | Ports, switches, power, recovery and fans on the Rogue |
| [updates](docs/updates.md) | RAUC on NVIDIA's A/B rootfs, channels, keys, identity, build and release |
| [console](docs/console.md) | The node's console and API: claiming, HTTPS on a LAN, network, diagnostics, state |
| [apps](docs/apps.md) | Apps and services, their manifests, the gateway, keys and memory |

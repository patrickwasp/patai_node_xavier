# Architecture

One generic image for every node, A/B updates with rollback, and apps in Docker. A node is complete with no internet
and no cloud account: [patai_cloud](https://github.com/patrickwasp/patai_cloud), when it exists, is only ever an
optional client of the node's own API.

## Constraints that shape everything

- **The Xavier ends at JetPack 5.1.5, Jetson Linux R35.6.2.** CUDA 11.4, a Volta GPU (`sm_72`), 32 GB of memory
  shared by the CPU and GPU. The BSP is frozen, so the bootloader rarely if ever changes.
- **Connect Tech's Rogue support is an overlay on NVIDIA's Ubuntu 20.04 L4T.** NVIDIA's flashing tools run only on an
  x86-64 Ubuntu 18.04 or 20.04 host, so everything that touches a board runs in this repo's 20.04 image.
- **Power:** a node runs at MAXN unless its owner keeps another mode. Under combined load that needs 15 V or more on P6
  and peaks near 67 W (`hardware.md`), so a node comes with a supply of 15 V or more.

## Storage

```
 eMMC 32 GB   bootloader A/B · APP (slot A) · APP_b (slot B) · /persist
 NVMe         /data: Docker, app images, models, app data
```

- **The OS lives on the eMMC, A/B.** Everything a node needs to boot, stay reachable, report and take an update is a
  native service there: RAUC, the health check, the console, Docker and NVIDIA's Container Toolkit.
- **Apps and models live on `/data`,** where an OS update can't reach them, so an update replaces only the OS.
  `/data` is mounted `nofail`: a node with a dead or missing NVMe still boots, takes updates and reports the fault.
  An NVMe the node hasn't accepted stays unmounted until its owner chooses keep or erase at claim (`console.md`).
- **`/persist`** holds the node's identity, its provisioning and its owner's settings, and survives updates.

## The OS

- A minimal headless Ubuntu 20.04 from NVIDIA's `nv_build_samplefs.sh --flavor minimal`, Connect Tech's BSP with its
  kernel built from CTI's source for cgroup v2 (`kernel.md`), and `overlay/`, built by `scripts/build-product`. No desktop: `patai-screen` draws a status page on HDMI with the node's
  name, addresses and a QR code for its console.
- **Fleet-generic.** No users, keys, hostnames or owner URLs; `docker/bin/check-rootfs` fails a build that has any.
  At first boot a node names itself `patai-<module serial>` and makes its SSH host keys and `machine-id` on
  `/persist`. What makes a node its owner's goes on `/persist` at the flash, never in the image.

## Updates

Signed RAUC bundles, installed to the inactive slot. A node follows one channel, whose `latest.json` the fleet key
signs. The new slot has to boot and pass a health check, or the bootloader falls back to the old one (`updates.md`).

## Apps

An owner turns on apps; an app is made of services; a service is a Compose recipe in `workloads/` whose
`manifest.json` pins its image and models by hash. The recipes ship in the OS, so the signed OS vouches for everything
that runs. `patai-fetch` downloads and checks what a recipe pins, `patai-workload` runs it, and a gate keeps Docker
from restarting a container nothing checked. What's on has to fit the memory kept for apps (`apps.md`).

## Distribution

Everything is public on GitHub, so neither a node nor the flasher needs a credential:

- OS bundles, each channel's signed `latest.json` and the flash packages are release files of this repo
  (`updates.md`, Releases).
- App images are on GHCR, pinned by digest, and share one CUDA base layer.
- Models are in the release `models`, in 1 GiB pieces that download several at once, each pinned by its SHA-256.
  A model whose licence doesn't allow a copy comes from its own address.
- `flash_new_node_base.sh` flashes the OS and the console; `flash_new_node_chat.sh` also writes the NVMe with the
  AI API and Chat apps and their default models. Both run on an x86-64 Ubuntu PC.

## Management

Local first: each node has its own console and API, claimed with a one-time code and complete with no internet.
Credentials a node makes for itself stay on it. `console.md` covers the processes, the API, claiming, HTTPS on a LAN
and what survives a rollback.

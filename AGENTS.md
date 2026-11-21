# patai_node_xavier

A patai node on a Jetson AGX Xavier 32 GB with a Connect Tech Rogue (AGX101) carrier. Read `README.md` for the
workflow and `docs/hardware.md` before touching a board. Design: `docs/architecture.md`.

How the console looks (colour, type, the rings mark, motion, words) is patai's, the same in every patai app: patai_docs
[`design/README.md`](https://github.com/patrickwasp/patai_docs/blob/main/design/README.md) and `design/tokens.json`.
Read it before changing the console or the HDMI screen; its last table says what the console still has to change.

The product is a **fleet**: one generic image, A/B updates with rollback, apps in Docker. A node is complete with no
internet and no cloud account; [patai_cloud](https://github.com/patrickwasp/patai_cloud) is only ever an optional
client of its API. Research and measurements are kept apart, in patai_docs, so this repo holds only what builds, runs
and flashes a node.

## Rules

- **Greenfield until the first node ships.** No node exists, so nothing keeps compatibility with earlier releases or
  their state: no migrations, shims or legacy files, and any found go. Change a state format freely. Use each
  component's newest release. The A/B update and rollback machinery stays.
- **Images are fleet-generic.** Nodes belong to different owners: no personal or owner-specific users, keys,
  hostnames or URLs in any image, and `docker/bin/check-rootfs` fails the build if it finds one. Those go in at
  provisioning, onto the persistent partition (`scripts/flash-product`).
- **Everything that touches the Jetson runs in the Ubuntu 20.04 image** (`docker/Dockerfile`, `scripts/lib.sh` →
  `l4t_run`). NVIDIA's R35 tools and CTI's BSP support only 18.04/20.04 hosts. Don't install flashing tools on the host.
- **Vendor files are fetched, never stored.** A new one goes in `vendor/MANIFEST` with its SHA-256
  (`scripts/fetch-vendor --pin`).
- **The kernel is CTI's, built here from CTI's source** with `docker/kernel/config` (`docs/kernel.md`), and a node
  boots cgroup v2. A change to `docker/kernel` takes one more `kernel_build` in `scripts/lib.sh`, which names the l4t
  image.
- **No credential in the repo or an image.** The fleet's signing key is given to the scripts as a folder
  (`PATAI_RAUC_KEYS`), never committed. Credentials a node makes for itself stay on it.
- **Keep images small,** OS and app alike, so an A/B update or an app install is quick.
- **A channel's `latest.json` is signed.** Write one with `scripts/sign-latest <channel> <version>`, never by hand. A
  node takes nothing from its channel without `update-channel-name`, and never a file older than it has seen from it:
  to go back, sign a new file naming the older release (`docs/updates.md`, Channels).
- **A service pins its image and models** in its `manifest.json`, so a rebuilt image or a new model needs a new OS
  release. Services mount `/data/models` read-only. An owner may add GGUF or safetensors, never a pickle checkpoint.
  Keep the gate in every recipe (`docs/apps.md`, Running them). A node pulls each image by its pinned digest, even
  one it built, and GHCR makes a new package private: until Patrick makes it public, nodes get `unauthorized`.
- Something on `/persist` that has to survive a power cut needs a sync before the rename, and of its directory after.
  ext4 only flushes first when a rename replaces a file, so a new file comes back empty (`console/internal/durable`).
- Don't run `apt-get upgrade` on a node: CTI pins its own L4T packages.
- `console/STATE_FORMAT` and `console/internal/store` move together.
- **Release N's data works with release N-1's code.** Only the OS is A/B: `/data` is both slots', so a rollback, a
  channel signed to go back or a restore runs older code on newer data. A service declares its data's `format`
  (version and floor) and a Postgres one its `cluster`, and `patai-formats` refuses an install, an offer, a restore or
  a start that would put code below the data's floor (`docs/updates.md`, Data across slots). Each Postgres major and
  one-way change has a folder of its own, which `patai-postgres` takes over into and keeps.
- **A backup holds every credential on a node**, so it's encrypted to a key only the owner keeps; the node keeps its
  public half and the seal, never the key (`docs/console.md`, Backups). A service that keeps state keeps it in its
  `data` folder, which backups hold, or it isn't backed up. A backup restores onto any kind of node: a new file in
  `/persist/provision` that only one kind has, like the power mode, goes in `backup.PlatformFiles`, and a database's
  folder is backed up as a dump (`data.backup: postgres`), never as its own files.
- **A credential the console keeps for somewhere outside is sealed** with `internal/sealing`'s unit key, which is never
  in a backup (`docs/console.md`, Where backups go is sealed).

## Board facts that are easy to get wrong

- Tailscale is the owner's, off until they turn it on (`docs/console.md`, Remote access, With Tailscale): `tailscaled`
  starts only while `/persist/tailscale/on` is there, in kernel mode, never userspace networking, which would expose
  the loopback workloads to the tailnet. Over SSH, `sudo tailscale status`.

- Flash/recovery port is **J3**, the USB-C next to the micro USB. It's a device port only in Force Recovery; after a
  normal boot it's a USB host port again, so no USB networking to the PC.
- **HDMI is dark until Linux starts**: CTI carriers don't use display head 0, which is all the Xavier bootloader
  drives. Watch early boot on the serial console (`scripts/console`).
- Fans: P11 (top edge, "12V FAN") is 12 V only; P8 (under the module) is 5 V only.
- Power modes: use `sudo patai-power-mode <id>`, which keeps the mode for every boot. Without one kept a node runs at
  MAXN. Plain `nvpmodel` lasts only until an update.
- Feed P6 at 15 V or more for MAXN, which a node runs at by default. Under combined load a node resets at 12 V on any supply. Don't read such a reset
  as a software fault (`docs/hardware.md`).
- `poweroff` restarts a node (S1 auto power-on). Before cutting power, `sudo systemctl halt` or Shut down on the
  System page, which says when it has halted. The watchdog starts a halted node again about 150 s later.
- Without an RTC battery a power cut puts the RTC back to 2000-01-01. Without NTP the clock then starts from the last
  time timesyncd saved, because `patai-clock` replaces NVIDIA's `nvrtc-sync-boot`.
- Board config: `cti/xavier/rogue/base` (`cti/xavier-I/...` is for the Industrial module).
- `nvbootctrl set-active-boot-slot` fails here and still exits 0. Use `tegra-slots` (`docs/updates.md`).
- Don't probe registers with `devmem`: a single bad read is a fatal kernel BUG.
- `/lib` is a symlink: extract anything into `/` with `tar --keep-directory-symlink`, or tar replaces it and the slot
  won't boot.
- Workload ports are on loopback. From the network a service is reached only through the console's gateway, with a key,
  and an app's own pages through the console on their own port, such as Chat's 8443, or at the unit's address when the
  owner puts the app in front; the console is always on 9443 too, and only there then (`docs/console.md`, The front
  page). Scripts call the console's API on 9443. Containers reach the gateway for
  apps on 18000, with their app's own key, and nothing else does (`docs/apps.md`, The gateway).
- A container's memory limit doesn't cap CUDA memory: nvmap allocates it outside the cgroup (`docs/apps.md`).
- The console's services, Docker's daemons, SSH and systemd-resolved keep their memory under pressure
  (`MemoryMin=`/`MemoryLow=`), and `system.slice.d/50-memory.conf` carries their sum, so a change to one is a change
  there (`docs/console.md`, Memory). It keeps CPU memory only. Don't push a node under about 1 GiB available on
  purpose: near the bottom its kernel hangs (`docs/hardware.md`, Huge pages).
- Docker mounts a container's cgroup read-only. One that limits processes of its own, as the code interpreter does
  its workers, runs on Docker's runtime `patai-delegate` (`patai-runc`), which refuses it unless nothing in it is
  root or has a capability: below its top, which `nsdelegate` guards, only file ownership keeps its limits root's.
- An NVMe the node hasn't accepted isn't mounted, and Docker stays off with it: `sudo patai-data-disk status`.
- A factory reset removes `admin`'s SSH keys, so to get back in, claim the node and add one on the Access page.
- A node that isn't where DHCP would put it may have a static address its owner set. `<name>.local` follows it. Over
  the serial console, `sudo patai-network reset` puts both ports back on DHCP.
- If both slots show `unbootable`, the UEFI menu can reset them over serial: ESC at the prompt, then Device Manager >
  NVIDIA Configuration > L4T Configuration > OS chain A/B status, F10, Y.

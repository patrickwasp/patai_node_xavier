# Updates

RAUC on NVIDIA's A/B root filesystem: what the firmware does, how a node takes an update and rolls back, and how
releases are signed and offered.

## What the platform gives us

- **Rootfs A/B** when flashed with `ROOTFS_AB=1`: partitions `APP` and `APP_b`. A rootfs slot is the same thing as a
  firmware boot chain, so a switch also changes the bootloader chain. UEFI's L4TLauncher boots the kernel, DTB and
  initrd from the selected slot's `/boot`, so **one rootfs image carries the kernel too**.
- **Each slot names its own root**: `extlinux.conf` holds `root=PARTUUID=<that slot>`, and the PARTUUIDs differ per
  flash, so `bundle/hook` rewrites that line for the slot it wrote.
- **Linux can't write UEFI variables here.** `efivarfs` is read-only, and `nvbootctrl set-active-boot-slot` fails
  while still **exiting 0**. As NVIDIA's OTA tool does, a switch writes files named `<variable>-<guid>` to
  `EFI/NVDA/Variables` on the ESP, which UEFI imports at the next boot and deletes. The first 4 bytes are the
  attributes (7); a file with nothing after them deletes the variable.
- **A finished switch blocks the next one.** `BootChainFwStatus` stays set after a switch, and UEFI drops any new
  `BootChainFwNext` while it exists, so each switch queues its deletion too.
- **Boot validation**: `l4t-rootfs-validation-config.service` runs `/usr/sbin/user_rootfs_validation.sh` at every
  boot, and a non-zero exit reboots. Then `nvbootctrl verify` resets the running slot's retry count. `overlay/` makes
  NVIDIA's unit `oneshot`, or verify wouldn't wait for the check.
- **Rollback**: UEFI counts down a slot's retries at each boot until verify resets them. At 0 it marks the slot
  `unbootable` and switches chain itself. Any reboot before verify costs a retry, so don't reboot by hand in a boot's
  first 90 s.
- **Retry counts are in scratch register `0x0C39041C`**: magic `FACE`, the current slot, then B's and A's counts.
  `set-primary` clears it. **Touch only this address**: a bad register access is a fatal `tegra194-cbb` BUG.
- **Stock kernels hang on a panic**: `kernel.panic` is 0 and CTI's device tree disables the watchdog.
  `patai-boot-config` adds `panic=10` and enables it, in each slot's `/boot`, so it travels with the image.
- **The watchdog only catches a hung kernel**: the driver feeds it from an interrupt. A stuck userspace is caught by
  a `multi-user.target` deadline (`JobTimeoutSec=300`, `reboot-immediate`).
- **The Security Engine must stay off.** dm-verity picks `tegra-se-sha256` for verity bundles, and it crashes about
  30 s into an install, so `patai-boot-config` disables its device tree nodes.
- **`verify` fails once after each switch**, because it can't delete `BootChainFwStatus`. `rauc status mark-good`
  queues that, and a drop-in accepts the failure only while the variable exists.

Each of these rolls back by itself: a failed health check, a kernel panic, a hung kernel, a stuck userspace, a
destroyed userland or filesystem, and a release the node's own updater installed. A bundle signed by another key, or
with one byte changed, is refused before anything is written. A power cut during an install leaves the old slot
running and the new one never activated, since RAUC activates a slot only after writing it.

A destroyed filesystem drops the initrd to a 30 s prompt on the serial console before it reboots, so a keypress
there stops the rollback. With both slots bad there's no recovery image, and the node needs a factory flash.

## RAUC setup on the node

- **RAUC 1.15.2, built from source** (`docker/rauc.Dockerfile`): Ubuntu 20.04 has 1.2, with no verity bundles.
- `overlay/etc/rauc/system.conf` is fleet-generic. `data-directory` is on `/persist`, not `/data`: the OS must not
  depend on the NVMe. The slots have `resize=true`, so RAUC grows the filesystem after writing it.
- `/usr/lib/rauc/tegra-slots`: RAUC's custom backend. Reads come from `efivarfs`, writes are queued on the ESP as
  above, so `get-primary` and `get-state` check the queue first.
- `bundle/hook`, a slot hook: sets `root=PARTUUID` for the slot being written and copies in the node's
  `machine-id`. Before an install it refuses a bundle for another product, or one whose console state the running
  release couldn't read back after a rollback (`console/STATE_FORMAT`).
- **The health check** checks only what the OS provides itself: sshd up, the console answering (`console.md`, Boot
  validation) and `rauc status mark-good`. Not the network, since a pulled cable must not roll a node back, and not
  `/data`. It waits for sshd, which NVIDIA's `nvfb.sh` restarts on a slot's first boot.
- Extract anything into `/` with `tar --keep-directory-symlink`: `/lib` is a symlink, and plain GNU tar replaces it
  with a directory, which destroys the slot.
- **RAUC answers nothing else while it installs.** `rauc status` prints nothing and exits 0. D-Bus properties still
  answer, which is where progress comes from.

## Update flow

1. A release is an ext4 image of the product rootfs in a **verity-format RAUC bundle** signed with the fleet key
   (Build and release).
2. The bundle and a signed `latest.json` (Channels) are GitHub release files (Releases).
3. `patai-update.timer` reads the channel from `/persist/provision/update-channel` every 6 hours, fetches
   `latest.json`, has `patai-channel` check it, and if it names another version downloads the bundle whole: to
   `/data/.patai-update` if the data disk has room, otherwise to `/run/patai-update`. A download resumes with a range
   request. Then `rauc install --require-manifest-hash=<hash>` checks it, writes the inactive slot and makes it
   primary. A release that was installed is recorded in `/persist/rauc/update-tried`, so one that rolled back doesn't
   install again. With `update-hold` the timer only checks, and the release waits for a person.
4. The node reboots into it at once with `update-reboot`, or at its next boot. The health check passes and marks it
   good, or the bootloader falls back and the old slot reports the failed version.

A person can do step 3 from the console's Updates page, from the channel or a file (`console.md`, Updates). It still
runs in `patai-update`, so an install outlives a console restart, and it never reboots for a person.

## Channels

RAUC checks that a bundle is signed, not which release it is, so on its own a server could offer any older signed
release. So `latest.json` is signed too, with the same fleet key, and a node takes only what it says.

**The file** is an envelope, `{"format": "patai-latest/1", "payload", "certificate", "signature"}`, all three base64.
The payload is checked as the exact bytes signed:

```json
{"compatible": "patai-node-xavier", "channel": "stable", "sequence": 1793713966,
 "version": "2026.11.03-1", "bundle": "2026.11.03-1/patai-2026.11.03-1.raucb", "manifest_hash": "<64 hex>",
 "size": 908507736}
```

`size` is the bundle's, at most 4 GiB, so a node knows it has room before it starts and cuts off a server that sends
more. The signature is RSA PKCS#1 v1.5 with SHA-256 over `patai-latest/1\n` and the payload, so it can't be taken for
anything else the key signs. A field added later is signed too, and a node that doesn't know it ignores it.

**Checking it** is `patai-channel verify /etc/rauc/keyring.pem` (`console/internal/channel`), a static Go binary. It
takes an RSA key of 4096 bits or more in a code-signing certificate issued by a CA in the keyring. The certificate's
dates aren't checked, because a node's clock can be years out; RAUC still checks the bundle's. Then `patai-update`
decides:

- **The channel's name** has to be the node's, `/persist/provision/update-channel-name`. Another channel may name an
  older release on purpose, so its file can't stand in for this one's. A node with no name installs nothing.
- **The sequence** is the time it was signed, in Unix seconds. Each channel's newest is kept in
  `/persist/rauc/channel-floors.json`, written to survive a power cut, and a file older than it is refused, as is the
  same sequence with a different payload.
- **Nothing signed before the running release was built** (`/usr/share/patai/built-at`) is taken. That covers a fresh
  flash, a factory reset and a lost `/persist`.
- **The bundle** is installed with `--require-manifest-hash`, so a server can't put another signed release at its
  address.
- **RAUC starts only on an offer that still stands:** the same channel and name, and nothing newer seen since.

**To roll a fleet back on purpose,** sign a new file naming the older release. A file an owner installs by hand is
the owner's call, older or not.

**What it can't do:** stop a server offering the same old file for ever, since a node with no trusted clock can't
tell a quiet channel from a stalled one. The Updates page says when the file was signed.

**Publishing** is `scripts/sign-latest <channel> <version> [folder]`. It signs at the current time and refuses a
clock NTP hasn't set, since a file from the future would stop every node that saw it. It checks the result as a node
would, refuses to replace a newer file or another channel's, keeps each old one as `latest-<sequence>.json`, and
replaces `latest.json` last. It also refuses a release whose `built-at` is in the future, as `flash-product` does.

## Keys

- **Two levels**, so a leaked or expired signing key doesn't mean touching every node: a fleet CA, whose certificate
  is `/etc/rauc/keyring.pem` in the image (`overlay/`; public, so it's in the repo), and a signing key issued by it.
  RSA-4096. The signing certificate is for code signing, so `system.conf` sets `check-purpose=codesign`.
- **`scripts/new-fleet-key <dir>`** makes them once, into a folder on tmpfs, and refuses to replace an existing CA,
  since every node trusts it. The private keys never go in the repo or an image: keep them in a secret store.
- **Signing** takes `PATAI_RAUC_KEYS=<dir>`, a folder holding the signing `key.pem` and `cert.pem`, for
  `scripts/build-bundle` and `scripts/sign-latest`. The same key signs bundles and every channel's `latest.json`.
- The image is the same for every owner; owners differ only by the channel in their provisioning.

## Node identity and provisioning

A slot holds only what the image holds, so anything that makes a node itself lives on `/persist` (`UDA`, written by a
factory flash and formatted by nothing else), and `patai-identity` puts it back at every boot, before the network:

| What | Where | Made by |
| --- | --- | --- |
| `machine-id` | `/persist/identity/machine-id` | systemd at the first boot; `bundle/hook` copies it into a new slot |
| SSH host keys | `/persist/identity/ssh_host_*` | `patai-identity` at the first boot |
| Hostname | `/etc/hostname` | `patai-<module serial>`, or `provision/hostname` |
| Owner's SSH keys | `/persist/provision/authorized_keys` | Provisioning or the console |
| Update channel | `/persist/provision/update-channel`, `update-channel-name`, `update-hold`, `update-reboot` | Provisioning or the console |
| Apps to run | `/persist/provision/apps.json` | Provisioning or the console (`apps.md`) |
| Serial console password | `/persist/provision/admin-password-hash` | Provisioning, optional |
| Claim code, TLS certificate | `/persist/console/` | `scripts/new-claim` at a flash (`console.md`) |
| Permission to format the NVMe | `/persist/provision/format-nvme` | A factory flash; used once, for the first disk seen |
| The NVMe this node accepted | `/persist/data-disk` | `patai-data-disk` (`console.md`, The data disk) |

Every image has one account, `admin`: sudo and Docker, password locked, no keys. So an update can't change who can
log in. `docker/bin/check-rootfs` fails the build otherwise.

**Provisioning is flash-time.** `scripts/flash-product <version> <dir>` writes `<dir>` to `/persist` as an ext4 image
on `UDA`, so the system images stay generic and an owner's node has access from its first boot. A reflash wipes
`/persist`, so a reflashed node has new host keys and needs its provisioning again.

If `/persist` won't mount, `patai-identity` makes a temporary identity in `/run`, so sshd starts and the health check
passes: a bad `UDA` isn't a bad release. Nobody can log in then, so that node needs a reflash.

## eMMC layout (ROOTFS_AB=1)

29.1 GiB usable: CTI's default 14 GiB slots and no extra partition. The flash tool keeps the recovery partitions at
fixed addresses, so `UDA` stays 283 MB at the end either way, plenty for `/persist`. The partition table can't change
through RAUC, so it's fixed for a node's life. Flash images are sparse and bundles carry a compact image, so slot size
doesn't change flash or install time.

## Build and release

`scripts/build-product <version>` runs from a clean checkout, in this repo's Ubuntu 20.04 image, in two stages. The
image already has the node's kernel in place of CTI's (`kernel.md`).

1. `docker/bin/build-rootfs`: NVIDIA's `--flavor minimal` samplefs from a pinned Ubuntu base, CTI's installer, Docker
   and the NVIDIA runtime, RAUC, `overlay/` and the `admin` account. Output: `rootfs.tar.zst` and a package list.
   `check-rootfs` fails the build on anything node-specific. Host `/dev` stays out of this container, because
   NVIDIA's script deletes a tree it bind-mounted `/dev` into.
   `slim-rootfs` then keeps only what a node runs: it purges `docker/rootfs-drop`, marks every package automatic
   except `docker/rootfs-keep`, the required and important ones and NVIDIA's and CTI's, and lets apt remove the rest,
   along with docs and man pages. That takes 983 packages to about 400 and the compressed rootfs from 840 to 460 MB.
   NVIDIA's `3d-core` needs its wayland and x11 packages, so they stay.
2. `docker/bin/build-images`: `flash.sh --no-flash` makes the slot images, since it finishes the rootfs as it does
   (DTB, command line, `nv_boot_control.conf`). `patai-boot-config` runs on those, and the bundle image is cut from
   slot A's, so **a flash and a bundle hold the same filesystem**. Output: `factory/` for `scripts/flash-product` and
   `rootfs.ext4` for `scripts/build-bundle`.

Focal is out of standard support and NVIDIA's `r35.6` repository is end of life, so the packages are effectively
frozen; the build records them in `rootfs.packages`.

`build-product` records the commit it built from in `commit`, with `-dirty` if the checkout had changes.

## Releases

Releases are this repo's GitHub releases, so nothing a node or a flasher downloads needs a credential:

| Release | Holds |
| --- | --- |
| `<version>` | `patai-<version>.raucb` and `rootfs.packages`, tagged at the commit it was built from |
| `channel-<name>` | the channel's signed `latest.json`, and each one before it as `latest-<sequence>.json` |
| `models` | pinned models, in pieces (`apps.md`) |

A node on a channel has `update-channel` set to `https://github.com/patrickwasp/patai_node_xavier/releases/download/channel-<name>`
and `update-channel-name` to `<name>`. GitHub answers each file with a redirect, which `patai-update` follows.

To release, from a clean checkout:

```bash
scripts/build-product <version>
PATAI_RAUC_KEYS=<dir> scripts/build-bundle <version>
scripts/publish-release <version>                              # the bundle only, to flash and try first
PATAI_RAUC_KEYS=<dir> scripts/publish-release <version> stable  # then offer it
```

`publish-release` refuses a release built with changes, uploads the bundle before it signs anything, never replaces a
file a release already has, and checks each channel's new `latest.json` against the one it offers now. Replacing
`latest.json` leaves a moment with none, which a node treats as a channel it couldn't reach. Offering a release on
`stable` marks it GitHub's latest. GitHub takes files under 2 GiB; a bundle is about 500 MB.

A service's image is pinned in its `manifest.json`, which ships in the OS, so a new app image needs a new OS release.

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
  `machine-id`. Before an install it refuses a bundle for another product, one whose console state the running
  release couldn't read back after a rollback (`console/STATE_FORMAT`), or one the apps' data on `/data` rules out
  (Data across slots).
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
replaces `latest.json` last. It also refuses a release whose `built-at` is in the future, as `flash-product` does, and
one the apps' data rules out on that channel (Data across slots), so the folder holds every file it has offered.

## Data across slots

Only the OS is A/B. What the apps keep is on `/data`, which both slots share, so older code meets data a newer release
wrote three ways: a rollback to the other slot, a channel signed to go back to an older release, and a backup restored
from a newer release. And the health check deliberately ignores `/data` and the apps (RAUC setup), so a slot whose
app can't start on its data is still marked good. Chat's database is the case that matters: it migrates at every
start, and nothing migrates back.

**The rules,** for every format an app keeps on `/data`: Chat's schema, its files, any other service's database.

- **Release N's data works with release N-1's code.** Expand, then contract: a release only adds, tables, nullable
  or defaulted columns, indexes, new files beside old ones; a drop or a rename waits for a later release than the
  one that stopped using it.
- **Migrations are incremental from the first shipped release:** `0000` frozen, one file for each change, made on
  main only.
- **Each migration is one transaction,** so one that fails changes nothing. Nothing that can't run in one, such as
  `CREATE INDEX CONCURRENTLY`.
- **Big backfills run in the background after start,** never in the boot migration.
- **Postgres is one-way** by major version and by an extension's update: each cluster has a folder of its own (below).

patai_chat `docs/schema.md` is Chat's side: its version file, its migrator, and the check that runs N-1's server tests
on N's schema.

### What a service declares

In its manifest's `data` (`apps.md`, What an app declares):

| | |
| --- | --- |
| `format` | `version`, what its code brings the data to, and `floor`, the oldest version whose code still works on that. Chat's come from its image's label, `patai.data-format`, which `scripts/publish-workloads` copies in |
| `postgres` | The Postgres service its database is in |
| `cluster`, `upgrades` | A Postgres service's: the cluster its image runs, and the older majors it has the binaries to `pg_upgrade` from |

Code reaches data at a newer version only if it's at or above that data's floor. The service writes what it last did
to `format.json` in its folder, `/data/chat-server/format.json` for Chat: `version`, `floor`, and `state`, which is
`migrating` (with the version it's moving to, so it never says less than the data holds), `ready` or `failed` (with
`error` and `detail`, and the version the data is still at). `data-formats.json`, what a release's services declare,
is made from the manifests into the image, `/usr/share/patai/data-formats.json`, and from there into its bundle.

### What refuses what

`patai-formats` (`console/internal/formats`) decides for each of them:

| Where | Refuses |
| --- | --- |
| `bundle/hook`, before an install, with the bundle's own `patai-formats` | A release older than the floor of the data on the unit (a downgrade too far). A release whose data the running one couldn't use, so it couldn't roll back: its floor above the running release's version. A Postgres cluster it can't run or take over, and a new major when the running release's own cluster has no folder to go back to |
| `scripts/sign-latest`, which signs what a release declares into `latest.json` as `data` | A release older than data any release the channel has offered may have left (from each `latest-*.json`); one the release the channel offers now couldn't roll back from, which its units would refuse; an older Postgres than the channel has offered |
| A restore, before it writes anything | A backup whose data is newer than this release reaches, and a dump from a newer Postgres major. Each backup's manifest records each service's `format` and the `cluster` that made its dump |
| `patai-workload`, before it starts a service | Data from a newer release than it reaches, and a cluster it can't run: the service fails with the reason, which the console's Apps page shows. That covers a rollback, which no hook sees |
| Chat, at start | The same, from the version its database keeps, and a migration that fails, which changes nothing. `format.json` says which, and the console's status carries it |

Each says it in the console as the service's title and the reason, then the versions, such as "Chat server: this
release is too old for its data on this unit (its data is at version 5, which needs 4 or newer; this release is 3)".

### Postgres clusters

Chat's Postgres keeps each cluster in a folder of its own, `/data/chat-postgres/<cluster>/docker`: the major, `18`, or
after a dot a generation for a one-way change within it, `18.1`, such as pgvector's `ALTER EXTENSION UPDATE`. The image
says its cluster, `PATAI_CLUSTER`, and `current` beside the folders says which ran last. At each start its entrypoint,
`patai-postgres`:

- **runs its own cluster if it's there,** even when a newer one ran last: that's how a rollback finds its own. Its data
  is as it was before the update; what was written since is in the newer folder.
- **takes over the one last run into a new folder** when its own isn't there: `pg_upgrade --copy` from a major whose
  binaries the image has (`UPGRADES` in its Dockerfile, from the PostgreSQL project's apt repository), a copy within its
  own major, then `ALTER EXTENSION … UPDATE` there. The old folder is left as it was.
- **takes over again** when its own is there but an older one ran since, after a rollback and an update back: the one
  last used is what Chat shows, and its own is set aside as `<cluster>.stale-<time>`.
- **refuses** a newer cluster whose own folder is gone, or one it can't take over, rather than make an empty one.
- **lets go of older clusters,** and those set aside, once a later release than the one that took over runs
  (`PATAI_RELEASE`, which `patai-workload` passes): the other slot then has a release that runs this cluster too.

A restore leaves the folder with only the dump, so a new cluster is made and loads it. A release that moves Postgres
to a new major changes nothing else of Chat's schema, so a rollback to the older cluster meets code that reads it.
`tests/patai-postgres.sh` runs all of this on a PC with real images, 17 to 18 and 18 to 18.1.

### Before a migration

Before a release moves a service's data forward (its `format.version` above what `format.json` says),
`patai-workload` waits for the database to answer and dumps it, `schema-<version>.dump` beside the clusters, owned by
root: one kept, the last, replaced at the next move. The migration is one transaction, so a failure changes nothing;
the dump is for one that worked and was wrong, and costs a few seconds once a release. A dump that can't be made is
said in the journal, and the service starts anyway. To go back to it by hand, with Chat off:

```bash
sudo docker run --rm --network none -v /data/chat-postgres:/var/lib/postgresql --user 10020:10020 \
  --entrypoint sh <chat-postgres image> -c 'pg_ctl -D "$PGDATA" -w start && dropdb -U patai patai_chat \
  && createdb -U patai patai_chat && pg_restore -U patai -d patai_chat --no-owner /var/lib/postgresql/schema-<version>.dump \
  && pg_ctl -D "$PGDATA" -w stop'
```

It's root's, so `chmod 644` it first, and the release that runs then has to reach that version.

### Until the first node ships

Greenfield: Chat's schema is one migration, rewritten in place, so a release moves a database only from its own
version (`from`), and a bench unit's takes the step by hand, with the SQL patai_chat's `scripts/check-schema.ts` writes,
before the new Chat starts. The floor, the folders and the checks are in place, so going incremental at the first ship
is one step (patai_chat `docs/schema.md`, At first ship). A greenfield change N-1 can't run on (a floor of its own
version) is refused over N-1: make it in two releases.

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

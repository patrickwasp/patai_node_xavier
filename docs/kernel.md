# Kernel

A node runs Connect Tech's kernel for the Rogue, R35.6.2 AGX V001, built here from CTI's own source with three
changes, so it can boot cgroup v2. CTI's BSP ships only the built kernel; the source came from their support on
request (2026-11-18).

## What changes

`docker/kernel/config`, on top of CTI's `cti_tegra_defconfig`:

| Option | Why |
| --- | --- |
| `CONFIG_CGROUP_BPF=y` | On cgroup v2, Docker's device filter is a BPF program attached to the container's cgroup, and the NVIDIA runtime's GPU devices pass through it |
| `CONFIG_PSI=y` | Pressure stall information: how long tasks wait for memory, CPU and I/O, in `/proc/pressure` and each cgroup |
| `CONFIG_MODULE_SIG` off | A node's modules are stripped of debug info, which takes their signatures too, and the kernel never required them: CTI's kernel was tainted by the first module it loaded. A signing key made at each build would also make each build different |

`olddefconfig` follows them with `PSI_DEFAULT_DISABLED=n` (on from boot) and `BPF_STREAM_PARSER=n`, and drops the
`MODULE_SIG_*` choices. `docker/kernel/build` fails if an option doesn't take, and prints the difference from
`cti_tegra_defconfig`.

`patai-boot-config` adds `systemd.unified_cgroup_hierarchy=1` to each slot's `extlinux.conf`, since focal's systemd
mounts cgroup v1 by default. That gives:

- **Cores come back by themselves.** cgroup v1 dropped an offline CPU from every cpuset for good, so after nvpmodel
  went up from a 4-core mode containers stayed on cores 0-3 until `patai-power-mode` widened them. v2 keeps what was
  asked for and gives a cpuset each core again as it comes online, so that code is gone.
- **Memory protection** (`MemoryMin=`, `MemoryLow=`), which v1 doesn't have: the console's services, Docker's
  daemons, SSH and mDNS keep their memory under pressure (`console.md`, Memory). focal's systemd mounts cgroup2 without
  `memory_recursiveprot`, so `system.slice` carries the sum of what its services are given.
- **The agentic API's code interpreter**, which needs v2. Not built yet (`apps.md`, The agentic API).

Docker 26 uses the systemd cgroup driver on v2, so a container is `system.slice/docker-<id>.scope`. `patai-rootd`
finds it from `/proc/<pid>/cgroup` of the container's first process and reads `anon` from `memory.stat` and
`memory.swap.current`, what v1 called `total_rss` and `total_swap`.

`CONFIG_RT_GROUP_SCHED` stays on, as CTI has it. On v2 a real-time process can't join a cgroup with the cpu controller
on, so a service or container that asks for `SCHED_FIFO` or `SCHED_RR` is refused. Nothing on a node does.

## Same as CTI's otherwise

- **The toolchain** is NVIDIA's for R35 kernels, Bootlin's GCC 9.3 (Buildroot 2020.08): the one CTI's kernel records
  in `CONFIG_CC_VERSION_TEXT`. The build checks for it.
- **The configuration.** `cti_tegra_defconfig` expands to CTI's kernel's `/proc/config.gz` but for six options, all
  for CTI camera drivers whose source isn't in CTI's archive: `VIDEO_CTI_CAM_PLATFORM` (built in), `CTI_FPDLINK`,
  `VIDEO_CTI_BOSON`, `VIDEO_CTI_JCB003`, `VIDEO_CTI_MAX929X` and `VIDEO_CTI_IMX390`.
- **The modules** are CTI's 1,139 but those drivers': `cti_boson_cam`, `cti_jcb003` and `cti_max929x`, and of what's
  built in, `cti_cam_common` and the GMSL and FPD-Link deserializers `cti-max96712`, `cti-max96792` and `ds90ub95x`.
  They're for CTI's camera adapters: the Rogue's device tree has no node for any of them, and on patai-dev none was
  bound. Every module comes stripped of debug info, as a node runs it, so the kernel package is 38 MB, not 392.
- **The device trees** are CTI's to the byte but for `nvidia,dtbbuildtime`, the time they were built.
- **The packages.** CTI's install.sh takes its kernel, device trees and kernel packages from `CTI-L4T/kernel`, and
  the build puts its own there: `nvidia-l4t-kernel`, `nvidia-l4t-kernel-dtbs` and `nvidia-l4t-display-kernel`, under
  CTI's names and versions, since `cti-l4t-utils` requires CTI's exact version of `nvidia-l4t-kernel`. Their
  descriptions say they were built here. Left out: the headers package, which nothing on a node uses, and the kernel
  package's `Image.backup` and update payloads, which held CTI's kernel and which nothing reads.
- `depmod` fails the build on a module needing a symbol nothing exports. NVIDIA's display modules, which a Xavier
  doesn't load, are built from the same archive.

## On a node

`scripts/check-kernel` checks a node over SSH: this kernel, `CONFIG_CGROUP_BPF` and `CONFIG_PSI`, cgroup v2 at
`/sys/fs/cgroup`, memory pressure, an untainted kernel, Docker on v2, the GPU in a throwaway container of llama.cpp's
pinned image (`llama-server --list-devices`), and for each app container its own cgroup and every online core. After a
power mode change, from a 4-core mode up to MAXN, it shows whether containers got the cores back.

Proven on patai-dev with release 2026.11.19-4 (2026-11-19): it booted in 85 s and the slot was marked good, every
check above passed, Gemma 4 26B answered through Chat, and the console read each service's memory and GPU memory from
its scope. From MAXN to `MODE_30W_4CORE` containers had cores 0-3, and back at MAXN all nine had 0-7 again by
themselves.

## Building it

```bash
scripts/build-kernel [dir]        # the kernel alone, into out/kernel: Image, dtb/, the packages, config, SHA256SUMS
scripts/build-image               # the l4t image, with it in place of CTI's
```

Both run `docker/Dockerfile`'s `kernel` stage, in the Ubuntu 20.04 image, from `vendor/`: CTI's source
(`cti/cti-l4t-src-agx-35.6.2-v001.tgz`), NVIDIA's toolchain and CTI's BSP, whose packages it rebuilds. The kernel takes
about 40 minutes on 8 cores. The `l4t` stage puts it into `CTI-L4T/kernel` before CTI's install.sh runs, so a
factory flash, the kernel partitions and every slot's `/boot` and `/lib/modules` all have it.

**Reproducible.** The build's date is fixed to CTI's newest source file, its user and host to `patai`, its paths are
the container's, and there's no signing key, so the same inputs make the same bytes. Tier IV's camera drivers stamp
the time they're built, the one place in the tree that does, so the build gives them its date. `SHA256SUMS` lists what
it made, and two builds are compared by it: one in the stage and one in another 20.04 container, an hour apart, made
the same files. Debug info isn't kept: a rebuild makes the same kernel, so its `/build/obj/vmlinux`, there until the
build's last line, matches a node's, for reading a crash.

**A change** to `docker/kernel` means one more `kernel_build` in `scripts/lib.sh`, which is in the l4t image's tag, so a
tag always means one kernel and an image built before the change isn't used by mistake.

**The source** is GPL: the kernel, CTI's drivers (each says `MODULE_LICENSE("GPL")`), NVIDIA's (GPL, or MIT and GPL),
and device trees. Images built from it are distributed, so the source is a release file of this repo, `sources`,
beside them, and `scripts/fetch-vendor` gets it from there. It's NVIDIA's public R35.6.2 kernel source with CTI's
drivers, device trees, camera drivers and fixes added.

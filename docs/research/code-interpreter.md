# The agentic API's code interpreter

Researched 2026-11-19, to build it into the node now that a node boots cgroup v2 (`kernel.md`). Sources: agentic-api's
source at v0.10.0 (`docs/design/embedded-code-interpreter.md`, `crates/agentic-server-core/src/tool/code_interpreter`),
its `main` up to e8ce5ef (nothing changed there since), Eryx 0.8.0 and `eryx-precompile` 0.8.0 from crates.io,
Wasmtime's security advisories, moby's source, and patai-dev on release 2026.11.19-4. What was built is in `apps.md`,
The code interpreter.

## In short

agentic-api runs a model's Python in **Eryx**: CPython compiled to a WebAssembly component and run by **Wasmtime**,
ahead-of-time compiled with Cranelift. WebAssembly is the sandbox. A guest has no system calls, only what the host
gives it, and agentic-api gives it the standard library to read and two output streams. No network, no files, no
processes, no host calls.

cgroup v2 isn't the sandbox; it bounds what a run takes. Each call runs in a **worker process** of its own, in a
**cgroup of its own** below the server's, with a memory limit, no swap and a process limit. The server makes these
cgroups itself, so it needs a cgroup v2 subtree delegated to its user.

It needs nothing else from the host: no Docker socket, no nested containers, no gVisor, no microVM, no KVM, and no
root. Wasmtime and Cranelift support aarch64 as a tier-1 target, and the runtime is compiled for the Xavier on the
PC. Three things had to change for the node:

- **Docker won't give a container a writable cgroup.** So a small runtime wrapper, `patai-runc`, does, for a container
  with no root and no capabilities.
- **Linux 5.10 has no `cgroup.kill`**, which agentic-api insists on. A patch kills a worker without it.
- **Wasmtime 48.0.2**, which v0.10.0 locks, has a critical aarch64 miscompilation advisory. The patch moves it to
  48.0.9.

## What a call does

A request declares `{"type": "code_interpreter", "container": {"type": "auto"}}`. That shape is the only one taken:
`file_ids`, `memory_limit`, explicit containers and unknown fields are refused. The model sees a function,
`code_interpreter(code)`, and must `print()` what it wants back.

For each call:

1. The server checks the source: UTF-8, at most `max_source_bytes`.
2. It takes a permit from one semaphore. If none is free, the call fails at once with a capacity error; it doesn't
   wait.
3. It makes `agentic-eryx-<uuid>` beside its own cgroup and writes the limits: `memory.max`, `memory.swap.max` 0,
   `memory.oom.group` 1, `pids.max` 64.
4. It starts `/proc/self/exe --agentic-code-interpreter-worker <socket>`, with the environment cleared but for
   `TMPDIR`, and stdin, stdout and stderr on `/dev/null`.
5. It moves the worker into the cgroup and checks the move took, and that the limits read back.
6. It accepts the worker's connection on a private Unix socket, and only from that worker's PID.
7. The worker checks its own limits, builds a fresh Eryx sandbox and runs the code once, with Eryx's wall time, fuel
   (Wasmtime's count of work done) and guest memory limits.
8. The output comes back over the socket, bounded. A deadline, a cancellation or the client going away kills the
   cgroup, and the permit is released only once the worker is reaped.

The answer has a `code_interpreter_call` item, whose `outputs` are `logs` items: stdout and stderr. Its `status` is
`completed`, `failed` (a Python exception) or `incomplete` (a limit). Streaming gives OpenAI's
`response.code_interpreter_call.*` events. There are no files or images, no sessions that last between calls, and no
packages beyond the standard library. Upstream lists each of these as not offered.

## What a guest can reach

- **Files.** Eryx preopens one directory: the extracted standard library, read-only, as `/python-stdlib`. Nothing is
  writable. The `vfs` feature, with an in-memory `/data`, isn't built into agentic-api. Writing a file fails.
- **Network.** Eryx's sockets are host functions of its own, and refuse unless the sandbox has a `NetConfig`.
  agentic-api never gives one. Wasmtime's own WASI sockets check each address against a rule that denies all by
  default. A guest has no network at all, not even loopback.
- **Processes, environment, host.** WASI has no `fork` or `exec`. The environment is `PYTHONHOME` and `PYTHONPATH`.
  There are no callbacks, so no host function but output.
- **Python's own modules** that need the OS, such as `subprocess`, `socket`, `ctypes` and `ssl`, are in the library
  but fail when used.

## Why cgroup v2

The worker limits are cgroup v2 files: `memory.max` with `memory.swap.max` 0, `memory.oom.group`, `pids.max`, and the
process listing used to kill one. A server not running as root can make and limit such cgroups only below a subtree
that's been delegated to its user.

cgroup v1 has no such delegation, and no `memory.swap.max`. The server refuses to start without `0::` in
`/proc/self/cgroup`.

At startup the server finds its cgroup's parent. It wants the parent delegated with `memory` and `pids` on for
children, not the hierarchy's root, not a systemd slice, and itself in a leaf below it. Outside a container it can get
that by re-running itself under `systemd-run --user --scope -p Delegate=yes`. A container has neither a user manager
nor a writable cgroup.

## Docker and the node

- **A container's `/sys/fs/cgroup` is read-only.** moby's default spec mounts it `ro`, and clears that only for a
  privileged container or one with a user namespace remapped. There's no option for it. Kubernetes proposes one
  (KEP-5474, writable cgroups), and says a runtime must not offer it where it can't keep the container in its own
  cgroup.
- **The node's cgroup2 has no `nsdelegate`** (`findmnt /sys/fs/cgroup`: `rw,nosuid,nodev,noexec,relatime`). With it,
  the kernel would refuse a write from inside a cgroup namespace to the resource files at its top. Without it, only
  file ownership protects them: they're root's, mode 0644. So nothing in a container with a writable cgroup may be root.
- **No AppArmor.** Docker's security options are seccomp and cgroupns only.
- **5.10.216** has `cgroup.freeze` and `memory.oom.group` but not `cgroup.kill` (5.14) or `memory.peak` (5.19).
- **Docker 26.1.3, runc 1.1.12, systemd driver.** runc makes each container a `docker-<id>.scope` with `Delegate=yes`,
  so systemd leaves the cgroups below it alone. A `docker exec` into a cgroup with children joins the first process's
  cgroup instead (runc falls back on `EBUSY`).

The ways to get the server a delegated subtree in its container:

| Way | Why not, or why |
| --- | --- |
| `privileged: true`, or `CAP_SYS_ADMIN` to remount `/sys/fs/cgroup` read-write at start | Root, or a capability that also opens `mount` and namespaces in Docker's seccomp profile for every process in the container |
| `cgroup: host`, with the container's slice bind-mounted read-write and chowned by `patai-workload` after start | Every container's cgroups show; the chown races the server's startup; the container's own scope is left empty |
| **A runtime wrapper**, for this container only, that makes its cgroup namespace's mount read-write and delegates a part of it | Chosen: `patai-runc`, Docker's runtime `patai-delegate` (below) |

## The arm64 build

- **Eryx's runtime** is `runtime.wasm`, 11.7 MB, from Eryx's GitHub release `eryx-v0.8.0`. It's the same for every
  platform and is pinned by its SHA-256. It's compiled to `runtime.cwasm` for a target, and Eryx's build embeds that
  in the server binary.
- **Eryx's precompiler** can cross-compile only if Cranelift has every backend, which Eryx doesn't turn on. It also
  locks its own Wasmtime. So the patch adds `patai-eryx-precompile`, in agentic-api's workspace, with Wasmtime's
  `all-arch`. The runtime is then compiled by the same Eryx and Wasmtime as the server that runs it.
- **The target** is `aarch64-unknown-linux-gnu` with Cranelift's baseline settings. A runtime compiled for the
  build machine's CPU, or for QEMU's, would use features Carmel lacks, and Wasmtime would refuse to load it.
- **The server** is cross-compiled on the PC, against Debian's arm64 OpenSSL. It takes minutes, not half an hour of a
  node's cores and memory.

## Wasmtime's advisories

v0.10.0 locks Wasmtime 48.0.2. Advisories fixed since then in 48.x:

- **GHSA-mghf-wjfp-p5xm, critical:** Cranelift on aarch64 could jump to the wrong target on a large `br_table`. Fixed
  in 48.0.7.
- **GHSA-3982-jqvv-277m, GHSA-qw58-7h65-crq2, GHSA-2674-w29h-wqvj and GHSA-xrvr-8678-5wp8** are about GC types,
  builds without `threads` and component streams.

The patch locks the Wasmtime and Cranelift family at 48.0.9 (0.135.9), and wasm-tools 0.254.2, and nothing else.
Wasmtime 50 is out, but Eryx 0.8 wants 48. Eryx itself is at 0.15.2, which agentic-api doesn't take yet.

## If a guest escaped

A bug in Wasmtime or Cranelift that let guest code out of WebAssembly would put it in the worker process: the
server's user, 10001, in the agentic-api container, with no capabilities.

What it couldn't reach:

- the host's files, `/persist`, the rest of `/data`, the Docker socket (never mounted), or another container's
  loopback;
- root, as nothing in the container is root, `no-new-privileges` is set, and there are no setuid files on a
  read-only root;
- its own limits, which are root's.

What it could reach is what agentic-api can:

- its conversations in `/data/agentic-api`;
- `llama-cpp` and SearXNG on `patai-ai`, and the internet;
- the gateway for apps on 18000, which takes only an app's key, and agentic-api has none.

It could also replace the unpacked runtime in `/tmp` for later workers, until the container restarts.

A worker in an empty network namespace would take the network away from such an escape. It would need user namespaces
allowed in the container's seccomp profile, which gives escaped code a kernel surface it lacks now (`nf_tables` and
the like, in a namespace of its own). The bounds above seemed the better trade (Decisions for Patrick in `apps.md`).

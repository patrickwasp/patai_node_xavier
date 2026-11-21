# Console

The node's own console and API. It's complete on a LAN with no internet; the cloud is only ever an optional client of the same API.

## What runs on the unit

All native, in the signed A/B rootfs. This is how a unit is managed, so it falls under the split rule in
`architecture.md`: the console, login and OS updates stay up without the NVMe, the internet or HQ. If
Docker is down, only workload operations stop, and they report that.

| Process | Runs as | Job |
| --- | --- | --- |
| `patai-api` | Its own account, no sudo, not in `docker`, which is root in all but name | HTTPS, login, permissions, audit, jobs, the UI, the workload gateway |
| `patai-rootd` | root, on a Unix socket only | Fixed verbs with checked arguments: reboot, install a bundle, start or stop a pinned stack, power mode, network. No commands, paths or Compose input |
| `patai-report` | root, from a timer | `/run/patai/report.json` every minute |
| `patai-screen` | As now | Reads a status document instead of asking Docker |
| `patai-link` | Its own account, off until linked | The cloud connection. No access to `patai-rootd` |

**Go** for the daemons: no runtime to carry on a frozen Python 3.8, one static file, TLS in the standard
library, and a much smaller tree than Node's on a process this close to root. **The UI** is the usual stack,
TypeScript, Next.js and shadcn/ui, as a static export embedded in the binary. There's no Node on the unit
and nothing loads from outside it, so it works offline. All of it ships in the image, so nothing installs
at runtime. Its words are short and clinical: labels, states and a line where one is needed, and how things work
stays in these docs. Lists of records (people, tokens, keys, jobs, installs, usage, requests, the audit log) are
TanStack Table data tables: a filter, a menu of values for columns like role or state, sorting, pages, and a choice of
columns that each browser keeps for itself. The Status page has a banner only when something needs attention. Each
card on it links to where its facts are managed, and its Load card reads CPU, GPU and memory every 3 s through `patai-rootd`'s `resources`, which needs root for nvmap's
counts. Memory in use there leaves out nvmap's pool, as `apps.md` measures it.

These services get CPU weight, OOM protection and memory the kernel doesn't take back (Memory, below), because
workloads share 32 GB with everything else and the console has to answer when the unit is struggling.

## Memory

cgroup v2's memory protection (`kernel.md`) keeps what the console needs in memory however hard the rest of the unit
is pressed, what Docker's daemons need, since every request to a service goes through them, and the other two ways to
reach the unit, SSH and mDNS. Measured on patai-dev:

| Unit | What it holds | Measured | Given |
| --- | --- | --- | --- |
| `patai-api` | The console, its pages, the gateway | 30 MiB at rest, 34 serving the UI, Status page and Chat's answers at once, 150 to 205 while people log in | `MemoryMin=192M` |
| `patai-rootd` | Root's verbs, and the tools it runs for the console's pages: `docker`, Python for `patai-workload`, `rauc` | 18 at rest, 50 to 127 while the Status page and Load card ask | `MemoryMin=192M` |
| `docker.service` | dockerd, and `docker-proxy`, which carries every request from the gateway to a service's loopback port, and every container's output | 115 to 165 | `MemoryLow=256M` |
| `containerd.service` | containerd, and each container's shim, which relays its output to dockerd | 90 to 125 | `MemoryLow=192M` |
| `ssh.service` | sshd's listener, and each connection until it has logged in | 5 at rest, 16 with 12 connections opening at once | `MemoryMin=32M` |
| `systemd-resolved` | mDNS, which answers for `<name>.local` and the Bonjour service, and every name the unit looks up | 10 at rest, 11 answering mDNS and lookups | `MemoryMin=32M` |

- **A login is 64 MiB.** A password check is Argon2id with 64 MiB, one runs as the console starts, for the hash an
  unknown name is checked against, and Go keeps the heap for minutes after. So the console's own 30 to 35 MiB is 150 to
  200 while people log in, and an owner logging in to a unit that's struggling needs it most.
- **`MemoryMin=` for the console, SSH and mDNS:** what's under it is never taken back, even if that means the OOM
  killer, which leaves the console alone anyway. SSH is the way in when the console can't help, and mDNS how a device
  finds the unit; both are small and steady, so a hard floor costs 64 MiB at most. **`MemoryLow=` for Docker's,** which
  is bigger and changes more: taken back only once nothing unprotected is left. Within a group the kernel still takes
  the coldest pages first, so a group over its amount loses what it used least.
- **`system.slice` carries the sums,** 448M of each (`system.slice.d/50-memory.conf`). A group is protected only as far
  as its parent's protection covers it, and focal's systemd mounts cgroup2 without `memory_recursiveprot`, so a
  service's amount does nothing without the slice's, and the rest of the slice gets nothing from it. Change both
  together.
- **CPU memory only.** CUDA memory is nvmap's, outside every cgroup, and can't be taken back or swapped, and freed GPU
  memory stays in nvmap's pool, up to 3.8 GiB (`apps.md`). A reservation keeps the console's pages, but it
  can't make room the GPU took, and new memory the console asks for waits on reclaim like anyone's. Admission keeps
  the GPU's use in bounds (`apps.md`, Memory).
- **Chat has none.** Its page and chat list take 0.5 s at the 95th percentile with no pressure at all, its own doing,
  and under pressure what moved that was protecting the console and Docker, which carry its requests. Protecting its
  three containers as well kept their pages but didn't change its time, and its answers, the GPU's work, took 15 to
  18 s in every run (Measured).
- **Not protected:** `patai-report`, journald, systemd itself, and what a login over SSH runs, in `user.slice`. At the
  bottom of memory `patai-report` stopped, and so did SSH and mDNS, before they had protection, and `patai-rootd`,
  protected but hung in the kernel; the console didn't (Measured).

### Measured

**Under pressure, 2026-11-19,** on patai-dev at MAXN, freshly booted, with Gemma 4 26B and the nine services of Chat
and the AI API running, 90 s for each. The pressure: every llama.cpp model file read through the page cache over and
over, as loading models does; for "tight", also a CUDA program holding 6.75 to 7 GiB of GPU memory, to about 1 GiB
left. Meanwhile, from a PC on the LAN: the console's UI, `/api/identity`, `/api/v1/status` and `/api/v1/resources`
twice a second each, a login every 10 s, Chat's page and chat list twice a second, and Chat writing three answers.
What the kernel took from each group in those 90 s, in 4 KiB pages, and how long its tasks stalled on memory:

| | 2026.11.19-4, churn | 2026.11.19-mem1, churn | 2026.11.19-4, tight | 2026.11.19-mem1, tight, two runs |
| --- | --- | --- | --- | --- |
| Lowest MemAvailable | 7.6 GiB | 7.1 GiB | 720 MiB | 859 and 680 MiB |
| `patai-api` | 1,867 pages, 48 ms | 0, 0 ms | 5,682, 29 ms | 0, 0 ms |
| `patai-rootd` | 16,898, 134 ms | 0, 8 ms | 67,599, 210 ms | 0, 2 and 30 ms |
| `docker.service` | 14,029, 19 ms | 0, 0 ms | 59,726, 117 ms | 0, 0 and 3 ms |
| `containerd.service` | 14,749, 8 ms | 0, 0 ms | 60,698, 135 ms | 0, 0 and 5 ms |
| The console's calls: median, 95%, slowest | 15 to 16, 78 to 94, 187 ms (two of the four) | 14 to 17, 43 to 48, 320 ms | 15 to 17, 48 to 56, 359 ms | 13 to 17, 35 to 52, 149 ms |
| A login: median, slowest | 170, 322 ms | 172, 445 ms | 189, 276 ms | 155 to 160, 211 ms |
| Chat's page and list: 95%, slowest | 570 to 592, 770 ms | 503 to 547, 593 ms | 528 to 568, 1,273 ms | 519 to 554, 654 ms |
| Chat's answers | 15.6 to 16.1 s | 14.0 to 17.6 s | 15.8 to 16.9 s | 15.6 to 16.5 s |

Every call measured answered. What the console lost came back as it was asked for: nearly every page taken was read
again, so the kernel was cycling its working set, not freeing anything it didn't need. On a unit that had been pressed
for an hour it was more: in one tight run `patai-api` lost 41,168 pages and stalled 413 ms, `patai-rootd` 209,000 and
506 ms, `docker.service` 359,612 and 1,154 ms. With protection set by hand there, less than now for the console, they
lost none. The busiest the console got was an update, at 562 MiB, 476 of them the bundle's page cache.

**Chat, the same day,** in tight runs one after another on that pressed unit, and with no pressure:

| Protected | Chat's page and list: 95%, slowest | Its three containers' stalls in 90 s |
| --- | --- | --- |
| Nothing | 802 to 916 ms, 1,333 | 127 ms |
| The console | 570 to 608 ms, 749 | 76 ms |
| The console and Docker | 574 to 610 ms, 769 | 186 ms |
| The console, Docker, and Chat's server, Postgres and pages with 256, 128 and 64 MiB | 525 to 593 ms, 698 | 16 ms |
| No pressure at all, nothing protected | 491 to 550 ms, 557 | |

**SSH and mDNS, the same day,** in tight runs one after another, with the console and Docker protected and Chat
asked nothing, the protection put in as runtime drop-ins on a release without it. From the PC, over Wi-Fi: SSH's
banner every second, a login every 5 s, and an mDNS query for `patai-dev.local` sent to the node every 2 s, past
every cache:

| | SSH and mDNS unprotected, two runs | Protected, two runs | No pressure |
| --- | --- | --- | --- |
| Lowest MemAvailable | 1,107 and 972 MiB | 1,170 and 1,317 MiB | |
| `ssh.service` | 1,029 and 15,158 pages, 163 and 427 ms | 0, 31 and 22 ms | |
| `systemd-resolved` | 1,162 and 1,157 pages, down to 1.6 MiB | 0, 0 ms | |
| SSH's banner: median, slowest | 43 to 56, 155 ms | 34 to 35, 162 ms | 43, 74 ms |
| A login: median, slowest | 0.35 to 0.39, 0.50 s | 0.33 to 0.34, 0.41 s | |
| mDNS: median, unanswered | 132 to 143 ms, 1 of 45 and 3 of 44 | 140 to 149 ms, 2 and 2 of 44 | 119 ms, 2 of 44 |

That far down both answered either way, about as fast as with no pressure. What changed is that the kernel stopped
cycling their pages: 15,158 taken from 8 MiB is the same pages taken and read again. mDNS misses as many queries with
no pressure, so those are the network's. Whether protection keeps them answering at the bottom of memory isn't
measured: a test there hangs the unit.

**The bottom of memory.** One run went on after its test lost the way to the unit, with GPU memory held, every model
file being read and swap filling, until MemAvailable was 217 MiB, and the kernel hung (`hardware.md`, Huge pages). The
console had less protection then than now, set by hand. For the 16 minutes until a power cut it answered every call by
its address but one that needed `patai-rootd`, at first in about 20 ms at the median, later in 40 to 600 ms with some
taking 9 s. Chat on 443 didn't: its TLS connections timed out from about when the kernel hung, though its containers
were protected then, so something on that path waited on what hung. SSH couldn't finish its banner,
`patai-dev.local` stopped resolving, `patai-report` stopped, and `patai-rootd` hung in the kernel, so the console
couldn't reboot the unit. A reservation can't keep a node off that
floor; admission's margin does (`apps.md`, Memory).

## API

JSON REST under `/api/v1`, described by OpenAPI, with a generated TypeScript client and server-sent events
for progress and log tails. Management is kept apart from the workload API at `/v1`.

- **Jobs.** Work that takes time returns `202` and a job id, recording actor, phase, progress and result.
  Changes take an idempotency key. Installing, switching slots and rebooting are serialised. After a crash
  or reboot, jobs are reconciled against RAUC, the boot id and Docker. A reboot job finishes when that boot
  is seen.
- **Versions.** `GET /api/v1/capabilities` gives the API version, state format, release and features. v1
  only ever gains things. The API ships in the signed OS, so the cloud has to cope with every release in
  the field: it hides what a unit lacks. A small identity document outside the versioned API (name, release,
  API versions) never changes, so a cloud can still list a unit on a major version it doesn't know.

## Claiming and logging in

- **Claim.** A new unit accepts nothing but a claim, which needs a one-time code. The code is on the HDMI
  screen until claim, in the QR code as a URL fragment, and on a handoff card from `scripts/new-unit` for a
  unit with no display. It proves someone can see the unit or holds its card, and it creates the first
  owner in the same transaction that uses it up. An IP address or a serial proves nothing.
- **Roles.** Owner, operator, viewer. Only an owner manages users, tokens, SSH keys, network, the update
  channel and the cloud. Web users aren't Unix users. Argon2id, HttpOnly cookies, CSRF and Host checks
  against DNS rebinding, throttled login, and a fresh password for sensitive actions.
- **API tokens.** Random, shown once, stored hashed, scoped, with an expiry.
- **A forgotten password.** Recovery codes issued at claim. Failing those, `patai-console reset-owner` over
  SSH with a provisioned key or on the P4 serial console. Failing those, a reflash. After claim the
  screen never shows a secret. There's no master password, so nobody else can get into an owner's node.

## The front page

What the unit's address, `https://<name>.local/` on 443, opens. It's the console's until an owner puts an app with
pages of its own in front, such as Chat, which everyone in the house uses every day: **Open at https://<name>.local/**
on its card, on the Apps page. The console is then at `https://<name>.local:9443/`, where it always is too, so an owner
always has one address for it, and Chat's people never meet its login. The page on port 80 links to the app first, and
to the console's port.

- **What stays the console's** at the address, whatever is in front: the AI API for keys, `/v1/`, so programs keep one
  address, and `/healthz`.
- **An app in front that isn't on** leaves the console at the address, so the owner can turn it on again from there.
- **Not a path** such as `/admin` on 443: the console and the app would share an origin, and a flaw in the app could
  reach the owner's session. On their own ports they're two origins, and the console's session cookie is taken off
  every request to an app's pages anyway.
- **Kept** in `/persist/console/front`, written durably, and read at start. Choosing it is an owner's, and audited.
- The app's pages stay on their own port too, such as Chat's 8443.
- Scripts that call the console's API use its port, `https://<name>.local:9443/api/v1/…`, which works whatever is in
  front.

## HTTPS on a LAN

One self-signed certificate per unit, made once at provisioning or first boot, and kept when the address
changes, so its fingerprint stays the one on the HDMI screen and the card. Its validity is fixed, 2025 to
2055, so making it doesn't depend on the unit's clock. Browsers warn at each new address, and the
fingerprint is there to check before typing a password. Port 80 serves a page that explains this and
nothing else. Plain HTTP on a port that serves HTTPS, as a browser sends for an address with a port and no `https://`,
such as `patai.local:9443`, is redirected to the same address over https, by the connection's first byte
(`upgrade.go`). A public certificate can be an option once linked, but it can never be the only way in, because it
needs DNS and internet.

**An owner's own certificate.** An owner can add a certificate for a name that points at the unit, from their own CA
or a public one, on the System page, with a fresh password. Browsers that trust its issuer don't warn at that name.
It's used only at its own names, chosen by the name the browser asks for (SNI), on 443, on the console's 9443 and on
each app's own port, such as Chat's 8443. The unit's addresses, `<name>.local`, any other name and a client that sends none always get the
unit's own certificate, so its fingerprint on the screen and the card stays right, and an owner's certificate that runs
out or goes wrong can't lock anyone out.

- **What's checked.** The page sends the certificate, with what issued it, and the key, as PEM files. The unit says
  what they'd do before anything changes: where they'd be used, the issuer, the dates and the fingerprint. It refuses
  a key that isn't the certificate's, one with a passphrase, anything but RSA of 2048 to 4096 bits or ECDSA on P-256
  or P-384, a CA's certificate first, one not for TLS servers, and a chain that doesn't verify, including name
  constraints. It refuses a wildcard, because the eMMC isn't encrypted and a stolen unit would give up a key for every
  name under it. Addresses, `.local` names and single labels in a certificate are listed as unused. A trailing root
  isn't served. Nothing can check that a browser trusts the issuer, that DNS points here, or that the key isn't used
  elsewhere.
- **Dates.** They're checked by the unit's clock only when a time server keeps it or an owner set it this boot.
  Otherwise the page shows them and says so, because a unit back from a power cut with no battery could refuse a good
  certificate. The page warns 30 days before it runs out, by the browser's clock, which is the one a browser checks it
  with. The unit doesn't renew it.
- **Kept** in `/persist/console/owner-tls.pem` with its key, 0600, written durably, and checked again at every start.
  One that won't load leaves the unit's own certificate everywhere, and the page says why. Replacing or removing it
  takes effect at the next connection, and sessions from before can't be resumed.
- **The console answers to its names**, as well as the unit's own, so the DNS-rebinding check holds: only the owner's
  CA could name them. An origin must now be HTTPS. Each name has its own login. HSTS is off, `max-age=0`, so that a
  browser forgets any it kept: with HSTS, a certificate that ran out would lock its name out with an error nobody can
  click through.
- The port 80 page lists the names. The screen and the card don't change. A rename leaves the certificate's names as
  they are. A factory reset removes it with the rest of `/persist/console`, and recovery mode doesn't load it.

Left out: making a key or a signing request on the unit, renewing, more than one certificate, IP addresses and
wildcards, and changing it from the cloud.

Not used:

- **A CA on each unit, installed on owners' devices.** The eMMC is unencrypted, so a stolen unit's
  key could impersonate any site to every device that trusted it.
- **Plain HTTP with a password protocol in the page.** Someone on the LAN could replace the page.
- **A shared wildcard certificate on a private-address DNS name.** The key would be on every unit, and it
  fails offline.

## Finding a unit

DHCP and `<name>.local` stay on both ports. The unit also publishes itself as a Bonjour service, `_patai._tcp` on 443
(`/etc/systemd/dnssd/patai.dnssd`), so Chat's native apps list the nodes on their network (patai_chat `docs/clients.md`). Add IPv4 link-local addressing, so a laptop cabled straight to a
unit reaches `https://<name>.local/` with no DHCP on either side; the name is on the card. There's no DHCP
server and no service port, because a unit plugged into the wrong network must not hand out addresses.
Wi-Fi is untested on this board, so there's no access-point or phone-only setup.

A network change from the console has to be kept within 5 minutes, or it goes back, including across a restart
(Network settings, below).

## Network settings

The System page shows each port by its label on the board, GbE 1 (J1A) and GbE 2 (J1B), with its cable, addresses,
gateway and DNS, from `/api/v1/network`. An owner can set one port at a time to DHCP or to a static IPv4 address with
an optional gateway, and either way give up to three DNS servers. That's all v1 has: no VLANs, bonds, routes, MTU or
proxy, IPv6 stays automatic, and a port can't be turned off. It takes a fresh password, and never a token or the cloud.

`patai-network` does the work as root. It checks the setting: a unicast address with a prefix of 8 to 30, a gateway on
that network, and no overlap with Docker's networks or the other port's, because replies would leave by the wrong one.
It writes it into `/run/systemd/network` ahead of `20-wired.network`, matched by where the port is on the board (udev's
`ID_PATH`), so it means the same on every Rogue whatever Linux calls the port. mDNS and link-local stay on whatever is
set, so a laptop cabled straight to a port still finds `<name>.local`. A static gateway gets DHCP's route metric, so a
port isn't preferred for being static. networkd 245 switches cleanly with `reload` and `reconfigure`, and
an address it leaves behind is taken away regardless.

**A change is a trial.** It's made 2 s after the request, so the answer gets back first, and goes back after 5 minutes
unless the owner keeps it. The deadline is a systemd timer, so it holds whether or not the console and `patai-rootd`
stay up, and a lock makes the timer and the owner take turns. The timer counts from boot, because a daemon-reload
starts an `--on-active` timer's count again in systemd 245. What's saved is on `/persist` and the trial only in
`/run`, so a restart or a power cut during a trial is the same as letting it run out. A trial won't start during an
update, a restart or a reset.

The way back takes nothing away until a static address it restores is there, and tries again until it is, so it
can't leave a port with neither. Going back to DHCP doesn't wait for a lease: that's how the port was. A trial on
its way back can't be kept.

Every page shows the trial to an owner, with Keep and Undo. When the address changes, the page that made the change
loses the unit, and says where it is now and what the certificate's fingerprint is: a browser warns again at each
address, and a session belongs to an address, so the owner logs in again there. Keeping is accepted from any address
that reaches the unit, because a second port set for a network the owner's laptop isn't on could otherwise never be
kept. The page says when that's what's happening. Five minutes leaves time for a certificate warning, a new login and
finding what DHCP gave.

**Getting back in** after keeping a setting that's wrong: the other port; a laptop cabled straight to the port, by
`<name>.local` or the link-local address; `sudo patai-network reset` over SSH or the serial console, which puts both
ports back on DHCP; a factory reset, which forgets the settings with the rest of `/persist/provision`; a reflash.

A rollback to a release older than 2026.11.02-1 leaves both ports on DHCP, because it doesn't read the settings. The
file only gains things: a port a release doesn't know is left in it for the release that wrote it.

## Remote access with Tailscale

An owner's choice, off in every image (Patrick, 2026-11-18; the research is `research/tailscale.md`). Turned on, the
unit joins the owner's own tailnet, and the owner reaches Chat at `https://<unit>.<tailnet>.ts.net` and the console at
the same name on 9443, from their own devices anywhere, with no port forwarded and no Wasupu relay. Wasupu has no
tailnet and is on none of the owners'; ops `decisions/0003` still rules out a Wasupu VPN to units.

- **On the System page**, Remote access, for owners: Turn on, then Log in, which shows the login link and a QR code of
  it for a phone, or an auth key from the owner's tailnet, used once and never kept. A control server of their own,
  such as Headscale's, is under More. Running, the card shows the tailnet, the unit's tailnet name with links to Chat
  and the console, its addresses, when its key runs out (warned of 30 days ahead), Tailscale's health messages, and
  Settings, Log out and Turn off. A device waiting for the tailnet's approval says so. Each change takes a fresh
  password, never a token or the cloud, and is audited, an auth key never.
- **The certificate.** With MagicDNS and HTTPS on for the tailnet, Settings can use a certificate from Let's Encrypt
  through Tailscale at the unit's tailnet name, whose key stays on the unit. It joins the certificate selector beside an
  owner's own, so the console and Chat answer to that name with no warning; the name goes into public Certificate
  Transparency logs, and the setting says so. Without it, the tailnet address works with the unit's own certificate,
  as any address does, and the tailnet name isn't answered.
- **Logs** stay on the unit (`--no-logs-no-support`) unless Settings sends them to Tailscale, for its support or a
  tailnet that requires network logs, which disconnects a node that won't send them.
- **How it runs.** `tailscaled` is built from Tailscale's tagged source (`docker/tailscale.Dockerfile`, pinned in
  `vendor/MANIFEST`), stripped, with its CLI inside, and without its updater, web client, SSH, Taildrop or Drive: it
  comes with each OS release, never updates itself, and each release should move to Tailscale's newest stable. Its unit
  is enabled in every image and starts only while `/persist/tailscale/on` is there, in kernel (TUN) mode: in userspace
  mode it would hand tailnet connections to 127.0.0.1, where the workloads listen. Its state, the node key, prefs and
  certificate, is in `/persist/tailscale/state`, so updates and rollbacks keep the login, and `patai-rootd` syncs it
  once a login finishes, as tailscaled doesn't sync the directory. The console talks to it over its socket through its
  local API (`internal/tailnet`), as its operator, which `patai-rootd` makes it each time it starts it. A login takes
  the unit's own DNS and routes, not the tailnet's, and offers no exit node, subnet route, Funnel, Serve or SSH.
- **What the tailnet reaches** is what the LAN reaches: 80, 443, 8443, 9443 and SSH, guarded by their own logins and the
  owner's tailnet policy. tailscaled runs with `--netfilter-mode=nodivert`: by default it would jump to its own chain at
  the top of INPUT and accept anything from the tailnet ahead of `patai-firewall`, gateway for apps on 18000 included.
  So 18000 stays closed to it, the workloads stay on loopback, and a person's terminal can't reach `100.64.0.0/10`. A
  static address in that range is refused.
- **A factory reset** logs the unit out of its tailnet first, while it still can, and its next boot deletes
  `/persist/tailscale`.

## Clock

P7 takes an RTC battery, but units can't be assumed to have one, and without it a 30 s cut puts the
PMIC's RTC back to 2000-01-01. A site may have no NTP either. Deadlines use monotonic time. Audit and job records carry
the boot id and a sequence number beside the timestamp.

**After a power cut** the kernel sets the clock from that RTC, systemd moves it up to its own build date, and timesyncd
to the time it last saved, which it does on each sync and when it stops. NVIDIA's `nvrtc-sync-boot` then set it back to
2000 from the RTC, and a unit with no time server stayed there. `patai-clock boot` takes its place, through a drop-in.
It takes the RTC's time only if it's between this release's build time and 2055. Otherwise it puts the clock at the
later of those two saved times, if it's behind them or past 2055, and writes that to the RTC. With NTP off, a shutdown
and 30 s unplugged came back 94 s behind, rather than in 2000.

**An owner can set the clock from their browser,** on the System page, with a fresh password. The page shows the
unit's time against the browser's and what set it, and the dashboard points there when no time server has. The time
sent is the browser's, plus what the console spends on the request, which is mostly the password check: about
0.3 s. `patai-clock set` refuses a time before this release was built or after the certificate runs out
in 2055. It sets the clock and the battery RTC, and moves timesyncd's saved time to it, synced, so that a cut doesn't
take the clock further back than that and a setting back isn't undone at the next boot. A synchronised clock can't be
set by hand, and a time server takes over again once one answers. Setting the clock back would bring back API tokens
that had run out, so those are deleted first, and again by the time the helper set the clock from. Settings take
turns, so that one going forward can't come between another's look at the clock and its setting back.

A cloud link needs a good clock to check the server's certificate, so it waits for NTP or the owner, and the console
says which.

Left out: a saved time on `/persist` for both slots. After a rollback the other slot's own saved time comes back, and
the owner sets the clock again if that's wrong. The unit keeps UTC, and the page shows times in the browser's zone.

## Name

The name is `<name>.local` on both ports, what DHCP is told, the console's DNS-rebinding check, the status screen, and
what a factory reset asks to be typed. An owner can change it on the System page, with a fresh password: one DNS label
of lower-case letters, digits and hyphens, starting with a letter. It goes into `/persist/provision/hostname`,
durably, and the unit restarts, because `patai-identity` gives the unit its name early in boot, so everything that
uses it changes at once. A rename while running would leave the console, mDNS and DHCP disagreeing for a while. It's a
job like Restart and is refused during an update, and the next boot finishes the job if the name took.

The certificate stays, so its fingerprint is still the one on the card and the screen, and the browser warns about the
name. A page that reached the unit by its old name can't follow it, so it gives `https://<new>.local/` and the
fingerprint; one on an address follows it to the login. Older releases read the same file, so a rollback keeps the name.
A factory reset removes it, and the unit is `xavier-<serial>` again.

## Power mode

The System page lists the nvpmodel modes in this release's `nvpmodel.conf` with their cores and limits, and an owner can
change the mode, with a fresh password. It changes at once without stopping anything, and containers get the cores a
mode brings online by themselves (`kernel.md`). `patai-power-mode` runs nvpmodel, checks the mode took, and only then
keeps it in `/persist/provision/power-mode`. A service after `nvpmodel.service` and before
Docker puts it back at every boot. nvpmodel's own record is on the slot, so without this an update would come up in its
default. `sudo patai-power-mode <id>` keeps it the same way.

No mode kept, or one this release doesn't have, means Maximum (MAXN), not nvpmodel's 15 W default (Patrick,
2026-11-14). It reads a prompt about twice as fast, which a chat feels before its first word: Chat's first answer took 47 s
in the 15 W mode and 30 s at MAXN. A node comes with a supply of 15 V or more, which MAXN needs under load: it resets
the unit under load on a 12 V supply (`hardware.md`). The unit can't measure its supply, so going back to MAXN from
another mode, the page asks the owner to say it's 15 V or more, and the API won't take MAXN without that. A rollback to an older
release uses that slot's own nvpmodel record.

## Shutting down

This board can't stay off while it has power: `poweroff` starts it again at once, and the watchdog starts a halted unit
again about 150 s after it halts (`hardware.md`). So Shut down on the System page means stopping so that it can be
unplugged. It's for an operator or owner who is logged in, not a token, because it takes someone beside the unit, and
it's refused during an update.

The page has to say when it's safe to unplug, and the unit can't tell it then, because its console has stopped. What the
page can see on port 443 does tell it. The console answers while workloads stop, because it's ordered before Docker and
so stops after it. Then connections are refused at once, while the kernel unmounts and syncs. Then nothing answers at
all, which comes at the same moment as `reboot: System halted` on the serial console. So the page, looking twice a second, says to unplug once two silences in a row follow two refusals
since the console last answered, with a deadline 120 s after the first silence. One lost packet can't say it, and a
refusal from before the console stopped doesn't count. If it only ever saw silence, as it would on losing the network,
it says it can't tell and asks for 30 s. About 40 s after a halt the laptop forgets the unit's MAC and connections fail
fast again, so after silence only an answer counts. What's left is a network that drops for seconds just as the kernel
finishes, up to about 3 s of refusals, and a power cut then does no harm anyway.

The next boot finishes the job: "stopped, and has started again", because a halt and a power cut look the same from
there.

## Diagnostics

**The Logs page** is for operators and owners. It shows this slot's journal by source, for this boot or an earlier one,
everything or only warnings and errors, and can follow it live. Each slot keeps its own journal, so the boots listed are
this slot's. Search runs in the browser over what's loaded. Workload output is what the running workload printed since it
was started: stopping it removes its container, and so does keeping a disk at claim, so that output never reaches another
owner. A viewer gets none of this.

| Source | What | Who | In a bundle |
| --- | --- | --- | --- |
| Everything | The whole journal, less sign-ins | Operator | Yes |
| Kernel | Kernel messages | Operator | Yes |
| Console | `patai-api`, `patai-rootd`, the report, the screen, the identity, factory reset | Operator | Yes |
| Updates | `patai-update` and its installs, RAUC, the rollback report, NVIDIA's boot services | Operator | Yes |
| Workloads | `patai-workload`, downloads, the data disk and its gate, Docker, containerd | Operator | Yes |
| Network | networkd, `patai-network`, the firewall, resolved, timesyncd, the clock | Operator | Yes |
| Power | `patai-power-mode`, nvpmodel, the fan, the thermal log | Operator | Yes |
| Sign-ins | sshd, sudo, logind, and systemd's lines about login sessions and user managers | Owner | Never |
| Workload output | The running workload's container | Operator | Never |

**How the console reads them.** `patai-api` stays out of the `systemd-journal` and `docker` groups: either would hand
it everything, and `docker` is root. `patai-rootd` has two verbs, `logs` and `workload-logs`. They take a source from the
list above, a boot from `journalctl`'s own list, a count up to 2,000 and a cursor whose shape is checked. `logs` runs
`journalctl` with fixed arguments at low CPU and I/O priority, for 10 s at most. Both read at most 4 MiB, and cut each
message at 4 KiB. Sign-in lines are dropped from every source but their own, by unit, by program and by session, so the
sign-ins themselves, and everything, are picked out of the whole journal. Credentials are taken out before anything
leaves the helper.

`workload-logs` asks Docker nothing. It finds the workload's container by its Compose labels in Docker's records on the
data disk and reads Docker's own file for it, back from the end: the local log driver keeps each line with its length
before and after it. Docker 26 writes a warning to the journal for every request for a container's logs, so following
through Docker filled the journal at a line a second, and a request could start Docker again after someone stopped it.

**Credentials.** Nothing of ours logs one: the claim code, password and recovery-code hashes, tokens and both TLS keys
live in files and the database, and the channel refuses a URL with a password in it. The helper still takes out the
current claim code and anything shaped like one, `patai_` tokens, Argon2 hashes, PEM private keys, a password in a URL,
`Bearer` values and `password=`-style settings, for what someone else's program prints. sshd logs whatever was typed as
a username, which is sometimes a password typed one prompt early. That's why sign-ins are an owner's, and never in a bundle.

**Following** is a server-sent event stream for one source. Each second the console asks the helper for what's come since
its last cursor, and sends each line as an event whose id is that cursor, so a reconnect with `Last-Event-ID` carries on
where it stopped. Workload output's cursor is Docker's time for the line, to the nanosecond, and how many lines at that
time have been sent, because stdout and stderr can share one. More than 500 lines in a second are caught up over the
next seconds, not skipped. Output more than 4 MiB behind its cursor can't be reached back to, and the page says lines
were missed. A comment every 15 s keeps proxies from closing it. A unit takes 4 streams and a session 2, each for 30
minutes, then an end event says so. The login is checked every second, and again before each batch is sent, without
counting as activity, so a logout or a removed user ends it. The page stops following while it's hidden. A browser
that reconnects after a restart, with the last boot's cursor, gets the new boot from its first line.

**A support bundle** is for an owner, logged in. It's a ZIP file, because every computer opens one, so the owner can read
it before sending it to anyone. The console makes it in memory, at most 8 MiB, one at a time, and hands it straight to
the browser, so nothing is written to the eMMC or kept on the unit. The audit log records each one.

- **In it:** a README saying what's in it, what isn't, and whether the unit's clock can be trusted; the status report;
  updates, with the slots, the history and the rollback reports, their saved lines less sign-ins; the network, the disk,
  the clock, the power mode and the workloads; the thermal log; this boot's journal and the one before, less sign-ins,
  newest 3 MiB of each.
- **Never in it:** workload output, which is the one place a prompt or an output can be; sign-ins; the audit log; the
  database or anything else from `/persist/console`; keys; credentials, taken out as above.
- **Says who the owner is:** the unit's name and serial, its addresses and channel, and addresses of devices on its
  network. The page says so before the download.

A bundle holds raw text, not only typed events from templates, which would explain only the failures somebody
foresaw. So the sources are programs whose output has been read, and the one place a prompt can appear never goes in a
bundle.

In recovery mode nobody can log in, so there are no logs; SSH and the serial console are the way in. A factory reset
already removes both slots' journals. The cloud gets none of it from reporting: it would be a grant of its own, asked for separately.

Left out: downloading the raw journal, searching on the unit, choosing a time range, a bundle with workload output, and
sending a bundle anywhere from the unit.

## State

`/persist/console/` holds users, tokens, grants, jobs and audit in SQLite, with a 64 MB budget including its
journal, out of the 232 MB usable on `/persist`. When space runs short, audit detail goes before security
state. Metric history is on `/data`, or in memory without an NVMe.

A rollback runs an older release against state a newer one wrote:

- **One `state-format` integer.** Each release declares what it can read and write. The install is refused
  unless the new release and the fallback can both use the resulting state. A break needs a bridge release.
- **Changes are additive.** An older writer keeps what it doesn't understand, or refuses the write. The
  integer alone proves nothing, so the rollback test below runs for every release.
- **No restore from snapshot.** A rollback would bring back revoked credentials without anyone asking. Restoring a
  backup does that too, which is why only an owner does it, with a fresh password, and the page says the backup's
  date (Backups).

| Event | Result |
| --- | --- |
| A/B update | State untouched on `/persist`, models and outputs untouched on `/data` |
| Rollback | Password changes and revoked tokens and cloud grants stay as they are |
| NVMe dead | Console, login, status and OS updates work; workloads show as unavailable |
| `/persist` full | Changes it can't record are refused; login and status still work |
| `/persist` corrupt or missing | Recovery mode: a public fault notice and nothing else, no claim, no workloads, no cloud. Needs a reflash, as it does today |
| Reflash | New identity, claim again. Nothing on the NVMe runs or is readable until claim asks keep or erase. Keep means models and outputs, not old containers or recipes |
| Change of owner | A factory reset erases users, tokens, grants, provisioning, identity, host keys and TLS key, and the NVMe unless told otherwise. A marker it clears last makes an interrupted reset resume before any service starts. It ends with a new claim code on the screen and in its output |
| Unit lost | Claim a new one and restore its backup (Backups): everything comes back as it was at the backup |

**Missing state must never look like a new unit.** A unit is claimable only through a claim code, which a
flash or `factory-reset` writes. `/persist` is formatted only by a flash. An NVMe is formatted at boot only
with the permission a flash leaves, once. Otherwise the console asks, whatever the disk looks like. Both used
to format whatever had no recognisable filesystem, so a damaged partition counted as blank.

### The data disk

`/persist/data-disk` names the one filesystem this unit has accepted. A flash's format, an owner's answer or a
factory reset writes it. Anything else stays unmounted: `patai-data-gate` decides at boot, ahead of
`data.mount` and `docker.socket`. Holding back the workload alone wouldn't do. Docker restarts old containers
by itself, and any `docker` command starts Docker, which mounts `/data`.

| State | What it is | The console |
| --- | --- | --- |
| `ready` | The accepted disk, mounted | Nothing to ask |
| `undecided` | A `data` filesystem this unit hasn't accepted | Keep or erase |
| `foreign` | Anything else: new, from another machine, or damaged | Erase and set it up |
| `absent` | No NVMe | Says so |
| `unmounted` | The accepted disk, which didn't mount | Says to restart, then that it needs service |

Claim asks straight after the recovery codes. "Decide later" leaves the disk unmounted and the dashboard says
so. Only an owner can answer, with a fresh password, and the helper checks the state again. To choose by, the
console shows about how much is on it, worked out from the superblock with nothing mounted.

**Keep** removes everything but `models/`, then anything in those that isn't a regular file or a
directory. Compose hands those directories to containers, so a symlink left there would hand them the host.
Image layers go because nothing can vouch for what's on a disk that has been elsewhere, and images come back
from GitHub, pinned. A model is checked against its manifest's hash again, and an owner's own is a GGUF or
safetensors file, which holds weights and can't run code. An app's data, such as Chat's people and conversations,
goes with the rest.

### Factory reset

From the System page, for an owner, with their password and the unit's name, or `patai-console factory-reset
<unit-name> [--keep-data]` as root. Tokens can't reach it and neither will the cloud.

It only leaves `/persist/factory-reset`, holding keep or wipe and the next owner's claim code, and restarts.
`patai-factory-reset` does all the erasing early in the next boot, before the identity, the disk gate or the
journal reaching the slot, so an interrupted reset and a new one are the same path: every step can run twice
and the marker goes last. It carries on with that boot rather than rebooting, because a reboot before the
health check costs a slot a retry. If the marker is still there when `patai-api` starts, the reset couldn't
finish, and the console stays in recovery mode rather than offering what's left.

The claim code is made before the restart, so whoever asked gets it, which is the only way to hand over a unit
with no display. It goes back in that one response and isn't kept in a job or the audit log.

| Goes | Stays |
| --- | --- |
| `/persist/console`, `provision`, `identity`, `report` and `logs`, so also SSH keys, the update channel and the chosen workload, and a restore waiting to be put in place | `/persist/rauc`, which says which slot is which |
| On both slots: host keys, journals and logs, temporary files and crash dumps, `admin`'s home and password, root's SSH directory. The machine-id is replaced | The two slots' images |
| The NVMe with wipe: discarded and formatted. With keep it's forgotten, so the next owner is asked | |

A disk that can't be erased, because it's missing or failing, is forgotten instead, so it stays unmounted and the
next owner is asked about it. A reset takes away access. It can't vouch for a slot: anyone who had root on a unit
could have changed its rootfs, so for a unit that's left your hands, reflash it.

## Backups

A disaster-recovery backup: what makes a unit what it is, so that a new unit restored from one is the old one again,
with its people and their passwords, their chats, files and memories, its keys, models from services, apps and their
settings, network and power mode (Patrick, 2026-11-19: "not just an export of chats"). Daily by default, to a disk
plugged into the unit, a shared folder on a NAS, an SFTP or WebDAV server, or an S3-compatible bucket, encrypted with a
key only the owner keeps. On the Backups page, for owners.

A backup restores onto any kind of node, not only another Xavier (Patrick, 2026-11-19: "I wouldn't want the backup to
like hard code it needs this hardware"): an x86 machine with an NVIDIA card, an AMD one, a Jetson Orin, a server
somewhere. Nothing in one names a device, a disk or a port's driver; what only one kind of node has is kept apart from
what the unit is anywhere, and the apps' data is in a form any node reads.

### What's in one

A backup is in three layers:

| Part | What |
| --- | --- |
| **The unit**, restored onto any node | |
| `unit/console` | The console's database, copied at that moment (people, password hashes, recovery codes, tokens and keys with their limits, usage, the request log, jobs and audit); models from services with their keys (`outside.json`); the app in front; Tailscale's settings; an owner's own certificate and key; the unit's own certificate and key; and the backup settings (`backup.json`): the schedule, the series, and the public half of the owner's key |
| `unit/provision` | The apps on, their APIs and the services' settings (`apps.json`); the models kept and in use (`models.json`); the services' secrets (`secrets.json`), such as Chat's database password and the key its sessions and connectors' sign-ins are sealed with; `admin`'s SSH keys; the name; the update settings; the serial console's password hash |
| `unit/identity` | The SSH host keys |
| `unit/tailscale` | Whether Tailscale is on, and whether its logs go to Tailscale |
| **The platform**, restored only onto a node of the same kind | |
| `platform/provision` | The power mode, which is one of this kind of node's modes; the network settings, which are by its ports (`gbe1`, `gbe2`); the update channel, whose images are for it |
| **The apps' data**, in a form any node reads | |
| `apps/<service>/files` | The folder of each service whose manifest has `data`: Chat's photos and files, agentic-api's stored responses and conversations in SQLite, and each person's computer |
| `apps/<service>/postgres` | A Postgres service's database as a dump (`pg_dump`'s custom format): Chat's people, chats, projects, memories, documents and scheduled tasks |

The manifest, the first entry, says which platform made it (RAUC's `compatible`, `patai-node-xavier` here, which is what
an update checks a bundle against too), its processor (`arm64`), its release, and how each service's data is held, or
that it had none.

**Left out**, as a new unit has it already or gets it again:

- The release. A restore runs on whatever the unit runs, as long as it reads the backup's console state format
  (`console/STATE_FORMAT`) and reaches the floor of the apps' data in it (`updates.md`, Data across slots); a newer
  Chat migrates the restored database when it starts.
- Pinned models and images, which come back by their pins from `models.json` and `apps.json` once the apps start: on
  patai-dev that's Gemma 4 26B's 16 GB before Chat answers. An owner's own model added by address comes back from it.
  One uploaded doesn't (open questions, below).
- `/persist/rauc` (the slots, what was installed, the channels' floors), `/persist/report` and `/persist/logs`, which
  are that hardware's history, and the journals.
- The data disk's acceptance, the machine-id and the claim code, which are the new unit's.
- Where backups go and how the unit signs in there (`backup-target.json`), and the key it's sealed with
  (`sealing-key`): the unit's own, which a restore keeps (Where backups go is sealed, below).
- Tailscale's state, its node in the tailnet (The unit's identity, below).
- Docker's own data, `patai-fetch`'s markers, downloads half done, temporary computers.
- A service's cache, the folders its manifest's `data.cache` names, which it makes again: agentic-api's `tmp`, its code
  interpreter's runtime unpacked, 235 MB, which made a backup of patai-dev 73.7 MiB. Left out, it was 492 KiB, 2 MB
  before compression, in 2 s (release 2026.11.21-code1).

### Where they go

One place, of five kinds, each a folder in it. Each is tested with a small file written, read back and deleted before
it's kept. A file is written under a hidden part's name and given its own only once the helper says the backup was
whole, so a half-sent backup is never listed. A download gives up only when nothing has arrived for two minutes, so a
slow link finishes. Every kind but the disk is reached by `patai-api` itself, with nothing mounted and no root.

| Kind | For | How |
| --- | --- | --- |
| **Disk** | A USB drive, or any disk plugged into the unit that it isn't using. Covers the node breaking, a disk dying, an update going wrong, everything deleted; not a fire or a theft, which take the drive too | ext4 or exFAT, which most drives come with. NTFS can only be read in this kernel and FAT32 can't hold a file over 4 GiB, so for those, and a disk with nothing on it, the page offers Format, which erases it and makes it ext4 after the owner's password. Known by its filesystem's ID, so the port and the name Linux gives it don't matter. `patai-rootd` mounts it only while it's used (`nosuid,nodev,noexec`) and lets it go 30 s after; the console reads and writes through files the helper opens in its `patai-backups` folder, as an `os.Root`, so a link on a disk that's been elsewhere reaches nothing. The data disk, the eMMC and anything mounted are never offered |
| **Shared folder** | SMB: a Synology, a QNAP, a Windows or macOS share | SMB 2 and 3, NTLM, from Go (`cloudsoda/go-smb2`, the fork rclone uses) rather than the kernel's `cifs`, so nothing is mounted and a broken NAS can't reach the kernel. Only to an address on the unit's own networks or the tailnet: SMB isn't safe over the internet |
| **SFTP** | Any server over SSH, a Hetzner Storage Box, a Synology | A password, or a key the unit makes for itself, whose public half the page shows for the server's `authorized_keys`; it stays on the unit, as a credential a unit makes does (ops `decisions/0009`). The server's key is shown, as its SHA-256 fingerprint, for the owner to confirm before it's kept; a server whose key then changes is refused |
| **WebDAV** | Nextcloud, ownCloud, a NAS's WebDAV, pCloud, Koofr | HTTP, written here as the S3 client is: PROPFIND, MKCOL, a chunked PUT and a MOVE. Redirects aren't followed, as one would drop the password. https, or http to the unit's own network |
| **Bucket** | S3: Amazon S3, Backblaze B2, Cloudflare R2, Wasabi, OVHcloud, Hetzner, Garage or MinIO | SigV4, 16 MiB parts in one multipart upload. https, or http to the unit's own network. A provider's versioning or object lock keeps old backups from someone who takes the bucket's key from a stolen unit |

Region matters for a bucket: Amazon wants the bucket's own, R2 `auto` and Garage `garage`. A NAS's https with a
certificate it signed itself isn't taken: use http to its address on the network, as the backup is encrypted anyway.

Not built:

- **FTP.** It sends the password in the clear, and whatever offers FTP offers SFTP or SMB.
- **NFS.** No sign-in but the address, and it's root's to mount.
- **Google Drive, OneDrive, Dropbox** (open questions).
- **Download and upload in the console**, for an owner with nowhere else (open questions).

### Where backups go is sealed

What the unit signs in with where backups go, an S3 secret, an SMB, SFTP or WebDAV password, or the private half of
the unit's own SFTP key, is in `/persist/console/backup-target.json` (0600, the console's), each sealed with AES-256-GCM
and bound to what it's for, so one can't be moved into another's place. The key is `/persist/console/sealing-key`, 32
random bytes made on the unit the first time something is sealed (`console/internal/sealing`). Neither file is ever
in a backup, and a restore keeps the unit's own of both, so a backup never holds a sign-in. Nothing is lost by that: a
restore starts from somewhere the unit reaches already, and backups go on there after it. A copy of the file without
the key holds nothing of them; a factory reset takes both away. The rest of the backup settings, in `backup.json`, are
in backups, so the schedule, the series and the owner's key's public half come back.

### Schedule and keeping

Daily at 03:00 in the owner's time zone, which the page takes from the browser, so it stays at night across daylight
saving. Kept: the newest of each of the last 7 days, 4 weeks and 6 months, so 17 at most and a fault noticed late can
still be gone back on. Each is a job, "Scheduled backup" by `schedule`, in Recent jobs and the audit log. The slot is
marked done before its job is queued, and the job's idempotency key is the slot, so a restart can't make it twice. A
unit that was off at its time makes it once when it's back; a slot from before the schedule was saved never runs.
After a backup has been sent, this unit's older ones (its series, below) the keeping doesn't want are deleted, each
with its record, never the newest. Run now makes one at once, and the same keeping applies.

A backup runs beside the job queue, not in it, so an hour of sending doesn't hold up a restart or an update, one at a
time with checks and restores. The Backups page shows what it's doing, how much has been sent, and the last one's
result.

### Encryption and the owner's key

A backup is one file, `<unit>-<UTC time>.tar.zst.age`: a tar in zstd in [age](https://age-encryption.org),
encrypted to the owner's key, a post-quantum hybrid one (ML-KEM-768 with X25519, age 1.3), since backups are kept for
months by someone else. An owner can open one anywhere without a unit: `age -d -i key.txt <backup> | zstd -d | tar -x`.

- **The key** is made on the unit when the owner asks, Make a key on the page with their password, and shown once:
  one line, `AGE-SECRET-KEY-PQ-1…`, with Copy and Download, a text file that names the unit and the key and that age
  reads as it is. The unit keeps only its public half, which encrypts, and the seal key, derived from the key with HKDF,
  which seals (Integrity). Neither opens a backup. A key made is kept only once the owner gives it back, pasted or from
  the file, which shows it has been saved somewhere outside the page; until then it's in memory, for 30 minutes.
- **How an owner keeps it:** in a password manager, and a copy away from the unit, such as the file on a USB stick or
  printed. The page shows its ID, `xxxx-xxxx`, as does each backup, to tell keys apart. Check a key says whether one is
  the unit's. Replace key makes another; backups made before open only with the old one, and the list says which key
  each was made with.
- **Lost,** its backups can't be opened by anyone, Wasupu included, and the unit can't either.
- **Why the unit doesn't keep it:** a stolen unit has the bucket's key too, so with the backup key it would open every
  backup in the bucket, with everything deleted since. Without it, a thief has only what's on the unit now.
- **Why not a passphrase:** one a person chooses can be guessed by whoever holds the bucket, with no limit on tries. A
  generated key can't, and a password manager keeps one as easily.

### Integrity

- **The seal.** A backup's last entries list every entry before them, the manifest first (name, type, mode, owner,
  size, link and each file's SHA-256), and an HMAC-SHA256 of that list with the seal key. A restore checks each entry
  as it reads it and takes nothing the list doesn't name, in its order. Anyone with the public half could encrypt a
  file, but only the unit and the owner's key can seal one, so a bucket can't plant a backup that would restore its
  own people or SSH keys.
- **age** authenticates every 64 KiB and the end, so a byte changed or a backup cut short fails.
- **The record** beside each backup, `<backup>.json`: when, the unit, its release, the key's ID, the series, the size
  and the SHA-256 of the file, sealed too. It holds nothing the bucket can't see anyway, and the list comes from it.
- **Check**, on a backup's menu, reads it back and compares it with its record, without the key, so the unit can
  check its bucket by itself. With the key it opens it too and checks every file against the seal, keeping nothing.

### Consistency

- **The console's database** is copied with `VACUUM INTO` into its runtime folder, `/run/patai-api`, a moment's
  state of it; jobs still open are marked so in the copy.
- **`patai-rootd`** copies `/persist`'s parts into a folder of its own on the data disk, plain files only and with
  nothing followed, then pauses every running container of the services that keep a folder and write to it (Docker's
  freezer: Chat's server, agentic-api, each computer), dumps each Postgres database from its server, copies the other
  folders, and lets them go. Chat's server writes each file once, into a folder it renames into place, so its files
  are copied as hard links, at once (`data.backup: link` in its manifest); the rest are copied. SQLite is frozen with
  everything it has written at one moment, so the copy is what a power cut then would leave, which it's made to start
  from.
- **Postgres** (`data.backup: postgres`, with its database and user) keeps running, and nothing writes to it while
  what does is paused, so its dump and the files are of one moment. A Postgres data folder is only promised to work
  under the same major version on the same kind of machine; a dump loads into that version or a newer one anywhere.
  With Chat off there's no server, so the helper runs Postgres from the service's own image on its folder alone, with
  no network and only its socket, holding `patai-workload`'s lock so the service can't start beside it.
- The copy is written through a pipe to `patai-api`, encrypted, which sends it where backups go. Nothing of it is
  stored on the unit, and the copy is deleted at the end, or when the helper next starts, which also lets go of any
  container a helper that died left paused.
- What `/persist` needs to survive a power cut is in the restore: a sync before each rename and of its folder after.

### Restore

**Onto a new unit,** freshly flashed or factory-reset with its disk erased: claim it, answer the data disk, then
"Start from a backup?" takes the owner to the Backups page. They choose where the backups are, plugging the disk in
first if that's where, and the list shows every backup there, from any unit, newest first. **Onto the same unit,**
it's the same list. Restore asks for the key, pasted
or its file, whether the unit the backup came from is still running (The unit's identity), and the owner's password.
It's refused during an update.

1. `patai-api` fetches the backup and streams it into `patai-rootd`, which opens it with the key and unpacks it into
   `/persist/.restore` and `/data/.patai-restore`, root's only, each opened as an `os.Root`, so no name in a backup can
   reach outside them. It refuses a backup this release can't read the console state of, one with a service's data in a
   form this release doesn't keep it in, one whose apps' data is newer than this release can use or whose database
   is from a newer Postgres (`updates.md`, Data across slots), or one the disk has no room for, before it writes
   anything. The key is used and forgotten: it's in memory for the job, never in a file or the
   database.
2. Only once the seal and the bucket's record both match is the restore marked complete. Then the helper leaves
   `/persist/restore` and restarts the unit. Anything wrong before then changes nothing on the unit.
3. Early in the next boot `patai-restore persist`, before the identity, the root helper and the console, swaps in
   `console`, `provision` and `tailscale` whole and the host keys over the unit's own. Then `patai-restore data`,
   once `/data` is mounted and before Docker, swaps in each service's folder, and removes one the backup didn't have.
   A Postgres service's folder is its dump alone, `restore.dump`: its image (`workloads/chat-postgres`) makes an empty
   cluster at its first start, as on a new unit, and loads the dump into it in one transaction, with its server on its
   socket only, before anything can connect. A dump that doesn't load takes the cluster with it and is kept, so the
   next start tries again rather than coming up empty.
   Every step can run twice, and each marker goes last, so an interrupted restore carries on at the next boot. While
   `/persist/restore` is there the console stays in recovery mode.
4. The unit comes up as the backup's: people log in with the passwords they had then, as do Chat's, and the apps
   start from `apps.json`. The console's sessions, held in memory, don't carry over; Chat's, in its database, do, so a
   device signed in at the backup is signed in again. The audit log, the backup's, gains `restored`, and the Backups
   page says which backup it was.

Credentials are as they were at the backup: a token, key or person removed since comes back, and one made since is
gone, which is why the page says the backup's date. Where backups go isn't: it's where the unit restored from, with the
sign-in it was given, so the next backup goes there. A factory reset forgets a restore waiting to be put in place.

### Onto another kind of node

A restore takes the unit's layer whatever made it, and the apps' data of every service this release has, wherever its
release keeps it, on whatever disk it accepted: an NVMe, a SATA disk, a larger one. The platform's layer is taken only
from a backup of the same platform; otherwise the node keeps its own power mode, network settings and update channel,
and the Backups page lists what wasn't taken. So does a service's data when this release has no such service. Apps and
models come back as the owner's choices in `apps.json` and `models.json`, fetched by this release's pins for this kind
of node.

What doesn't carry over by its nature: programs a person installed on their computer are for the processor that ran
them, though their files come back; and an app this kind of node can't run, such as one needing more memory than it
has, is the owner's to turn off. Only Xaviers exist so far, so a restore onto another platform has been tested with a
backup that says it's from one, not with other hardware.

### The unit's identity

A restore makes the unit the one backed up, so that every device that knew it keeps working: a browser that accepted
its certificate, `known_hosts` on the owner's laptop, Chat's native apps, a static address in a router's rules.

| | A restore | "The unit it came from is still running" |
| --- | --- | --- |
| Name | The backup's | This unit's |
| Network settings | The backup's, a static address included, from the same platform; this unit's from another | This unit's |
| The unit's own certificate | The backup's, so its fingerprint is the old one, not the one on a new unit's card | This unit's |
| SSH host keys | The backup's | This unit's |
| An owner's own certificate | The backup's | The backup's |
| Tailscale | This unit's own, if it has any; otherwise the backup's settings: on but logged out, so the owner logs in again and removes the old machine from their tailnet | The same |
| The node id phones know it by (`/persist/console/node-id`, Chat's Connect phone) | The backup's | This unit's |
| machine-id, the data disk's acceptance | This unit's | This unit's |
| Where backups go, its sign-in and the sealing key | This unit's | This unit's |

Tailscale's node key is never in a backup: two machines with one node key take turns being reachable, Tailscale's own
advice is never to copy its state, and a factory reset logs a unit out of its tailnet anyway. "Still running" is for
moving to new hardware while the old one is in use: two units on one network with one name or address would answer
for each other.

### Measured

On patai-dev, 2026-11-19, release 2026.11.19-backup4, to a bucket on the LAN (`rclone serve s3` on a PC, over its
Wi-Fi):

- **What it held:** Chat with 4 people, 4,978 chats and 41,305 messages (most from a ChatGPT import), 2,556 files, 37
  projects and a memory; 4 computers; three models from services with their keys; three API keys; Chat in front; a
  power mode and a DNS setting kept. 1.46 GB of files, 1.24 GB in the bucket after zstd and age.
- **The scheduled backup** ran by itself at its time and took 73 s, sending at about 25 MB/s. The apps were paused for
  6.1 s, Chat's files being links. Check with the key passed, and the stock `age` 1.3.2 and `zstd` opened it on the PC.
- **A factory reset with the disk erased** brought the unit up in 1.5 minutes as `patai-<serial>`, unclaimed, with a
  new certificate and host keys, nothing on the disk and nothing but the claim on `/persist`. Claimed, given the bucket,
  it listed the backup; the restore fetched and checked it in 70 s, and the unit was back as patai-dev 60 s after.
- **After:** the console's people, tokens, API keys, models from services, the app in front, power mode, network
  settings, SSH keys, update settings, Tailscale's settings, the apps on, the name, the certificate's fingerprint, the
  host keys and the hash of every file in `/persist/provision` and of every file in the apps' folders were as before. The
  owner logged in with the old password, over SSH with the old key, the restored API key answered, and the outside
  services' keys listed their models. Chat's tables had the same rows, its three people signed in with their
  passwords, a photo sent before came back byte for byte, and it answered a new question.
- **The apps** come back once `patai-workload serve` has fetched everything they pin: it fetches all of it before it
  starts any, so on an erased disk Chat waited 38 minutes for its 20 GB of models (11 MB/s here), not only for its own
  images.
- **The models** came back from their pins, not from the backup: those `models.json` keeps (Gemma 4 12B, and 26B with
  its projector and MTP draft), with each service's defaults (nomic-embed, two Piper voices, Whistle), 22 GB in all.
  Pinned models that were on the disk but not kept, such as the benchmark's and FLUX.2's, and files no manifest pins
  didn't come back: the disk held 134 GB before and 26 GB after.

On patai-dev, 2026-11-20, release 2026.11.20-backup1, to throwaway servers on a PC on the LAN (Samba 4.19, OpenSSH 9.6
and `rclone serve webdav`, in containers), with Chat's test data after the import was removed (3 people, 4 chats, 12
messages, a file), 0.93 MB of files, 197 KiB sent:

- **Each kind** was set on the Backups page's API, tested, and took a backup and its record in about 7 s: SMB with a
  password, SFTP with a password and with the unit's own key (its fingerprint as the server's `ssh-keygen -l` gave
  it), and WebDAV. Check with the key passed. Opened on the PC, a backup held `unit/`, `platform/provision/power-mode`
  and `apps/`, Chat's database as a 42 KB dump owned by its user, and named `patai-node-xavier` and `arm64`.
- **With Chat off,** the helper ran Postgres alone on its folder: the same 42 KB dump, the apps paused 2.4 s, nothing
  left running.
- **A restore onto the same platform** after the power mode was changed to 3 and an API key added: the power mode
  came back as MAXN, the key was gone, and Chat's Postgres made a new cluster (a new system identifier) and loaded the
  dump at its first start, with the same rows. `bench` signed in to Chat.
- **A restore onto another platform,** with the unit's `compatible` made `patai-node-orin` for the moment: the unit
  kept its own power mode, 3, and the report said what it didn't take. Chat came back as before.
- Not tested: a USB disk, as patai-dev had none plugged in.

### Open questions for Patrick

1. **Tailscale's node key:** back it up and restore it onto other hardware, so the unit keeps its tailnet name and
   address with no login, or log in again (as built, recommended: the old unit may still boot, and Tailscale advises
   against copying its state).
2. **An owner's uploaded models:** include them as an option, which can be tens of GB, or leave them out (as built,
   recommended for v1: the page could list what didn't come back).
3. **Google Drive, OneDrive, Dropbox**, for owners with no NAS. Each works from the unit with no cloud of ours:
   Google's sign-in for TVs and devices allows `drive.file` (only files the app made), and the page would show a code
   to enter on a phone; OneDrive's device code sign-in and Dropbox's pasted code need no secret. Each needs Wasupu to
   register an app with that company, and Google's device client has a secret that would be in every image, against
   the image's no-credential rule, though Google doesn't treat it as one for devices.
4. **The unit's own certificate** onto other hardware: the backup's (as built, so nothing warns again), which leaves
   the new unit's screen showing the old fingerprint and its card a stale one.
5. **Retention defaults** of 7 days, 4 weeks and 6 months, and 03:00.
6. **Apps after a restore onto an erased disk:** `patai-workload serve` could start the services whose images are there
   while the models still download, so people sign in to Chat at once rather than after 20 GB (not changed here).
7. **Pinned models on the disk but not kept**: record in the backup which pinned models were
   there, so a restore downloads all of them again, not only those `models.json` keeps (recommended: it's a list, and
   each comes from its pin).
8. **A second destination** with its own schedule, such as the disk nightly and a bucket weekly, as a disk doesn't
   survive a fire or a theft (recommended, not built).
9. **Download and upload in the console**, for an owner with nowhere else to keep one: the same stream, through the
   browser; a PC that syncs its downloads to a cloud drive then keeps it there too.
10. **Space:** each backup is whole, so 17 kept of patai-dev's 1.2 GB is 20 GB, fine on a disk or a NAS and more than
    a free 15 GB cloud drive. Sending only what changed would need a different format (recommended later, if cloud
    drives come).

### API

All for owners, `/api/v1/backups`:

| Route | Does |
| --- | --- |
| `GET /backups` | The key's ID, where backups go without its secret and in a line, the unit's SFTP key's public half, the schedule and the next slot, what's running, the last backup's job, the last restore with what it didn't take |
| `GET /backups/list` | What's where backups go, from the records, newest first |
| `PUT /backups/destination` | Set where backups go (`kind`: `disk`, `smb`, `sftp`, `webdav` or `s3`, and `folder`), tested with a file written, read and deleted; a fresh password |
| `GET /backups/disks`, `POST /backups/disks/format` | The disks backups could go on, each filesystem and why one can't be used; format one (a fresh password) |
| `POST /backups/host-key` | An SFTP server's key and fingerprint, to confirm before the destination is set |
| `POST /backups/ssh-key` | The unit's own key for SFTP, made the first time |
| `POST /backups/key`, `/key/confirm`, `/key/check` | Make a key (a fresh password), keep it once it's given back, say whether one is the unit's |
| `PUT /backups/schedule` | On or off, the time, the zone and the keeping |
| `POST /backups/run` | A backup now, as a job |
| `POST /backups/check` | Check one, with the key or without, as a job |
| `POST /backups/restore` | Restore one, with the key, whether to keep this unit's identity and a fresh password, as a job |

`patai-rootd`'s verbs are `backup-open`, which answers with the read end of a pipe, `backup-finish`, `restore-open`
and `restore-check-open`, which answer with the write end, `restore-finish`, `restore-apply` and `restore-discard`.
For disks: `backup-disks`, `backup-disk-format`, `backup-disk-files`, `backup-disk-read` and `backup-disk-write`, which
answer with an open file, `backup-disk-commit`, `backup-disk-abort` and `backup-disk-delete`.

## Model downloads

The Apps page (`apps.md`) shows each app and what it's doing, and an app's
own page, in tabs, its state, APIs and services, its models, its APIs' keys, their usage and request log, and its
services' settings, from `/api/v1/workloads/status`. Turn on, Turn
off and Download only are jobs on `/api/v1/apps/<app>/`, and an API's switch is
`/api/v1/apps/<app>/apis/<api>/on` or `off`. Turn on gets what's missing and runs it. Download only gets it without stopping whatever is serving, for an operator or a token, as
with Turn on. It isn't held in a job, because jobs run one at a time and a download can take hours: the job ends when the
download begins, and progress and failure come from the status. A failed start says why instead of reading as a slow one.

Models are the ones each manifest pins, plus an owner's own GGUF or safetensors, by address or upload, for an owner
only. Use, Download and Remove are jobs, `model-use`, `model-fetch` and `model-remove`, for an operator, with the
service and model as the target. An upload goes to `PUT /api/v1/workloads/<service>/models/upload/<name>`. `patai-api` can't write
to `/data` by its sandbox, so `patai-rootd` gives it the write end of a pipe across its socket, and writes the file
itself, no more of it than the size given. The console never holds the file, so it can't change a model once it's
been checked, or fill the disk. `apps.md`, Running them, has where models come from and how they're checked.

## Updates

The Updates page, `/api/v1/updates` and `patai-update` underneath. A slot isn't touched until someone or
something asks, and the unit keeps working while it's written.

| Who | Can |
| --- | --- |
| Viewer | See the release, what's on offer, progress and history |
| Operator, or a token | Check the channel, install what it offers, restart |
| Owner, with a fresh password | Set the channel and whether updates install or restart by themselves, install a file |

An operator can only take what the owner's channel is offering, and only the release they were shown, so it
needs no password, and the cloud can use the same route under its own grant. A file can be any signed release,
including an older one, which makes it the owner's call.

**A channel** is any plain file server, so an owner with no internet can use one on their LAN. Workload images come
from it too. The address has to be a plain `http` or `https` folder: it ends up on a root command line, and one
holding a password would be on show to anyone who can read the status. It has a name, which the owner sets with the
address, and a copy on another server keeps. Changing either forgets the last check, which was another channel's
answer. Its `latest.json` is signed by the fleet for that name, and a unit never takes one older than it has seen
(`updates.md`, Channels), so a channel can't offer an older release unless the fleet signs it doing so. The Updates
page says when the channel's file was signed, because nothing on a unit can tell a quiet channel from a stalled one.

**A file** goes to memory (`/run`), not the NVMe, so it works with no disk, and a power cut leaves nothing behind.
It needs 1 GB free, and the console refuses at the start if that isn't there, rather than running a workload out of
memory. `patai-api` only receives it. `patai-rootd` moves it out of that account's reach, checks it through an open
descriptor that follows no link, because root changing the owner of a name is how a planted link does damage, then has
RAUC check its signature, and a bad one is gone before anyone is asked anything. Sending a file installs nothing. It
waits an hour for an owner's password, then it's forgotten. The password goes with the install, not with a request
lasting minutes.

**From a channel,** the release is downloaded whole before RAUC sees it, onto the data disk if there is one and
otherwise into memory, which workloads aren't using then. The page shows how much has come, from the size the
channel's signed file gives. A unit with room in neither place says so and installs nothing.

**Installing** is one job, and jobs run one at a time, but the work runs in a service of its own, so a restart of
the console or of `patai-rootd` doesn't stop it, and the job picks up watching it. A person's install never restarts
the unit: the page says when it's ready. While anything is writing a slot, including a RAUC service that outlived
whatever started it, a restart is refused, and an install is refused while a restart is on its way. A power cut during
an install costs the slot being written, and the next install repairs it (`updates.md`). Saving settings puts what
limits the unit first and what frees it last, so a check or a power cut partway through can't install against the
owner's choice.

A file carries the OS only. If a release changes a workload's image, a unit with no channel can't fetch the new one,
and that workload won't start until it can.

## Boot validation

A release with a dead console is a bad release, so the health check gains: `patai-api` answers on loopback
HTTPS, serves the UI, opens its login store or enters recovery mode, and `patai-rootd` answers. It doesn't
need DHCP, a display, the cloud, the NVMe or a workload. **Recovery mode passes only for an identified
storage fault**, such as `/persist` failing to mount, because that isn't the release's fault and mustn't roll
back both slots. A schema mismatch or a failed start fails the check, so a bad release can't hide there. The
order is: prepare state for this release, prove the above, `mark-good`. Changes from users wait until then. A login doesn't: it changes
nothing a rollback could lose, and someone watching a unit restart shouldn't be turned away.

## Reporting on the unit

`report.json` holds version, booted and primary slot, each slot's state, `is-system-running`, failed units,
last update check, temperatures, fan, power mode, disk use, NVMe presence, and the workload and whether it
answers. Three changes on the unit go with it:

- **A dead channel isn't a sick unit.** `patai-update` treats a failed fetch as a result: it exits 0 and
  records the time and outcome, so the console shows an overdue check and the unit stays `running`.
- **A rollback explains itself.** Each slot keeps a persistent journal capped at 64 MB. On boot, seeing the
  other slot `bad`, the unit mounts it with `ro,noload`, so nothing is replayed, and copies its failed units
  and last lines to `/persist/report/`, keeping the last five, up to 1 MB. A slot too damaged to mount is
  reported as that.
- **`latest.json` is signed**, with a sequence number the unit won't go below, so a file server can't offer an
  older signed release. Rolling a fleet back on purpose publishes a higher sequence naming the older one
  (`updates.md`, Channels).

## Cloud link

The cloud is [patai_cloud](https://github.com/patrickwasp/patai_cloud), optional and not built yet. What's fixed is the
contract, so the local API won't change for it.

- **Linking.** Off until an owner connects it: the console shows a code, the owner enters it in the cloud and
  confirms the organisation and the grant. It's bound to an Ed25519 key the unit made for itself.
- **Transport.** HTTPS to port 443 only: reports by POST, requests by long poll. Each request is signed by
  the unit and carries a counter, so it can't be replayed. There's one path, no VPN and no general tunnel: an owner's own tailnet (Remote access with Tailscale) is theirs, not the cloud's. Long poll passes more proxies than a WebSocket, but not all, so the console shows why a
  link is down, and a proxy that intercepts TLS isn't supported.
- **A year offline.** The unit's key doesn't expire. It re-enrols by proving it holds that key, and rotating
  the cloud's trust needs a bridge release or an offline update.
- **Permission** is the cloud user's role intersected with the local grant, enforced in `patai-api`. A grant
  starts at reporting only; workload operations and signed OS updates are separate grants.
- **Commands** carry an id, an expiry and the grant's generation, and are rechecked at execution, so a
  reconnect can't run a stale reboot. Revoking closes the link and cancels queued work, and a rollback doesn't
  undo it.
- **Never from the cloud:** users, tokens, SSH keys, network, channel, its own grant, formatting, a shell,
  arbitrary models or Compose. The person it names goes in the local audit log as that organisation's
  statement, not as a proven identity.
- **Reports** carry health and versions. No prompts, images, outputs or raw logs by default.
- **The cloud has its own UI** and uses capabilities. It doesn't serve a unit's pages from its own origin,
  because a compromised unit could then attack the fleet account.
- **The cloud notices units that stop reporting,** which a unit can't do for itself.
- **Support shell,** after the rest, and the one deliberate exception to "no tunnel": an owner starts it, it
  shows on the screen, and it ends at its expiry, at a reboot or on revocation, closing any open session. It's
  SSH end to end against a pinned host key, and the cloud only carries the bytes.

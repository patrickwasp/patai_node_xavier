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

These services get CPU weight and OOM protection, because workloads share 32 GB with everything
else and the console has to answer when the unit is struggling. They get no memory reservation yet, though cgroup v2
can give one (`kernel.md`). With 23 GiB of GPU memory held and every core busy, the console answered every call anyway
(`apps.md`, Measured).

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
- **No restore from snapshot.** It would bring back revoked credentials.

| Event | Result |
| --- | --- |
| A/B update | State untouched on `/persist`, models and outputs untouched on `/data` |
| Rollback | Password changes and revoked tokens and cloud grants stay as they are |
| NVMe dead | Console, login, status and OS updates work; workloads show as unavailable |
| `/persist` full | Changes it can't record are refused; login and status still work |
| `/persist` corrupt or missing | Recovery mode: a public fault notice and nothing else, no claim, no workloads, no cloud. Needs a reflash, as it does today |
| Reflash | New identity, claim again. Nothing on the NVMe runs or is readable until claim asks keep or erase. Keep means models and outputs, not old containers or recipes |
| Change of owner | A factory reset erases users, tokens, grants, provisioning, identity, host keys and TLS key, and the NVMe unless told otherwise. A marker it clears last makes an interrupted reset resume before any service starts. It ends with a new claim code on the screen and in its output |

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
| `/persist/console`, `provision`, `identity`, `report` and `logs`, so also SSH keys, the update channel and the chosen workload | `/persist/rauc`, which says which slot is which |
| On both slots: host keys, journals and logs, temporary files and crash dumps, `admin`'s home and password, root's SSH directory. The machine-id is replaced | The two slots' images |
| The NVMe with wipe: discarded and formatted. With keep it's forgotten, so the next owner is asked | |

A disk that can't be erased, because it's missing or failing, is forgotten instead, so it stays unmounted and the
next owner is asked about it. A reset takes away access. It can't vouch for a slot: anyone who had root on a unit
could have changed its rootfs, so for a unit that's left your hands, reflash it.

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

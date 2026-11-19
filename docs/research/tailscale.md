# Tailscale on a unit, set up from the console

Researched 2026-11-18, for Patrick's question: install a Tailscale client on the unit and configure it in the console.
Sources are Tailscale's docs and its source at v1.104.7, pkgs.tailscale.com, Headscale v0.29.7, and the docs and code
of ten appliances that ship Tailscale. Everything about this repo was checked on patai-dev, release 2026.11.18-13.

**Decided 2026-11-18** (Patrick: "Yes, build it"), with the recommendations below: built from source, logs off by
default, the console on the tailnet too, the card on the System page. What was built is in `console.md`, Remote access
with Tailscale.

## In short

It fits, as **an owner's option**: off in every image, turned on by the owner on the System page, and joined to the
owner's own tailnet. With it, the owner reaches Chat and the console from their phone or laptop anywhere, at
`https://<unit>.<tailnet>.ts.net`, with a certificate browsers trust. There are no open ports and no Wasupu relay.

The way to do it:

- **Install:** tailscaled built from Tailscale's tagged source into the image, without its updater, in kernel (TUN)
  mode.
- **State:** on `/persist/tailscale`.
- **Logs:** uploads to Tailscale off by default.
- **Console:** about eight calls to tailscaled's local API, and a card on the System page.
- **Certificate:** the ts.net certificate goes into the console's certificate selector, beside an owner's own.

**It's against what the docs say now.** `console.md` lists "WireGuard or Tailscale" as rejected, and `chat.md` and
`apps.md` say Tailscale stays rejected. All three cite ops `decisions/0003`. That decision was about a different case:
a VPN from Wasupu's monitoring to client hosts, putting them on Wasupu's LAN. Here Wasupu has no tailnet and isn't on
the owner's. The case to put to Patrick is to keep 0003 for Wasupu, add Tailscale as the owner's choice, and reword
those three lines (Decisions for Patrick).

## What it gives an owner

- **Chat from anywhere.** On their phone or laptop, at `https://patai-dev.tail1234.ts.net`. This works before the
  cloud's relay exists (xavier-rogue `chat.md`, From outside the home), and without it.
- **The console from anywhere** at the same name on 9443, with its own login as now.
- **No certificate warning**, once the owner has turned on MagicDNS and HTTPS for their tailnet. Without that, the
  unit is still reached by its tailnet address, `100.x.y.z`, with the unit's own certificate and its fingerprint, as
  on the LAN.
- **Nothing to set on the router.** Tailscale connects out and traverses NAT, or relays through its DERP servers.

What Tailscale sees: its control server knows the unit's name, addresses, OS and when it's online. The traffic is
WireGuard, encrypted between the owner's devices and the unit, so Tailscale never sees a prompt. With log uploads off,
it gets no connection logs either.

## How other appliances do it

| Product | UI | How the owner logs in | Notes |
|---|---|---|---|
| Synology, QNAP | Tailscale's web client, wrapped | Log In opens Tailscale in a new tab | Editing moves to a second UI on `100.x:5252` |
| Home Assistant | Web client read-only, plus settings | Log In pop-up | Log upload off by default; offers Serve and Funnel |
| Unraid | Its own pages | Login opens the URL, also shown as a link | The fullest: health, peers, expiry countdowns, warning dialogs, daily ts.net certificate |
| TrueNAS, pfSense | A form | Auth key only | |
| OPNsense | Its own pages | Optional auth key, or the URL as text | Has a login-server field, for Headscale |
| GL.iNet | Its own page | A bind link | Beta; forces accept-routes on |
| Umbrel | Web client on the LAN | Log In | Likely no auth on the page |

Lessons for the console:
- **Don't embed Tailscale's web client.** On generic Linux it has no login at all. Its editing moves to `100.x:5252`,
  whose identity check is skipped on any control server but Tailscale's. Starting it also turns that port on.
- **Unraid is the model:** its own page, health shown, expiry warned of well ahead, and a warning before anything that
  can cut the owner off.
- **Nobody shows a QR code.** The console is often open on a laptop while the owner signs in to Tailscale on a phone,
  so one helps.

## On the unit

- **Kernel (TUN) mode, never userspace networking.** patai-dev has `CONFIG_TUN=y` and `/dev/net/tun`. In userspace
  mode tailscaled hands each connection to the unit's tailnet address to `127.0.0.1:<port>`. That would expose every
  workload kept on loopback behind the console's gateway (`netstack.go`). In kernel mode a socket bound to 127.0.0.1
  never sees them.
- **What the tailnet reaches.** Exactly what the LAN reaches: 80, 443 (Chat), 8443, 9443 (the console) and SSH on 22,
  which takes keys only.
  - By default Tailscale accepts tailnet traffic at the top of INPUT, before any rule of ours, and puts its jump back
    on top if moved. That would open 18000 to the tailnet, so the unit runs it with `--netfilter-mode=nodivert`: its
    chains exist, nothing jumps to them, and INPUT treats the tailnet as it does the LAN (found while building).
  - The gateway for apps on 18000 stays closed: `patai-firewall` lets only loopback and the Docker bridges reach it, so
    `tailscale0` is dropped.
  - Workload ports stay on loopback.
- **Containers.** A person's terminal is already kept off `100.64.0.0/10` and `fc00::/7` (`patai-firewall`), so a
  model's commands can't reach the owner's other devices. Other apps' containers can, as they can the LAN. That means
  Chat's connectors could live on the owner's tailnet. Keep it that way.
- **iptables.** Tailscale uses the same `iptables` as Docker (1.8.4, legacy; there's no nft here) and adds its own
  `ts-*` chains. Its CGNAT range doesn't overlap Docker's pools. `patai-network` should refuse a static address in
  `100.64.0.0/10`, as it refuses Docker's.
- **Settings the console fixes:**
  - accept DNS off, so the unit's resolver stays its own and containers don't break (#12225);
  - accept routes off, so an overlapping route can't cut the unit off its LAN, as Umbrel found;
  - no exit node, used or offered;
  - Tailscale SSH off; the unit has its own SSH.
- **Known issue.** Each Docker bridge made or removed makes tailscaled rebind, stalling tailnet connections for 10 s
  to a minute (#22137, open). Each app has a bridge, so turning an app on or off would briefly stall Tailscale.
  Measure it on the bench.
- **State.** `--statedir=/persist/tailscale` holds the node key, prefs, certificates and the netmap cache. It survives
  updates and rollbacks. A factory reset logs out first, if it can, then deletes it.
  - tailscaled fsyncs the file but not its directory (`atomicfile.go`), so the first state after a login could be lost
    in a power cut. The console has root sync the directory once the login finishes (`internal/durable`'s rule).
- **Log uploads.** tailscaled streams its logs, connection opens and closes among them, to `log.tailscale.com` unless
  run with `--no-logs-no-support`. Off by default, as the unit is local first.
  - The cost: Tailscale support gets no logs.
  - A tailnet with network flow logs (Premium and up) disconnects a node that won't log. The console shows Tailscale's
    health message and offers a switch to send logs.
- **Updates come with the OS.** Building without the updater (`ts_omit_clientupdate`) means "auto-update new devices",
  which is on by default in new tailnets, can't rewrite the slot.
  - The stock tarball would overwrite `/usr/sbin/tailscaled` in place. A `.deb` installed without Tailscale's apt
    list fails safe.
  - The cost: Tailscale ships stable every four weeks, with security fixes in between (TS-2026-014 to 017 in 1.104.4),
    so each OS release has to move Tailscale to its newest stable. Nothing says how long Tailscale's control server
    keeps accepting an old client.
- **Size and cost.** The official arm64 `tailscaled` is 41 MB, 13 MB of it debug info and symbols, plus a 32 MB CLI.
  A stripped build without the updater, web client, SSH, Taildrop and Drive, with the CLI built in (`--box`), is
  smaller. Tailscale publishes no memory, CPU or arm64 throughput figures, so measure on patai-dev.
- **Licence.** The client is BSD-3 with a patent grant, and its dependencies have no copyleft.
  - Tailscale's terms govern downloads from pkgs.tailscale.com, and they forbid redistributing them commercially.
  - Building from the tagged source is the clean route, and ships the notices (`tailscale licenses`).
  - There are no public trademark rules for naming Tailscale in a product's UI.

## The console

- **No SDK.** `tailscale.com/client/local` would pull in 77 of Tailscale's packages and tie the console to
  Tailscale's Go version (1.27.4 for v1.104.7; the console is on 1.26). Its local API is HTTP and JSON over
  `/run/tailscale/tailscaled.sock`, so a small client of its own does it.
  - Every call is made by the console's server, never passed through from a browser: tailscaled refuses any request
    with `Origin` or `Referer`.
  - The calls, the first group marked stable in the source:
    - `status`: state, addresses, DNS name, tailnet, key expiry, health, login URL;
    - `prefs` GET and PATCH;
    - `login-interactive`;
    - `cert/<name>`;
    - `start`: login with an auth key, or another control server;
    - `logout`.
- **Permission.** The console runs as `patai-api`. tailscaled gives it read-only access unless it's the operator, so
  the unit starts tailscaled with `OperatorUser: patai-api` in its config file. Certificates need that, or
  `TS_PERMIT_CERT_UID`.
- **Starting and stopping** is root's: `patai-rootd` verbs to enable and start, stop and disable the unit, and to
  sync the state directory. Turning it on, logging in and out, and turning it off take a fresh password, as network
  settings do. Never from a token or the cloud.

### The card, on the System page

1. **Off.** "Remote access with Tailscale", a line saying what it is, and **Turn on**.
2. **Log in.** **Log in to Tailscale** asks tailscaled for a login, then the card waits for the URL. It shows it as a
   link and a QR code, for signing in on a phone. **Use an auth key** takes a key from the owner's admin console,
   reusable, tagged or pre-approved. It's used once and not kept. **Control server**, under More, is for Headscale.
3. **Waiting for approval** (`NeedsMachineAuth`). The tailnet approves devices, so this links to its admin console.
4. **On.** The card shows:
   - the tailnet;
   - the unit's tailnet name and addresses;
   - links to Chat and the console at the ts.net name;
   - key expiry, warned of 30 and 7 days ahead, with the advice to turn expiry off for the unit in the admin console;
   - Tailscale's health messages;
   - **Turn off**, which keeps the login, and **Log out**, which removes the unit from the tailnet.
5. **Certificate.** Once MagicDNS and HTTPS are on for the tailnet, the card offers **Use a Tailscale certificate**,
   saying the name goes into public Certificate Transparency logs. The certificate joins the console's selector for the
   unit's ts.net name, as an owner's own does at its names. So `hostAllowed` answers to that name, on 443 for Chat and
   on 9443. tailscaled renews it when it's asked for near expiry. Without it, the ts.net name isn't answered, and the
   tailnet address works with the unit's certificate.
6. **Through Tailscale.** If the owner's page reaches the unit over Tailscale, Turn off and Log out warn that the page
   will lose the unit, as Unraid's do.

What isn't offered: Funnel, which puts a service on the public internet; Serve, as the console serves the ts.net name
itself; exit nodes; subnet routes; Tailscale SSH; Taildrop; the web client. Sharing the owner's LAN through the unit
(a subnet router) could come later.

## Plans and other ways

- **The tailnet is the owner's, and so is its plan.**
  - The free Personal plan has 6 users and unlimited user devices, and is "only suitable for non-commercial use".
  - A unit logged in as a person is one of their devices, and its key expires after 180 days unless they turn that off.
  - Logged in with a tagged key, it's one of 50 tagged devices, and doesn't expire.
- **Headscale**, Tailscale's open-source control server, works through the control-server field. It's a single
  tailnet at v0.29, and has no `tailscale cert`, so a unit there is reached by its address and the unit's own
  certificate, or an owner's own.
- **NetBird or plain WireGuard** would need their own console work, and WireGuard needs a port forwarded.
- **The cloud's relay** (`chat.md`) is still the way for owners who won't run Tailscale, and for the phone apps' push.
  Tailscale doesn't replace it.

## Decisions for Patrick

1. **Tailscale as the owner's own choice**, joined to their tailnet. Keep `decisions/0003` for Wasupu: no Wasupu
   tailnet with units, no Wasupu remote support through it. Reword `console.md`, `chat.md` and `apps.md` (in
   xavier-rogue and patai_docs) to say so.
2. **Build from Tailscale's tagged source** in the image, rather than shipping its `.deb`, for the licence.
3. **Log uploads off** by default, with a switch.
4. **The console on the tailnet too**, not only Chat. Recommended, since the owner controls who's on their tailnet
   and the console has its own login.
5. **Where the card goes:** the System page beside Network settings (recommended), or the Access page.

## Build order, if it goes ahead

1. tailscaled from source in the image build, stripped, without the updater, web client, SSH, Taildrop or Drive.
   Its unit is disabled, with `--statedir=/persist/tailscale`, `--no-logs-no-support` and the operator in its config.
   Notices ship with the image.
2. `patai-rootd` verbs (on, off, sync, the factory reset's logout), and `patai-network` refusing `100.64.0.0/10`.
3. The console's local API client and the card, with tests against a stand-in tailscaled socket.
4. The ts.net certificate in the selector.
5. On patai-dev, with Patrick's tailnet:
   - Chat from a phone on mobile data;
   - tailscaled's memory and CPU;
   - throughput;
   - the stall when an app starts (#22137);
   - a power cut right after login;
   - an update and a rollback keeping the login;
   - a factory reset removing it.

## Sources

- Tailscale docs: install and static builds, tailscaled flags, logging, netfilter and firewall modes, key expiry,
  tags, auth keys, OAuth clients, device approval, HTTPS certificates, Serve, Funnel, the device web interface, and
  pricing: https://tailscale.com/docs, https://tailscale.com/pricing
- Tailscale source, v1.104.7: https://github.com/tailscale/tailscale/tree/v1.104.7. In particular `client/local`,
  `ipn/localapi`, `ipn/ipnstate`, `wgengine/netstack/netstack.go`, `util/linuxfw`, `clientupdate`,
  `feature/featuretags` and `atomicfile`.
- Open issues: https://github.com/tailscale/tailscale/issues/22137 and https://github.com/tailscale/tailscale/issues/12225
- Headscale v0.29.7: https://github.com/juanfont/headscale
- The appliances:
  - Synology and QNAP: `release/dist` in Tailscale's repository.
  - Home Assistant: https://github.com/hassio-addons/app-tailscale
  - Unraid: https://github.com/dkaser/unraid-tailscale
  - TrueNAS: https://github.com/truenas/apps
  - OPNsense: https://github.com/opnsense/plugins
  - pfSense: the FreeBSD-ports package.
  - GL.iNet: https://docs.gl-inet.com/router/en/4/interface_guide/tailscale/
  - Umbrel: https://github.com/getumbrel/umbrel-apps
- The full notes, with a source for each fact, were kept in the session's scratchpad. They are not in this repository.

# Tailscale for the rootfs (docs/console.md, Remote access with Tailscale), built from its tagged source: its BSD
# licence covers a build of our own, where Tailscale's terms govern its packages. One stripped arm64 tailscaled with
# the CLI in it, run as `tailscale` through a link. Without the updater, so a tailnet's "auto-update new devices" can't
# rewrite a slot: it comes with the OS, as everything else does. Without what a unit doesn't offer: Tailscale's web
# client, its SSH, Taildrop, Drive, and the platforms that aren't this one. `docker run` prints the install tree as a
# tar, as docker/rauc.Dockerfile does. go.sum pins the modules.
FROM golang:1.27.4-alpine@sha256:70bd7ddea8b6cbe5d6943cde279d6b67feb10cc924fb9100d1d8757c1575b280
ARG TAILSCALE=1.104.7
COPY vendor/tailscale/tailscale-${TAILSCALE}.tar.gz /src/
RUN tar -C /src -xzf /src/tailscale-${TAILSCALE}.tar.gz \
 && cd /src/tailscale-${TAILSCALE} \
 && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath \
      -tags ts_include_cli,ts_omit_clientupdate,ts_omit_webclient,ts_omit_ssh,ts_omit_taildrop,ts_omit_drive,ts_omit_systray,ts_omit_synology,ts_omit_kube,ts_omit_aws,ts_omit_tap,ts_omit_wakeonlan,ts_omit_capture,ts_omit_flashappliance,ts_omit_networkmanager,ts_omit_relayserver \
      -ldflags "-s -w -X tailscale.com/version.longStamp=${TAILSCALE}-patai -X tailscale.com/version.shortStamp=${TAILSCALE}" \
      -o /out/usr/local/sbin/tailscaled ./cmd/tailscaled \
 && install -d /out/usr/local/bin /out/usr/share/doc/tailscale \
 && ln -s ../sbin/tailscaled /out/usr/local/bin/tailscale \
 && install -m 644 LICENSE PATENTS /out/usr/share/doc/tailscale/ \
 && install -m 644 licenses/tailscale.md /out/usr/share/doc/tailscale/licenses.md
CMD ["tar", "-C", "/out", "-cf", "-", "."]

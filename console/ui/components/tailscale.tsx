"use client"

import QRCode from "qrcode"
import { useCallback, useEffect, useState } from "react"

import { ErrorNote, Field, Muted, Notice, Panel, Row, Spinner } from "@/components/common"
import { CurrentPassword, FormDialog } from "@/components/form-dialog"
import { isOwner, useSession } from "@/components/session"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import { api, plainError } from "@/lib/api"
import { duration, when } from "@/lib/format"
import { field, usePoll } from "@/lib/hooks"
import type { TailscaleStatus } from "@/lib/types"

/** The console's own port, which the tailnet reaches as the LAN does. */
const CONSOLE_PORT = 9443
/** How long before the unit's key runs out the card says so. */
const WARN_DAYS = 30

type Dialog = "on" | "login" | "settings" | "logout" | "off" | null

/** Remote access with Tailscale (docs/console.md): the owner's own tailnet, off until they turn it on. */
export function TailscalePanel() {
  const session = useSession()
  const [status, setStatus] = useState<TailscaleStatus | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [dialog, setDialog] = useState<Dialog>(null)
  const owner = isOwner(session)
  const load = useCallback(async () => {
    try {
      setStatus(await api<TailscaleStatus>("/api/v1/tailscale"))
      setError(null)
    } catch (failure) {
      setError(plainError(failure))
    }
  }, [])
  // Quickly while it waits on something, slowly once it's settled.
  const waiting = !!status && ["starting", "NeedsLogin", "NeedsMachineAuth", "Starting", "NoState"].includes(status.state)
  usePoll(owner ? load : async () => {}, waiting ? 3_000 : 30_000)

  if (!owner || (status && !status.available)) return null
  const send = async (path: string, form: FormData, extra: Record<string, unknown> = {}) => {
    setStatus(
      await api<TailscaleStatus>(path, {
        method: path.endsWith("settings") ? "PUT" : "POST",
        body: { current_password: field(form, "current_password"), ...extra },
        timeoutMs: 90_000,
      }),
    )
    setDialog(null)
  }
  // The page itself may be on the tailnet, and would lose the unit.
  const throughTailnet =
    !!status &&
    (window.location.hostname === status.name ||
      status.addresses.some((address) => window.location.hostname.replace(/^\[|\]$/g, "") === address))
  const losesPage = throughTailnet ? " You're reaching this page through Tailscale, so it will lose the unit." : ""

  return (
    <Panel id="tailscale" title="Remote access">
      <ErrorNote>{error}</ErrorNote>
      {!status && !error && <Spinner />}
      {status && !status.on && (
        <>
          <Muted>Reach Chat and this console from your own devices anywhere, through your Tailscale account.</Muted>
          <div>
            <Button variant="outline" onClick={() => setDialog("on")}>
              Turn on Tailscale
            </Button>
          </div>
        </>
      )}
      {status?.on && <State status={status} onLogin={() => setDialog("login")} />}
      {status?.on && (
        <div className="flex flex-wrap gap-2">
          {status.state === "Running" && (
            <Button variant="outline" onClick={() => setDialog("settings")}>
              Settings
            </Button>
          )}
          {status.state === "Running" && (
            <Button variant="outline" onClick={() => setDialog("logout")}>
              Log out
            </Button>
          )}
          <Button variant="outline" onClick={() => setDialog("off")}>
            Turn off
          </Button>
        </div>
      )}

      <FormDialog
        open={dialog === "on"}
        onOpenChange={(open) => setDialog(open ? "on" : null)}
        title="Turn on Tailscale?"
        description="The unit joins your tailnet once you log in. Tailscale's servers see its name and when it's online, never what's said in Chat."
        submit="Turn on"
        action={(form) => send("/api/v1/tailscale/on", form)}
      >
        <CurrentPassword />
      </FormDialog>
      {status && (
        <LoginDialog
          open={dialog === "login"}
          onOpenChange={(open) => setDialog(open ? "login" : null)}
          action={(form) =>
            send("/api/v1/tailscale/login", form, {
              auth_key: field(form, "auth_key"),
              control_url: field(form, "control_url"),
            })
          }
        />
      )}
      {status && dialog === "settings" && (
        <SettingsDialog status={status} onClose={() => setDialog(null)} send={send} />
      )}
      <FormDialog
        open={dialog === "logout"}
        onOpenChange={(open) => setDialog(open ? "logout" : null)}
        title="Log out of Tailscale?"
        description={`The unit leaves your tailnet, and joins again only with a new login.${losesPage}`}
        submit="Log out"
        destructive
        action={(form) => send("/api/v1/tailscale/logout", form)}
      >
        <CurrentPassword />
      </FormDialog>
      <FormDialog
        open={dialog === "off"}
        onOpenChange={(open) => setDialog(open ? "off" : null)}
        title="Turn off Tailscale?"
        description={`The login is kept for when it's turned on again.${losesPage}`}
        submit="Turn off"
        destructive
        action={(form) => send("/api/v1/tailscale/off", form)}
      >
        <CurrentPassword />
      </FormDialog>
    </Panel>
  )
}

function State({ status, onLogin }: { status: TailscaleStatus; onLogin: () => void }) {
  switch (status.state) {
    case "starting":
    case "Starting":
    case "NoState":
      return <Muted>Starting…</Muted>
    case "NeedsLogin":
      return status.login_url ? (
        <LoginLink url={status.login_url} onAgain={onLogin} />
      ) : (
        <>
          <Muted>Log in with your Tailscale account, or an auth key from your tailnet.</Muted>
          <div>
            <Button onClick={onLogin}>Log in</Button>
          </div>
        </>
      )
    case "NeedsMachineAuth":
      return (
        <Notice level="warn" title="Waiting for approval">
          Your tailnet approves new devices. Approve this one in{" "}
          <a href="https://login.tailscale.com/admin/machines" target="_blank" rel="noreferrer" className="underline">
            Tailscale&apos;s admin console
          </a>
          .
        </Notice>
      )
    case "Running":
      return <Running status={status} />
    default:
      return (
        <>
          <Muted>Tailscale is {status.state.toLowerCase()}.</Muted>
          <div>
            <Button variant="outline" onClick={onLogin}>
              Log in
            </Button>
          </div>
        </>
      )
  }
}

/** The login URL, as a link and a QR code for signing in on a phone. */
function LoginLink({ url, onAgain }: { url: string; onAgain: () => void }) {
  const [qr, setQr] = useState<string | null>(null)
  useEffect(() => {
    let current = true
    QRCode.toDataURL(url, { margin: 1, width: 176 })
      .then((data) => current && setQr(data))
      .catch(() => current && setQr(null))
    return () => {
      current = false
    }
  }, [url])
  return (
    <div className="grid gap-3">
      <Muted>Open this on a device signed in to your Tailscale account.</Muted>
      <div className="flex flex-wrap items-center gap-4">
        {qr && <img src={qr} alt="QR code of the login link" width={176} height={176} className="rounded-md border" />}
        <div className="grid gap-2">
          <a href={url} target="_blank" rel="noreferrer" className="break-all underline underline-offset-4">
            {url}
          </a>
          <div>
            <Button variant="outline" size="sm" onClick={onAgain}>
              Use an auth key instead
            </Button>
          </div>
        </div>
      </div>
    </div>
  )
}

function Running({ status }: { status: TailscaleStatus }) {
  const left = status.key_expiry ? (Date.parse(status.key_expiry) - Date.now()) / 1000 : null
  const name = status.name
  return (
    <>
      <dl className="grid gap-1.5">
        {status.tailnet && <Row label="Tailnet">{status.tailnet}</Row>}
        {name && (
          <Row label="Chat">
            <a href={`https://${name}/`} className="font-mono break-all underline underline-offset-4">
              {name}
            </a>
          </Row>
        )}
        {name && (
          <Row label="Console">
            <a href={`https://${name}:${CONSOLE_PORT}/`} className="font-mono break-all underline underline-offset-4">
              {name}:{CONSOLE_PORT}
            </a>
          </Row>
        )}
        <Row label="Addresses">
          <span className="grid gap-0.5 font-mono text-xs">
            {status.addresses.map((address) => (
              <span key={address}>{address}</span>
            ))}
          </span>
        </Row>
        <Row label="Key expires">{status.key_expiry ? when(status.key_expiry) : "Never"}</Row>
        <Row label="Certificate">
          {status.certificate.in_use
            ? `Tailscale's, until ${when(status.certificate.not_after)}`
            : status.certificate.on
              ? "Not yet"
              : "The unit's own"}
        </Row>
      </dl>
      {status.expired && (
        <Notice level="bad" title="The unit's key has run out">
          Log in again, or extend it in Tailscale&apos;s admin console.
        </Notice>
      )}
      {!status.expired && left !== null && left < WARN_DAYS * 86_400 && (
        <Notice level="warn" title={`The unit's key runs out in ${duration(left)}`}>
          Turn off key expiry for this unit in Tailscale&apos;s admin console, or it leaves the tailnet then.
        </Notice>
      )}
      {status.certificate.on && status.certificate.problem && (
        <Notice level="warn">{status.certificate.problem}</Notice>
      )}
      {status.health.map((message) => (
        <Notice key={message} level="warn">
          {message}
        </Notice>
      ))}
    </>
  )
}

function LoginDialog({
  open,
  onOpenChange,
  action,
}: {
  open: boolean
  onOpenChange: (open: boolean) => void
  action: (form: FormData) => Promise<void>
}) {
  return (
    <FormDialog
      open={open}
      onOpenChange={onOpenChange}
      title="Log in to Tailscale"
      description="Without an auth key, the unit shows a link to log in with."
      submit="Log in"
      action={action}
    >
      <Field
        label="Auth key"
        name="auth_key"
        autoComplete="off"
        hint="Optional. From your tailnet's admin console. Used once, not kept."
      />
      <details className="text-sm">
        <summary className="cursor-pointer text-muted-foreground">More</summary>
        <div className="mt-3">
          <Field
            label="Control server"
            name="control_url"
            placeholder="https://headscale.example.com"
            hint="Your own, such as Headscale's. Empty for Tailscale's."
          />
        </div>
      </details>
      <CurrentPassword />
    </FormDialog>
  )
}

function SettingsDialog({
  status,
  onClose,
  send,
}: {
  status: TailscaleStatus
  onClose: () => void
  send: (path: string, form: FormData, extra: Record<string, unknown>) => Promise<void>
}) {
  const [certificate, setCertificate] = useState(status.certificate.on)
  const [logs, setLogs] = useState(status.logs)
  return (
    <FormDialog
      open
      onOpenChange={(open) => !open && onClose()}
      title="Tailscale settings"
      submit="Save"
      action={(form) => send("/api/v1/tailscale/settings", form, { certificate, logs })}
    >
      <label className="flex items-start gap-2 text-sm">
        <Checkbox checked={certificate} onCheckedChange={(checked) => setCertificate(checked === true)} />
        <span className="grid gap-0.5">
          <span>Use a Tailscale certificate</span>
          <span className="text-muted-foreground">
            No browser warning at {status.name ?? "the unit's tailnet name"}. Needs MagicDNS and HTTPS on for your
            tailnet. The name goes into public certificate logs.
          </span>
        </span>
      </label>
      <label className="flex items-start gap-2 text-sm">
        <Checkbox checked={logs} onCheckedChange={(checked) => setLogs(checked === true)} />
        <span className="grid gap-0.5">
          <span>Send logs to Tailscale</span>
          <span className="text-muted-foreground">
            For Tailscale&apos;s support, and tailnets that require network logs. Restarts Tailscale.
          </span>
        </span>
      </label>
      <CurrentPassword />
    </FormDialog>
  )
}

"use client"

import { useCallback, useEffect, useRef, useState } from "react"

import { Choice } from "@/components/choice"
import { ErrorNote, Field, Muted, Notice, Panel, Row } from "@/components/common"
import { CurrentPassword, FormDialog } from "@/components/form-dialog"
import { useIdentity } from "@/components/gate"
import { isOwner, useSession } from "@/components/session"
import { Button } from "@/components/ui/button"
import { api, ApiError, plainError } from "@/lib/api"
import { field, usePoll } from "@/lib/hooks"
import { networkSummary } from "@/lib/labels"
import type { NetworkLast, NetworkPort, NetworkStatus, NetworkTrial } from "@/lib/types"

/** Sent when this page starts a change, so the banner looks at once. */
const NETWORK_EVENT = "patai:network"
const PORTS_EVERY_MS = 15_000
const TRIAL_EVERY_MS = 3_000
const RESTING_MS = 30_000

const MODES = [
  { value: "dhcp", label: "DHCP", hint: "The network gives the address. This is how a new unit starts." },
  { value: "static", label: "Static", hint: "An address you choose, for a network with no DHCP or a fixed address." },
]

function clock(seconds: number): string {
  const left = Math.max(0, Math.ceil(seconds))
  return `${Math.floor(left / 60)}:${String(left % 60).padStart(2, "0")}`
}

function cable(port: NetworkPort): string {
  if (!port.carrier) return "Not connected"
  const speed = port.speed_mbps
  if (!speed) return "Connected"
  return `Connected, ${speed >= 1000 ? `${speed / 1000} Gb/s` : `${speed} Mb/s`}`
}

function portLabel(status: NetworkStatus | null, id: string): string {
  return status?.ports.find((port) => port.id === id)?.label ?? id
}

/** Whether the request came in on this port, as far as the unit can say. */
function reachedThrough(port: NetworkPort, via: string): boolean {
  return port.addresses.some((address) => address.split("/")[0] === via) || port.link_local === via
}

export function NetworkPorts() {
  const session = useSession()
  const [status, setStatus] = useState<NetworkStatus | null>(null)
  const [error, setError] = useState<string | null>(null)
  const load = useCallback(async () => {
    try {
      setStatus(await api<NetworkStatus>("/api/v1/network"))
      setError(null)
    } catch (failure) {
      setError(plainError(failure))
    }
  }, [])
  usePoll(load, PORTS_EVERY_MS)
  useEffect(() => {
    window.addEventListener(NETWORK_EVENT, load)
    return () => window.removeEventListener(NETWORK_EVENT, load)
  }, [load])

  if (!status) {
    return (
      <Panel title="Network">
        {error ? <ErrorNote>{error}</ErrorNote> : <Muted>Looking at the ports…</Muted>}
      </Panel>
    )
  }
  return (
    <>
      {status.problem && (
        <Notice level="warn" title="Saved settings not in use" className="md:col-span-2">
          {status.problem}
        </Notice>
      )}
      {status.ports.map((port, index) => (
        <PortPanel key={port.id} port={port} status={status} owner={isOwner(session)} first={index === 0} />
      ))}
    </>
  )
}

/** The first port is where a link to the unit's network lands. */
function PortPanel({
  port,
  status,
  owner,
  first,
}: {
  port: NetworkPort
  status: NetworkStatus
  owner: boolean
  first: boolean
}) {
  const [editing, setEditing] = useState(false)
  const trying = status.trial?.port === port.id ? status.trial : null
  const here = status.via !== "" && reachedThrough(port, status.via)
  return (
    <Panel
      id={first ? "network" : `port-${port.id}`}
      title={
        <>
          {port.label} <span className="font-normal text-muted-foreground">({port.connector})</span>
        </>
      }
      action={
        owner &&
        port.interface && (
          <Button variant="outline" size="sm" onClick={() => setEditing(true)} disabled={status.trial !== null}>
            Change
          </Button>
        )
      }
    >
      {!port.interface ? (
        <Muted>This unit doesn&apos;t have this port.</Muted>
      ) : (
        <dl className="grid gap-1.5">
          <Row label="Cable">{cable(port)}</Row>
          <Row label="Set to">{networkSummary(trying ? trying.setting : port.saved)}</Row>
          <Row label="Address">
            {port.addresses.length > 0 ? (
              <span className="font-mono text-sm">{port.addresses.join(" ")}</span>
            ) : port.link_local ? (
              <span className="font-mono text-sm">{port.link_local} (link-local only)</span>
            ) : (
              "None"
            )}
          </Row>
          {port.gateway && (
            <Row label="Gateway">
              <span className="font-mono text-sm">{port.gateway}</span>
            </Row>
          )}
          {port.dns.length > 0 && (
            <Row label="DNS">
              <span className="font-mono text-sm">{port.dns.join(" ")}</span>
            </Row>
          )}
          <Row label="MAC">
            <span className="font-mono text-xs">{port.mac}</span>
          </Row>
        </dl>
      )}
      {here && <Muted>You&apos;re connected to the unit through this port.</Muted>}
      {trying && <Muted>A change is being tried on this port. Keep it or undo it at the top of the page.</Muted>}
      {editing && (
        <EditPort port={port} trialSeconds={status.trial_s} here={here} onClose={() => setEditing(false)} />
      )}
    </Panel>
  )
}

function EditPort({
  port,
  trialSeconds,
  here,
  onClose,
}: {
  port: NetworkPort
  trialSeconds: number
  here: boolean
  onClose: () => void
}) {
  const [mode, setMode] = useState(port.saved.mode)
  const minutes = Math.round(trialSeconds / 60)
  return (
    <FormDialog
      open
      onOpenChange={(open) => !open && onClose()}
      title={`Change ${port.label}`}
      description={
        <>
          The unit tries it first, and goes back to how it is now unless you keep it within {minutes} minutes.
          {here &&
            " You're connected through this port, so if its address changes, this page loses the unit and you keep the change from the new address."}
        </>
      }
      submit="Try it"
      action={async (form) => {
        const dns = field(form, "dns")
          .split(/[\s,]+/)
          .filter(Boolean)
        const body =
          mode === "static"
            ? { mode, address: field(form, "address").trim(), gateway: field(form, "gateway").trim(), dns }
            : { mode, dns }
        const trial = await api<NetworkTrial>(`/api/v1/network/${port.id}`, {
          method: "PUT",
          body: { ...body, current_password: field(form, "current_password") },
        })
        onClose()
        // The trial goes with it: a look could lose the race with the switch, and leave this page without
        // the new address to send its owner to.
        window.dispatchEvent(new CustomEvent<NetworkTrial>(NETWORK_EVENT, { detail: trial }))
      }}
    >
      <Choice
        name="mode"
        label="Address"
        options={MODES}
        defaultValue={port.saved.mode}
        onChange={(value) => setMode(value === "static" ? "static" : "dhcp")}
      />
      {mode === "static" && (
        <>
          <Field
            label="Address and prefix length"
            name="address"
            defaultValue={port.saved.address ?? ""}
            placeholder="192.168.1.20/24"
            hint="/24 is a mask of 255.255.255.0."
            inputMode="decimal"
            autoCapitalize="none"
            autoCorrect="off"
            required
          />
          <Field
            label="Gateway"
            name="gateway"
            defaultValue={port.saved.gateway ?? ""}
            placeholder="192.168.1.1"
            hint="Leave it empty on a network that doesn't lead to the internet."
            inputMode="decimal"
            autoCapitalize="none"
            autoCorrect="off"
          />
        </>
      )}
      <Field
        key={mode}
        label="DNS servers"
        name="dns"
        defaultValue={mode === port.saved.mode ? port.saved.dns.join(" ") : ""}
        placeholder={mode === "static" ? "192.168.1.1" : "From DHCP"}
        hint={
          mode === "static" ? "Up to 3, separated by spaces." : "Leave it empty to use the ones DHCP gives. Up to 3."
        }
        inputMode="decimal"
        autoCapitalize="none"
        autoCorrect="off"
      />
      <CurrentPassword />
    </FormDialog>
  )
}

/**
 * A change being tried, from any page, for owners. It follows the change when this page loses the unit, and
 * says how it ended.
 */
export function NetworkTrialBanner() {
  const { identity } = useIdentity()
  const [status, setStatus] = useState<NetworkStatus | null>(null)
  const [trial, setTrial] = useState<NetworkTrial | null>(null)
  const [deadline, setDeadline] = useState(0)
  const [lost, setLost] = useState(false)
  const [ended, setEnded] = useState<NetworkLast | null>(null)
  const [now, setNow] = useState(() => Date.now())
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const seen = useRef<string | null>(null)
  // Goes up when this page starts a trial, so an answer to a question asked before then is ignored.
  const turn = useRef(0)

  const load = useCallback(async () => {
    const asked = turn.current
    try {
      const next = await api<NetworkStatus>("/api/v1/network", { timeoutMs: 8_000 })
      if (asked !== turn.current) return
      setStatus(next)
      setLost(false)
      setTrial(next.trial)
      if (next.trial) {
        seen.current = next.trial.id
        setDeadline(Date.now() + next.trial.left_s * 1000)
      } else if (seen.current) {
        if (next.last?.id === seen.current) setEnded(next.last)
        seen.current = null
      }
    } catch (failure) {
      if (asked === turn.current && seen.current && failure instanceof ApiError && failure.status === 0) setLost(true)
    }
  }, [])
  usePoll(load, trial || lost ? TRIAL_EVERY_MS : RESTING_MS)
  useEffect(() => {
    function started(event: Event) {
      const started = (event as CustomEvent<NetworkTrial | undefined>).detail
      if (started) {
        turn.current += 1
        seen.current = started.id
        setTrial(started)
        setDeadline(Date.now() + started.left_s * 1000)
      }
      void load()
    }
    window.addEventListener(NETWORK_EVENT, started)
    return () => window.removeEventListener(NETWORK_EVENT, started)
  }, [load])
  useEffect(() => {
    if (!trial) return
    const timer = setInterval(() => setNow(Date.now()), 1000)
    return () => clearInterval(timer)
  }, [trial])

  async function decide(verb: "keep" | "undo") {
    if (!trial) return
    setBusy(true)
    setError(null)
    try {
      await api(`/api/v1/network/${verb}`, { method: "POST", body: { id: trial.id } })
      await load()
    } catch (failure) {
      setError(plainError(failure))
    } finally {
      setBusy(false)
    }
  }

  const left = (deadline - now) / 1000
  if (trial && lost) {
    return (
      <Notice level="warn" title="This page lost the unit">
        <div className="grid gap-2">
          <p>
            That&apos;s expected if its address changed. To keep the change, open the unit at its new address, log in and
            keep it there {left > 0 ? `within ${clock(left)}` : "now"}:
          </p>
          <ul className="grid gap-1">
            {trial.urls.map((url) => (
              <li key={url}>
                <a href={url} className="font-mono break-all underline underline-offset-4">
                  {url}
                </a>
              </li>
            ))}
          </ul>
          {identity.fingerprint && (
            <p>
              Your browser will warn about the certificate there too. It&apos;s the unit if the fingerprint is{" "}
              <code className="font-mono text-xs break-all">{identity.fingerprint}</code>.
            </p>
          )}
          <p>
            {left > 0
              ? "If nobody keeps it, the unit goes back by itself, and this page carries on."
              : "If it was kept, the unit is at the new address. If not, it's going back, and this page carries on when it answers."}
          </p>
        </div>
      </Notice>
    )
  }
  if (trial) {
    const port = status?.ports.find((item) => item.id === trial.port)
    const elsewhere = Boolean(port && status?.via && !reachedThrough(port, status.via))
    return (
      <Notice level="warn" title={`Keep the change to ${port?.label ?? trial.port}?`}>
        <div className="grid gap-2">
          <p>
            {networkSummary(trial.setting)}.{" "}
            {trial.reverting
              ? "The unit is going back to how it was."
              : trial.applied
                ? `Unless you keep it, the unit goes back to how it was in ${clock(left)}.`
                : "The unit is making the change."}
          </p>
          {trial.applied && elsewhere && (
            <p>
              You&apos;re reaching the unit through another port, so this page doesn&apos;t show that {port?.label} works.
            </p>
          )}
          <ErrorNote>{error}</ErrorNote>
          <div className="flex flex-wrap gap-2">
            <Button onClick={() => void decide("keep")} disabled={busy || !trial.applied || trial.reverting}>
              Keep it
            </Button>
            <Button variant="outline" onClick={() => void decide("undo")} disabled={busy}>
              Undo now
            </Button>
          </div>
        </div>
      </Notice>
    )
  }
  if (ended) {
    const label = portLabel(status, ended.port)
    const words = {
      kept: { level: "good" as const, text: `The change to ${label} is kept.` },
      undone: { level: "unknown" as const, text: `The change to ${label} was undone.` },
      "went back": { level: "warn" as const, text: `Nobody kept the change to ${label}, so the unit went back to how it was.` },
    }[ended.outcome]
    return (
      <Notice level={words.level} title={words.text}>
        <Button variant="outline" size="sm" onClick={() => setEnded(null)}>
          Dismiss
        </Button>
      </Notice>
    )
  }
  return null
}

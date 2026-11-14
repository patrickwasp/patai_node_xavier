"use client"

import { useCallback, useEffect, useRef, useState } from "react"

import { ErrorNote, Field, Muted, Notice, Panel, Row, Spinner } from "@/components/common"
import { CurrentPassword, FormDialog } from "@/components/form-dialog"
import { useIdentity } from "@/components/gate"
import { Picker } from "@/components/picker"
import { RestartButton } from "@/components/restart"
import { canOperate, isOwner, useSession } from "@/components/session"
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
  AlertDialogTrigger,
} from "@/components/ui/alert-dialog"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import { Label } from "@/components/ui/label"
import { api, plainError, startJob } from "@/lib/api"
import { duration } from "@/lib/format"
import { field, sleep, usePoll } from "@/lib/hooks"
import { powerLabel } from "@/lib/labels"
import type { ClockStatus, Identity, Job, PowerModeInfo, PowerStatus } from "@/lib/types"

const CLOCK_EVERY_MS = 30_000
/** Closer than this, the two clocks agree as far as a page can tell. */
const SAME_CLOCK_MS = 2_000

// From the bench (docs/console.md, Shutting down). The console stops after Docker, and from then until the
// kernel halts, a connection is refused at once. The moment nothing answers at all is when it halted, and
// the watchdog starts it again about 150 s after that.
const PROBE_MS = 500
const PROBE_TIMEOUT_MS = 2_000
const REFUSED_WITHIN_MS = 1_000
/** Looks in a row it takes to believe a refusal or a silence: one lost packet mustn't say unplug. */
const SURE = 2
const UNPLUG_BY_S = 120
/** With no refusals seen, how long after the console went to say unplug anyway. */
const UNSURE_WAIT_S = 30

function fullTime(ms: number): string {
  return new Intl.DateTimeFormat(undefined, { dateStyle: "medium", timeStyle: "medium" }).format(new Date(ms))
}

function clockTime(ms: number): string {
  return new Intl.DateTimeFormat(undefined, { timeStyle: "medium" }).format(new Date(ms))
}

function countdown(seconds: number): string {
  const left = Math.max(0, Math.ceil(seconds))
  return `${Math.floor(left / 60)}:${String(left % 60).padStart(2, "0")}`
}

function difference(ms: number): string {
  if (Math.abs(ms) < SAME_CLOCK_MS) return "Matches your browser."
  return `${duration(Math.abs(ms) / 1000)} ${ms > 0 ? "ahead of" : "behind"} your browser.`
}

/** Ticks once a second while it's shown. */
function useNow(): number {
  const [now, setNow] = useState(() => Date.now())
  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), 1_000)
    return () => clearInterval(timer)
  }, [])
  return now
}

export function ClockPanel() {
  const session = useSession()
  const [clock, setClock] = useState<ClockStatus | null>(null)
  // How far the unit's clock is from this browser's, measured from the middle of the request.
  const [offset, setOffset] = useState(0)
  const [error, setError] = useState<string | null>(null)
  const [asking, setAsking] = useState(false)
  const [unkept, setUnkept] = useState<string | null>(null)
  const now = useNow()

  const take = useCallback((status: ClockStatus, sent: number) => {
    setClock(status)
    setOffset(status.now_ms - (sent + Date.now()) / 2)
  }, [])
  const load = useCallback(async () => {
    const sent = Date.now()
    try {
      take(await api<ClockStatus>("/api/v1/clock"), sent)
      setError(null)
    } catch (failure) {
      setError(plainError(failure))
    }
  }, [take])
  usePoll(load, CLOCK_EVERY_MS)

  const source = !clock
    ? null
    : clock.synced
      ? `A time server${clock.server ? `, ${clock.server}` : ""}`
      : clock.set
        ? `An owner, ${duration(clock.set.ago_s)} ago`
        : "Nothing since the unit started"

  return (
    <Panel id="clock" title="Clock">
      <ErrorNote>{error}</ErrorNote>
      {clock && (
        <>
          <dl className="grid gap-1.5">
            <Row label="Unit's time">
              <span className="tabular-nums">{fullTime(now + offset)}</span>
            </Row>
            <Row label="Set by">{source}</Row>
          </dl>
          <Muted>{difference(offset)}</Muted>
          {!clock.synced && clock.rtc_lost && (
            <Notice level="warn" title="The unit lost the time when it last lost power">
              Its clock battery is missing or flat, and no time server answers.
            </Notice>
          )}
          {unkept && <Notice level="warn">The unit&apos;s clock is set, but {unkept}, so a power cut may lose it.</Notice>}
          {clock.synced ? null : isOwner(session) ? (
            <Button variant="outline" onClick={() => setAsking(true)}>
              Set it to this browser&apos;s time
            </Button>
          ) : (
            <Muted>No time server answers. An owner can set it.</Muted>
          )}
        </>
      )}
      <FormDialog
        open={asking}
        onOpenChange={setAsking}
        title="Set the unit's clock?"
        description={
          <>
            To your browser&apos;s time, {fullTime(now)}, so check that it&apos;s right. If a time server answers the unit
            later, it takes over.
          </>
        }
        submit="Set the clock"
        action={async (form) => {
          const sent = Date.now()
          const status = await api<ClockStatus>("/api/v1/clock", {
            method: "PUT",
            body: { time_ms: Date.now(), current_password: field(form, "current_password") },
          })
          take(status, sent)
          const lost = [
            status.rtc_written === false && "its battery-backed clock didn't take it",
            status.floor_saved === false && "the unit couldn't save it",
          ].filter(Boolean)
          setUnkept(lost.length > 0 ? lost.join(" and ") : null)
          setAsking(false)
        }}
      >
        <CurrentPassword />
      </FormDialog>
    </Panel>
  )
}

function modeHint(mode: PowerModeInfo): string {
  if (!mode.cpu_mhz && !mode.gpu_mhz) return `${mode.cores} cores and graphics, with no limits`
  const cpu = mode.cpu_mhz ? ` up to ${(mode.cpu_mhz / 1000).toFixed(1)} GHz` : ""
  const gpu = mode.gpu_mhz ? `${mode.gpu_mhz} MHz` : "no limit"
  return `${mode.cores} cores${cpu}, graphics ${gpu}`
}

/** Lowest power first: by the watts in its name, with Maximum last, then by cores and clock. */
function byPower(a: PowerModeInfo, b: PowerModeInfo): number {
  const watts = (mode: PowerModeInfo) => (mode.name === "MAXN" ? Infinity : Number(/(\d+)W/.exec(mode.name)?.[1] ?? Infinity))
  return watts(a) - watts(b) || a.cores - b.cores || (a.cpu_mhz ?? Infinity) - (b.cpu_mhz ?? Infinity)
}

function modeName(mode: PowerModeInfo, defaultId: number | null): string {
  return mode.id === defaultId ? `${powerLabel(mode.name)} (the default)` : powerLabel(mode.name)
}

/** Power mode, restarting and shutting down, together. */
export function PowerPanel({
  onRestart,
  restartError,
  onShutdown,
  shutdownError,
}: {
  onRestart: () => void
  restartError: string | null
  onShutdown: () => void
  shutdownError: string | null
}) {
  const session = useSession()
  const [status, setStatus] = useState<PowerStatus | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [asking, setAsking] = useState(false)
  const [chosen, setChosen] = useState<string | null>(null)
  const load = useCallback(async () => {
    try {
      setStatus(await api<PowerStatus>("/api/v1/power-mode"))
      setError(null)
    } catch (failure) {
      setError(plainError(failure))
    }
  }, [])
  useEffect(() => {
    void load()
  }, [load])

  const current = status?.modes.find((mode) => mode.id === status.current)
  const kept = status?.modes.find((mode) => mode.id === status.saved)
  const pick = status?.modes.find((mode) => String(mode.id) === chosen)
  return (
    <Panel id="power" title="Power">
      <ErrorNote>{error}</ErrorNote>
      <ErrorNote>{restartError}</ErrorNote>
      <ErrorNote>{shutdownError}</ErrorNote>
      {status && (
        <dl className="grid gap-1.5">
          <Row label="Power mode">{current ? modeName(current, status.default) : "Not known"}</Row>
          {kept && kept.id !== current?.id && <Row label="At the next start">{modeName(kept, status.default)}</Row>}
        </dl>
      )}
      {current && <Muted>{modeHint(current)}</Muted>}
      {canOperate(session) ? (
        <div className="flex flex-wrap gap-2">
          <RestartButton label="Restart" title="Restart the unit?" onConfirm={onRestart}>
            Apps stop, and the console is away for about a minute.
          </RestartButton>
          <ShutdownButton onConfirm={onShutdown} />
          {isOwner(session) && status && (
            <Button
              variant="outline"
              onClick={() => {
                setChosen(String(status.current ?? status.default ?? ""))
                setAsking(true)
              }}
            >
              Change power mode
            </Button>
          )}
        </div>
      ) : (
        <Muted>Your role can&apos;t restart or shut down the unit.</Muted>
      )}
      <FormDialog
        open={asking}
        onOpenChange={setAsking}
        title="Change the power mode?"
        description="It changes at once, and stays."
        submit="Change it"
        action={async (form) => {
          setStatus(
            await api<PowerStatus>("/api/v1/power-mode", {
              method: "PUT",
              body: {
                mode: Number(field(form, "mode")),
                supply_checked: field(form, "supply_checked") !== "",
                current_password: field(form, "current_password"),
              },
            })
          )
          setAsking(false)
        }}
      >
        {status && (
          <Picker
            name="mode"
            label="Power mode"
            items={[...status.modes].sort(byPower).map((mode) => ({
              value: String(mode.id),
              label: modeName(mode, status.default),
              detail: modeHint(mode),
            }))}
            defaultValue={chosen ?? ""}
            onChange={setChosen}
          />
        )}
        {pick?.name === "MAXN" && (
          <Notice level="warn" title="Maximum needs a supply of 15 V or more">
            <div className="grid gap-2">
              <p>On a 12 V supply the unit can reset under load. It can&apos;t measure its own supply.</p>
              <Label className="items-center gap-3 leading-snug">
                <Checkbox name="supply_checked" required />
                This unit&apos;s supply gives 15 V or more
              </Label>
            </div>
          </Notice>
        )}
        <CurrentPassword />
      </FormDialog>
    </Panel>
  )
}

/** Whether this page reached the unit by a name, which stops working when the name changes. */
function byName(identity: Identity): boolean {
  const host = window.location.hostname.toLowerCase()
  return host === identity.name.toLowerCase() || host === `${identity.name.toLowerCase()}.local`
}

export function RenameButton({ onRenamed }: { onRenamed: (name: string, job: Job) => void }) {
  const { identity } = useIdentity()
  const [asking, setAsking] = useState(false)
  return (
    <>
      <Button variant="outline" size="sm" onClick={() => setAsking(true)}>
        Rename
      </Button>
      <FormDialog
        open={asking}
        onOpenChange={setAsking}
        title="Rename this unit?"
        description="The unit restarts to take the new name, which stops any app for about a minute, and everyone logs in again."
        submit="Rename and restart"
        action={async (form) => {
          const name = field(form, "name").trim().toLowerCase()
          const job = await startJob("/api/v1/rename", { name, current_password: field(form, "current_password") })
          setAsking(false)
          onRenamed(name, job)
        }}
      >
        <Field
          label="New name"
          name="name"
          defaultValue={identity.name}
          autoComplete="off"
          autoCapitalize="none"
          spellCheck={false}
          className="font-mono"
          hint="Lower-case letters, digits and hyphens. It's also the unit's address on this network, name.local."
          required
        />
        <CurrentPassword />
      </FormDialog>
    </>
  )
}

/** What the restart overlay says while a rename takes effect. */
export function Renaming({ name }: { name: string }) {
  const { identity } = useIdentity()
  const url = `https://${name}.local/`
  return (
    <>
      {byName(identity) ? (
        <>
          This page uses the old name, so it can&apos;t follow the unit. In about a minute, open{" "}
          <a href={url} className="font-mono underline underline-offset-4">
            {url}
          </a>{" "}
          and log in there.
        </>
      ) : (
        <>It comes back as {name}. This page will move on by itself, and you&apos;ll log in again.</>
      )}{" "}
      {identity.fingerprint && (
        <>
          The certificate stays the same, so your browser may warn about the name: it&apos;s the unit if the fingerprint
          is <code className="font-mono text-xs break-all">{identity.fingerprint}</code>.
        </>
      )}
    </>
  )
}

type Stage = "stopping" | "finishing" | "unplug" | "unsure" | "restarting" | "back"
type Probe = "answers" | "refused" | "silent"

/** One look at the unit. Any reply is the console; a refusal comes back at once, from a kernel with nothing listening. */
async function probe(): Promise<Probe> {
  const sent = Date.now()
  try {
    await fetch("/api/identity", { cache: "no-store", signal: AbortSignal.timeout(PROBE_TIMEOUT_MS) })
    return "answers"
  } catch {
    return Date.now() - sent < REFUSED_WITHIN_MS ? "refused" : "silent"
  }
}

/**
 * Shuts the unit down and says when to unplug it. A refusal after the console stops means the kernel is still
 * finishing; silence after that means it has halted. Seeing only silence, the page can't tell a halt from
 * losing the network, and says so.
 */
export function useShutdown() {
  const [stage, setStage] = useState<Stage | null>(null)
  const [since, setSince] = useState<number | null>(null)
  const [error, setError] = useState<string | null>(null)
  const alive = useRef(true)
  useEffect(() => {
    alive.current = true
    return () => {
      alive.current = false
    }
  }, [])

  async function shutdown() {
    setError(null)
    setStage("stopping")
    let job: Job
    try {
      job = await startJob("/api/v1/shutdown")
    } catch (failure) {
      setStage(null)
      setError(plainError(failure))
      return
    }
    // Refusals since it last answered, and silences in a row.
    let refusals = 0
    let quiet = 0
    let quietSince = 0
    let gone: number | null = null
    while (alive.current) {
      await sleep(PROBE_MS)
      const sent = Date.now()
      const seen = await probe()
      if (gone !== null) {
        // Once it has gone quiet, a quick failure is the network forgetting the unit, and only an answer counts.
        if (seen === "answers") {
          setStage("back")
          return
        }
        if (Date.now() - gone > UNPLUG_BY_S * 1000) setStage("restarting")
        continue
      }
      if (seen === "answers") {
        refusals = 0
        quiet = 0
        // Still stopping workloads, unless the helper said no.
        const current = await api<Job>(`/api/v1/jobs/${job.id}`, { timeoutMs: PROBE_TIMEOUT_MS }).catch(() => null)
        if (current?.state === "failed") {
          setStage(null)
          setError(current.result ? `The unit couldn't shut down: ${current.result}` : "The unit couldn't shut down.")
          return
        }
        continue
      }
      if (seen === "refused") {
        refusals += 1
        quiet = 0
        if (refusals === SURE) setStage("finishing")
        continue
      }
      if (quiet === 0) quietSince = sent
      quiet += 1
      if (quiet < SURE) continue
      gone = quietSince
      setSince(gone)
      setStage(refusals >= SURE ? "unplug" : "unsure")
    }
  }

  return { stage, since, error, shutdown }
}

export function ShuttingDown({ stage, since }: { stage: Stage; since: number | null }) {
  const now = useNow()
  const gone = since === null ? 0 : Math.max(0, (now - since) / 1000)
  // The page re-renders each second, which can be ahead of the stage the last look set.
  const shown: Stage = (stage === "unplug" || stage === "unsure") && gone >= UNPLUG_BY_S ? "restarting" : stage
  const until = since === null ? 0 : since + UNPLUG_BY_S * 1000
  const deadline = `Unplug it by ${clockTime(until)}, in ${countdown(UNPLUG_BY_S - gone)}. If it's still plugged in after that, it starts again by itself.`
  const words: Record<Stage, { title: string; text: string }> = {
    stopping: { title: "The unit is shutting down", text: "Stopping apps. This page will say when to unplug it." },
    finishing: { title: "The unit is nearly stopped", text: "Its console has stopped, and the rest is finishing. Don't unplug it yet." },
    unplug: { title: "The unit has stopped: you can unplug it", text: deadline },
    unsure: {
      title: "This page lost touch with the unit",
      text:
        gone < UNSURE_WAIT_S
          ? `It may have lost the network, so it can't see the unit stop. Wait ${countdown(UNSURE_WAIT_S - gone)} before unplugging it.`
          : `It should have stopped by now. ${deadline}`,
    },
    restarting: {
      title: "If it's still plugged in, the unit is starting again",
      text: "Its watchdog starts a stopped unit after about two minutes. To unplug it, shut it down again once it's back.",
    },
    back: { title: "The unit has started again", text: "Log in to use it." },
  }
  const waiting = shown === "stopping" || shown === "finishing" || shown === "restarting" || (shown === "unsure" && gone < UNSURE_WAIT_S)
  return (
    <div role="status" className="fixed inset-0 z-50 grid place-items-center bg-background p-6">
      <div className="grid max-w-sm justify-items-center gap-3 text-center">
        {waiting && <Spinner className="size-8" />}
        <p className="text-lg font-semibold">{words[shown].title}</p>
        <Muted>{words[shown].text}</Muted>
        {shown === "back" && <Button onClick={() => window.location.assign("/login/")}>Log in</Button>}
      </div>
    </div>
  )
}

function ShutdownButton({ onConfirm }: { onConfirm: () => void }) {
  const [asking, setAsking] = useState(false)
  return (
    <AlertDialog open={asking} onOpenChange={setAsking}>
      <AlertDialogTrigger render={<Button variant="outline" />}>Shut down</AlertDialogTrigger>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>Shut the unit down?</AlertDialogTitle>
          <AlertDialogDescription>
            Apps stop, and this page says when to unplug it. If nobody does, it starts again after about two
            minutes.
          </AlertDialogDescription>
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogCancel>Cancel</AlertDialogCancel>
          <AlertDialogAction
            variant="destructive"
            onClick={() => {
              setAsking(false)
              onConfirm()
            }}
          >
            Shut down
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  )
}

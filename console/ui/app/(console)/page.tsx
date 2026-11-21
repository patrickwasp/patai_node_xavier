"use client"

import { type ReactNode, useState } from "react"
import Link from "next/link"

import { ErrorNote, Muted, Notice, Panel, Row, Spinner, tones } from "@/components/common"
import { DomainBanner } from "@/components/domain"
import { Rollbacks } from "@/components/rollbacks"
import { bytes, capitalise, duration, when } from "@/lib/format"
import { assess, DISK_FULL, DISK_WARN, isStale } from "@/lib/health"
import { api } from "@/lib/api"
import { usePoll, useStatus } from "@/lib/hooks"
import type { Report, Status } from "@/lib/report"
import type { Resources } from "@/lib/types"
import { cn } from "@/lib/utils"
import { appTitle, powerLabel } from "@/lib/labels"

// A unit takes a little while to reach a time server after a boot, and that isn't worth a warning.
const CLOCK_GRACE_S = 300
const LOAD_EVERY_MS = 3_000
/** Memory this full is worth a look, and this full means a start is likely to be refused. */
const MEMORY_WARN = 0.8
const MEMORY_FULL = 0.92

const MISSING = "Not reported"

export default function DashboardPage() {
  const { status, error } = useStatus()

  if (status === null) {
    return error ? (
      <ErrorNote>{error}</ErrorNote>
    ) : (
      <div role="status" className="flex justify-center py-12">
        <Spinner />
        <span className="sr-only">Loading</span>
      </div>
    )
  }

  const report = status.report
  return (
    <>
      <h1 className="sr-only">Status</h1>
      <HealthBanner status={status} />
      <DomainBanner />
      <ErrorNote>{error && `${error} These are the last numbers it sent.`}</ErrorNote>
      {isStale(status) && status.age !== null && (
        <Notice level="warn" title="These numbers are out of date">
          Last reported {duration(status.age)} ago.
        </Notice>
      )}
      {report?.clockSynced === false && !report.clockSet && (report.uptime ?? 0) > CLOCK_GRACE_S && (
        <Notice level="unknown">
          The unit doesn&apos;t know the date.{" "}
          <Link href="/system/#clock" className="underline underline-offset-4">
            Set its clock
          </Link>
        </Notice>
      )}
      {report && (
        <div className="grid items-start gap-4 md:grid-cols-2">
          <div className="grid content-start gap-4">
            <Rollbacks report={report} />
            <App report={report} />
            <Software report={report} />
          </div>
          <div className="grid content-start gap-4">
            <Load />
            <Temperatures report={report} />
            <Storage report={report} />
            <Network report={report} />
          </div>
        </div>
      )}
    </>
  )
}

/** Shown only when something is wrong. */
function HealthBanner({ status }: { status: Status }) {
  const health = assess(status)
  if (health.level === "good") return null
  const tone = tones[health.level]
  const Icon = tone.icon

  return (
    <section aria-live="polite" className={cn("flex items-center gap-4 rounded-xl border p-4", tone.box)}>
      <span className={cn("grid size-12 shrink-0 place-items-center rounded-full", tone.solid)}>
        <Icon aria-hidden className="size-7" />
      </span>
      <div className="grid min-w-0 gap-0.5">
        <p className="text-lg leading-tight font-semibold">{health.title}</p>
        {health.detail && <p className="text-sm">{health.detail}</p>}
      </div>
    </section>
  )
}

function workloadSummary(workload: Report["workload"]): { text: string; level: "good" | "warn" | "unknown" } {
  if (workload === null) return { text: MISSING, level: "unknown" }
  if (!workload.stack || workload.state === "stopped") return { text: "None on", level: "unknown" }
  if (workload.state === "partly running") return { text: "Partly running", level: "warn" }
  if (workload.state !== "running") return { text: capitalise(workload.state ?? "unknown"), level: "unknown" }
  if (workload.healthy === true) return { text: "Running and answering", level: "good" }
  if (workload.healthy === false) return { text: "Running, but not answering", level: "warn" }
  return { text: "Running", level: "unknown" }
}

/** Where a card's facts are managed. */
function Manage({ href }: { href: string }) {
  return (
    <Link href={href} prefetch={false} className="text-sm text-muted-foreground underline underline-offset-4">
      Manage
    </Link>
  )
}

function App({ report }: { report: Report }) {
  const summary = workloadSummary(report.workload)
  return (
    <Panel title="App" action={<Manage href="/apps/" />}>
      <dl className="grid gap-1.5">
        {report.workload?.stack && (
          <Row label={report.workload.apps.length > 1 ? "On" : "Name"}>
            {(report.workload.apps.length > 0 ? report.workload.apps : [report.workload.stack]).map(appTitle).join(", ")}
          </Row>
        )}
        <Row label="State">
          <span className={tones[summary.level].text}>{summary.text}</span>
        </Row>
      </dl>
    </Panel>
  )
}

function Reading({ label, value }: { label: string; value: ReactNode }) {
  return (
    <div className="grid gap-0.5">
      <dt className="text-xs text-muted-foreground">{label}</dt>
      <dd className="text-lg font-medium tabular-nums">{value ?? "–"}</dd>
    </div>
  )
}

function celsius(value: number | null): string | null {
  return value === null ? null : `${Math.round(value)}°C`
}

function Temperatures({ report }: { report: Report }) {
  const fan = report.fanPwm === null ? null : `${Math.round((report.fanPwm / 255) * 100)}%`
  const power = report.powerModeName ? powerLabel(report.powerModeName) : report.powerModeId === null ? null : `Mode ${report.powerModeId}`
  return (
    <Panel title="Temperature and power" action={<Manage href="/system/#power" />}>
      <dl className="grid grid-cols-3 gap-3">
        <Reading label="Processor" value={celsius(report.cpuC)} />
        <Reading label="Graphics" value={celsius(report.gpuC)} />
        <Reading label="Board" value={celsius(report.boardC)} />
      </dl>
      <dl className="grid gap-1.5">
        <Row label="Fan">{fan ?? MISSING}</Row>
        <Row label="Power mode">{power ?? MISSING}</Row>
      </dl>
    </Panel>
  )
}

/** A share of something, as a bar, with what it is in words. */
function Meter({ label, share, text, warn = 1, full = 1 }: { label: string; share: number; text: string; warn?: number; full?: number }) {
  const percent = Math.round(Math.min(Math.max(share, 0), 1) * 100)
  const colour = share >= full ? "bg-destructive" : share >= warn ? "bg-warn" : "bg-foreground/60"
  return (
    <div className="grid gap-1">
      <div className="flex items-baseline justify-between gap-3">
        <span>{label}</span>
        <span className="text-xs text-muted-foreground tabular-nums">{text}</span>
      </div>
      <div
        role="meter"
        aria-label={label}
        aria-valuemin={0}
        aria-valuemax={100}
        aria-valuenow={percent}
        className="h-2 overflow-hidden rounded-full bg-muted"
      >
        <div className={cn("h-full rounded-full", colour)} style={{ width: `${Math.max(percent, 1)}%` }} />
      </div>
    </div>
  )
}

function Load() {
  const [load, setLoad] = useState<Resources | null>(null)
  const [failed, setFailed] = useState(false)
  usePoll(async () => {
    try {
      setLoad(await api<Resources>("/api/v1/resources"))
      setFailed(false)
    } catch {
      setFailed(true)
    }
  }, LOAD_EVERY_MS)
  const percent = (value: number | null) => (value === null ? "–" : `${Math.round(value)}%`)
  return (
    <Panel title="Load">
      {!load && <Muted>{failed ? MISSING : "Reading…"}</Muted>}
      {load && (
        <>
          <Meter label="Processor" share={(load.cpu_percent ?? 0) / 100} text={percent(load.cpu_percent)} />
          <Meter label="Graphics" share={(load.gpu_percent ?? 0) / 100} text={percent(load.gpu_percent)} />
          <Meter
            label="Memory"
            share={load.memory_used / Math.max(load.memory_total, 1)}
            text={`${bytes(load.memory_used)} of ${bytes(load.memory_total)}`}
            warn={MEMORY_WARN}
            full={MEMORY_FULL}
          />
          <dl className="grid gap-1.5">
            {load.gpu_memory !== null && <Row label="Of which GPU">{bytes(load.gpu_memory)}</Row>}
            {load.swap_used > 0 && <Row label="Swap">{`${bytes(load.swap_used)} of ${bytes(load.swap_total)}`}</Row>}
          </dl>
        </>
      )}
    </Panel>
  )
}

function Storage({ report }: { report: Report }) {
  return (
    <Panel title="Storage" action={<Manage href="/system/#disk" />}>
      {report.disks.length === 0 && <Muted>{MISSING}</Muted>}
      {report.disks.map(({ key, label, disk }) => (
        <Meter
          key={key}
          label={label}
          share={disk.used / disk.size}
          text={`${bytes(disk.used)} of ${bytes(disk.size)}`}
          warn={DISK_WARN}
          full={DISK_FULL}
        />
      ))}
      {report.nvmePresent !== null && (
        <dl>
          <Row label="NVMe drive">
            {report.nvmePresent ? "Present" : <span className="text-warn">Not found</span>}
          </Row>
        </dl>
      )}
    </Panel>
  )
}

function slotState(state: string | null): string {
  if (state === null) return "Unknown"
  if (state === "bad") return "Failed"
  return capitalise(state)
}

function Software({ report }: { report: Report }) {
  const switching = report.primarySlot && report.bootedSlot && report.primarySlot !== report.bootedSlot
  const check = report.lastCheck
  return (
    <Panel title="Software" action={<Manage href="/updates/" />}>
      {switching && <Notice level="good" title={`Slot ${report.primarySlot} starts at the next restart`} />}
      <dl className="grid gap-1.5">
        <Row label="Release">{report.release ?? MISSING}</Row>
        <Row label="Up for">{report.uptime != null ? duration(report.uptime) : MISSING}</Row>
        {report.slots.map((slot) => (
          <Row key={slot.name} label={`Slot ${slot.name}`}>
            {slot.name === report.bootedSlot && <span className="text-muted-foreground">In use · </span>}
            <span className={slot.state === "bad" ? "text-destructive" : undefined}>{slotState(slot.state)}</span>
          </Row>
        ))}
        <Row label="Last check">{check ? (when(check.time) ?? "Time unknown") : MISSING}</Row>
        {check && (
          <Row label="Result">
            {check.ok === false ? (
              <span className="text-warn">{check.detail ? `Failed: ${check.detail}` : "Failed"}</span>
            ) : check.detail ? (
              capitalise(check.detail)
            ) : check.ok ? (
              "OK"
            ) : (
              MISSING
            )}
          </Row>
        )}
        {check?.ok === false && report.lastSuccess && <Row label="Last worked">{when(report.lastSuccess)}</Row>}
      </dl>
    </Panel>
  )
}

function Network({ report }: { report: Report }) {
  return (
    <Panel title="Network" action={<Manage href="/system/#network" />}>
      {report.addresses.length === 0 ? (
        <Muted>{MISSING}</Muted>
      ) : (
        <dl className="grid gap-1.5">
          {report.addresses.map(({ name, address }) => (
            <Row key={`${name}-${address}`} label={name ?? "Address"}>
              <span className="font-mono text-sm">{address}</span>
            </Row>
          ))}
        </dl>
      )}
    </Panel>
  )
}

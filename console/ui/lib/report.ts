// The unit's report, read defensively. Any field may be missing or of the wrong type, because releases
// in the field differ and the report is written by a separate program. Everything past readReport can
// rely on these types.

export type Disk = { size: number; used: number }

export type Report = {
  generated: string | null
  clockSynced: boolean | null
  /** An owner set the clock this boot. */
  clockSet: boolean | null
  uptime: number | null
  state: string | null
  failed: string[]
  release: string | null
  bootedSlot: string | null
  primarySlot: string | null
  slots: { name: string; state: string | null }[]
  channel: string | null
  lastCheck: { time: string | null; ok: boolean | null; detail: string | null } | null
  lastSuccess: string | null
  cpuC: number | null
  gpuC: number | null
  boardC: number | null
  fanPwm: number | null
  powerModeId: number | null
  powerModeName: string | null
  disks: { key: string; label: string; disk: Disk }[]
  nvmePresent: boolean | null
  dataState: string | null
  /** The first app that's on as stack, every app that's on, and how all their services are doing. */
  workload: { stack: string | null; apps: string[]; state: string | null; healthy: boolean | null } | null
  rollbacks: { version: string | null; time: string | null; reason: string | null }[]
  addresses: { name: string | null; address: string }[]
}

export type Status = {
  report: Report | null
  /** Seconds since the report was written, or null if nobody knows. */
  age: number | null
}

const DISK_LABELS: [string, string][] = [
  ["root", "System"],
  ["persist", "Settings and logins"],
  ["data", "Models and data"],
]

function record(value: unknown): Record<string, unknown> {
  return isRecord(value) ? (value as Record<string, unknown>) : {}
}

function isRecord(value: unknown): boolean {
  return typeof value === "object" && value !== null && !Array.isArray(value)
}

function text(value: unknown): string | null {
  return typeof value === "string" && value !== "" ? value : null
}

function number(value: unknown): number | null {
  return typeof value === "number" && Number.isFinite(value) ? value : null
}

function flag(value: unknown): boolean | null {
  return typeof value === "boolean" ? value : null
}

function list(value: unknown): unknown[] {
  return Array.isArray(value) ? value : []
}

export function readReport(raw: unknown): Report {
  const report = record(raw)
  const system = record(report.system)
  const slots = record(report.slots)
  const updates = record(report.updates)
  const lastCheck = updates.last_check
  const thermal = record(report.thermal)
  const powerMode = record(report.power_mode)
  const disks = record(report.disks)
  const workload = record(report.workload)

  return {
    generated: text(report.generated),
    clockSynced: flag(report.clock_synced),
    clockSet: flag(report.clock_set),
    uptime: number(report.uptime_s),
    state: text(system.state),
    failed: list(system.failed).map(String),
    release: text(report.release),
    bootedSlot: text(slots.booted),
    primarySlot: text(slots.primary),
    slots: Object.entries(slots)
      .filter(([, value]) => isRecord(value))
      .map(([name, value]) => ({ name, state: text(record(value).state) }))
      .sort((a, b) => a.name.localeCompare(b.name)),
    channel: text(updates.channel),
    lastCheck: !isRecord(lastCheck)
      ? null
      : {
          time: text(record(lastCheck).time),
          ok: flag(record(lastCheck).ok),
          detail: text(record(lastCheck).detail),
        },
    lastSuccess: text(updates.last_success),
    cpuC: number(thermal.cpu_c),
    gpuC: number(thermal.gpu_c),
    boardC: number(thermal.board_c),
    fanPwm: number(thermal.fan_pwm),
    powerModeId: number(powerMode.id),
    powerModeName: text(powerMode.name),
    disks: DISK_LABELS.flatMap(([key, label]) => {
      const disk = record(disks[key])
      const size = number(disk.size)
      const used = number(disk.used)
      return size !== null && used !== null && size > 0 ? [{ key, label, disk: { size, used } }] : []
    }),
    nvmePresent: flag(disks.nvme_present),
    dataState: text(disks.data_state),
    workload: !isRecord(report.workload)
      ? null
      : {
          stack: text(workload.stack),
          apps: list(workload.apps).flatMap((app) => (typeof app === "string" ? [app] : [])),
          state: text(workload.state),
          healthy: flag(workload.healthy),
        },
    rollbacks: list(report.rollbacks).map((item) => {
      const rollback = record(item)
      return {
        version: text(rollback.version),
        time: text(rollback.time),
        reason: text(rollback.reason),
      }
    }),
    addresses: list(report.addresses).flatMap((item) => {
      const entry = record(item)
      const address = text(entry.address)
      return address === null ? [] : [{ name: text(entry.interface), address }]
    }),
  }
}

export function readStatus(raw: unknown): Status {
  const status = record(raw)
  const age = number(status.age_s)
  return {
    report: status.report === null || status.report === undefined ? null : readReport(status.report),
    age: age === null || age < 0 ? null : age,
  }
}

/** The workload the report says is up, if any. */
export function runningStack(report: Report | null): string | null {
  const workload = report?.workload
  if (!workload?.stack) return null
  return workload.state === "running" || workload.state === "starting" ? workload.stack : null
}

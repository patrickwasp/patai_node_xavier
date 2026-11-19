import type { Status } from "@/lib/report"
import { appTitle } from "@/lib/labels"

export type Level = "good" | "warn" | "bad" | "unknown"

export type Health = {
  level: Level
  title: string
  detail: string | null
}

/** A report older than this means patai-report has stopped writing them. It writes one a minute. */
export const STALE_AFTER_S = 180

export const DISK_WARN = 0.85
export const DISK_FULL = 0.95

export function isStale(status: Status): boolean {
  return status.age !== null && status.age > STALE_AFTER_S
}

/**
 * The one thing someone glancing at a phone needs, shown only when it isn't good. A rollback or a failed update check doesn't count
 * here: the unit went back to something that works, and a dead update server isn't a sick unit. Both
 * show further down the page.
 */
export function assess(status: Status): Health {
  const report = status.report
  if (report === null) {
    return {
      level: "unknown",
      title: "The unit hasn't reported yet",
      detail: "Its first report usually appears within a minute of starting.",
    }
  }

  if (report.failed.length > 0 || report.state === "degraded") {
    const names = report.failed.join(", ")
    return {
      level: "bad",
      title: "The unit needs attention",
      detail: names ? `Not working: ${names}` : "Part of the unit has stopped working.",
    }
  }

  if (isStale(status)) {
    return { level: "warn", title: "No recent report from the unit", detail: null }
  }

  if (report.state === "starting" || report.state === "initializing") {
    return { level: "warn", title: "The unit is starting", detail: null }
  }
  if (report.state === "stopping" || report.state === "maintenance") {
    return { level: "warn", title: "The unit is shutting down", detail: null }
  }
  if (report.state !== "running") {
    return {
      level: "unknown",
      title: "The unit's state isn't clear",
      detail: report.state ? `It reports “${report.state}”.` : "Its report doesn't say.",
    }
  }

  const full = report.disks.find(({ disk }) => disk.used / disk.size >= DISK_FULL)
  if (full) {
    return { level: "warn", title: "The unit is nearly full", detail: `${full.label} is nearly full.` }
  }
  if (report.workload?.state === "running" && report.workload.healthy === false) {
    const name = report.workload.stack ? appTitle(report.workload.stack) : "The app"
    return { level: "warn", title: "The app isn't answering", detail: `${name} is running but doesn't answer.` }
  }
  if (report.dataState === "undecided") {
    return {
      level: "warn",
      title: "The disk needs an answer",
      detail: "It has data from before. Apps can't run until an owner chooses to keep or erase it, on the System page.",
    }
  }
  if (report.dataState === "foreign") {
    return {
      level: "warn",
      title: "The disk isn't set up",
      detail: "Apps can't run until an owner sets it up, on the System page.",
    }
  }
  if (report.dataState === "unmounted") {
    return {
      level: "warn",
      title: "The disk didn't mount",
      detail: "Apps can't run. Restart the unit, and if that doesn't help, it needs service.",
    }
  }
  if (report.nvmePresent === false) {
    return {
      level: "warn",
      title: "The NVMe drive is missing",
      detail: "Apps can't run without it.",
    }
  }
  return { level: "good", title: "The unit is healthy", detail: null }
}

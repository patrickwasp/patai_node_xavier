"use client"

import { Muted, Panel } from "@/components/common"
import { when } from "@/lib/format"
import type { Report } from "@/lib/report"

/** Releases this unit tried and went back from, each with why. Wide spans both columns of a page's grid. */
export function Rollbacks({ report, wide = false }: { report: Report; wide?: boolean }) {
  if (report.rollbacks.length === 0) return null
  return (
    <Panel title="Updates that didn't work" className={wide ? "md:col-span-2" : undefined}>
      <Muted>The unit went back to the release it had before.</Muted>
      <ul className="grid gap-3">
        {report.rollbacks.map((rollback, index) => (
          <li key={index} className="grid gap-0.5">
            <p className="font-medium">
              {rollback.version ?? "An update"}
              {rollback.time && <span className="font-normal text-muted-foreground"> · {when(rollback.time)}</span>}
            </p>
            <p className="font-mono text-xs">{rollback.reason ?? "No reason was recorded."}</p>
          </li>
        ))}
      </ul>
    </Panel>
  )
}

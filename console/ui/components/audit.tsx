"use client"

import { useCallback, useEffect, useState } from "react"

import { ErrorNote, Muted, Panel, Spinner } from "@/components/common"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { api, plainError } from "@/lib/api"
import { capitalise, when } from "@/lib/format"
import { auditAction } from "@/lib/labels"
import type { AuditEntry } from "@/lib/types"

/** The API's page size. A shorter page is the last one. */
const PAGE = 200

/** Who did what on this unit, newest first. An owner's. */
export function AuditLog() {
  const [entries, setEntries] = useState<AuditEntry[] | null>(null)
  const [more, setMore] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const load = useCallback(async (before?: number) => {
    setBusy(true)
    setError(null)
    try {
      const page = await api<AuditEntry[]>(`/api/v1/audit${before === undefined ? "" : `?before=${before}`}`)
      setEntries((previous) => (before === undefined ? page : [...(previous ?? []), ...page]))
      setMore(page.length === PAGE)
    } catch (failure) {
      setError(plainError(failure))
    } finally {
      setBusy(false)
    }
  }, [])

  useEffect(() => {
    void load()
  }, [load])

  return (
    <>
      <ErrorNote>{error}</ErrorNote>
      {entries === null && !error && (
        <div role="status" className="flex justify-center py-12">
          <Spinner />
          <span className="sr-only">Loading</span>
        </div>
      )}
      {entries && (
        <Panel title={`${entries.length} ${entries.length === 1 ? "entry" : "entries"}`}>
          <Muted>
            Newest first. A unit may not know the date, so entries also show which start of the unit they belong to.
          </Muted>
          {entries.length === 0 && <Muted>Nothing yet.</Muted>}
          <ol className="grid divide-y">
            {entries.map((entry) => (
              <Line key={entry.seq} entry={entry} />
            ))}
          </ol>
          {more && (
            <Button variant="outline" disabled={busy} onClick={() => load(entries[entries.length - 1]?.seq)}>
              {busy ? "Loading…" : "Load more"}
            </Button>
          )}
        </Panel>
      )}
    </>
  )
}

function Line({ entry }: { entry: AuditEntry }) {
  const actor = entry.actor === "-" || !entry.actor ? "Someone" : entry.actor
  const refused = entry.result !== "ok" && entry.result !== "queued"
  const boot = entry.boot_id.trim().slice(0, 8)
  const facts = [`#${entry.seq}`, boot && `start ${boot}`, entry.source].filter(Boolean)
  return (
    <li className="grid gap-0.5 py-2 first:pt-0 last:pb-0">
      <div className="flex items-baseline justify-between gap-3">
        <p className="min-w-0">
          <span className="font-medium">{actor}</span>{" "}
          <span className="text-muted-foreground">{auditAction(entry.action, refused).toLowerCase()}</span>
          {entry.target && <span className="font-mono text-xs"> {entry.target}</span>}
        </p>
        {refused && <Badge variant="destructive">{capitalise(entry.result)}</Badge>}
      </div>
      <p className="text-xs text-muted-foreground">
        {when(entry.at) ?? "No date"} · <span className="font-mono">{facts.join(" · ")}</span>
      </p>
    </li>
  )
}

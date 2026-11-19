"use client"

import { useCallback, useEffect, useState } from "react"

import { ErrorNote, Muted, Panel, Spinner } from "@/components/common"
import { columnsOf, DataTable } from "@/components/data-table"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { api, plainError } from "@/lib/api"
import { capitalise, when } from "@/lib/format"
import { auditAction } from "@/lib/labels"
import type { AuditEntry } from "@/lib/types"

const audit = columnsOf<AuditEntry>()
const actor = (entry: AuditEntry) => (entry.actor === "-" || !entry.actor ? "Someone" : entry.actor)
const refused = (entry: AuditEntry) => entry.result !== "ok" && entry.result !== "queued"

const AUDIT = audit.columns([
  audit.accessor("seq", { header: "#", meta: { numeric: true, className: "font-mono text-xs text-muted-foreground" } }),
  audit.accessor((entry) => when(entry.at) ?? "No date", {
    id: "at",
    header: "When",
    sortFn: (a, b) => a.original.seq - b.original.seq,
    meta: { className: "text-xs text-muted-foreground" },
  }),
  audit.accessor(actor, { id: "actor", header: "Who", meta: { facet: {}, className: "font-medium" } }),
  audit.accessor((entry) => auditAction(entry.action, refused(entry)), { id: "action", header: "What", meta: { facet: {} } }),
  audit.accessor("target", { header: "On", meta: { className: "font-mono text-xs whitespace-normal wrap-anywhere" } }),
  audit.accessor((entry) => capitalise(entry.result), {
    id: "result",
    header: "Result",
    meta: { facet: {} },
    cell: (cell) =>
      refused(cell.row.original) ? <Badge variant="destructive">{cell.getValue()}</Badge> : cell.getValue(),
  }),
  audit.accessor("source", { header: "From", meta: { facet: {}, className: "font-mono text-xs" } }),
  audit.accessor((entry) => entry.boot_id.trim().slice(0, 8), {
    id: "boot",
    header: "Start",
    meta: { facet: {}, className: "font-mono text-xs text-muted-foreground" },
  }),
])

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
        <Panel title="Audit log">
          <Muted>A unit may not know the date, so each entry also says which start of the unit it belongs to.</Muted>
          <DataTable
            id="audit"
            label="Audit log"
            data={entries}
            columns={AUDIT}
            rowId={(entry) => String(entry.seq)}
            empty="Nothing yet."
            hidden={["seq", "source"]}
            sort={[{ id: "at", desc: true }]}
            pageSize={50}
            more={
              more && (
                <Button size="sm" variant="outline" disabled={busy} onClick={() => load(entries[entries.length - 1]?.seq)}>
                  {busy ? "Loading…" : "Load older"}
                </Button>
              )
            }
          />
        </Panel>
      )}
    </>
  )
}

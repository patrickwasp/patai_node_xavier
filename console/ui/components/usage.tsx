"use client"

import { useCallback, useEffect, useMemo, useState } from "react"
import { RefreshCw } from "lucide-react"

import { ErrorNote } from "@/components/common"
import { columnsOf, DataTable } from "@/components/data-table"
import { Button } from "@/components/ui/button"
import { api, plainError } from "@/lib/api"
import { useLoad } from "@/lib/hooks"
import type { App, APIRequest, Usage } from "@/lib/types"
import { cn } from "@/lib/utils"

const number = (value: number) => value.toLocaleString()

const usageColumns = columnsOf<Usage>()
const keyName = (row: { key_id: number; key: string }) => (row.key_id === 0 ? `${row.key} (console)` : row.key)

const USAGE = usageColumns.columns([
  usageColumns.accessor("day", { header: "Day (UTC)" }),
  usageColumns.accessor(keyName, { id: "key", header: "Key", meta: { facet: {} } }),
  usageColumns.accessor("requests", { header: "Requests", meta: { numeric: true }, cell: (cell) => number(cell.getValue()) }),
  usageColumns.accessor("errors", { header: "Failed", meta: { numeric: true }, cell: (cell) => number(cell.getValue()) }),
  usageColumns.accessor("prompt_tokens", { header: "Tokens in", meta: { numeric: true }, cell: (cell) => number(cell.getValue()) }),
  usageColumns.accessor("completion_tokens", { header: "Tokens out", meta: { numeric: true }, cell: (cell) => number(cell.getValue()) }),
  usageColumns.accessor("images", { header: "Images", meta: { numeric: true }, cell: (cell) => number(cell.getValue()) }),
  usageColumns.accessor((row) => row.millis / Math.max(row.requests, 1), {
    id: "average",
    header: "Average",
    enableGlobalFilter: false,
    meta: { numeric: true },
    cell: (cell) => seconds(cell.getValue()),
  }),
])

/** Each key's use by day, for the last week. */
export function UsageTable() {
  const usage = useLoad<Usage[]>("/api/v1/api-usage?days=7")
  if (usage.error) return <ErrorNote>{usage.error}</ErrorNote>
  if (!usage.data) return null
  return (
    <DataTable
      id="usage"
      label="Use by day"
      data={usage.data}
      columns={USAGE}
      rowId={(row) => `${row.day} ${row.key_id}`}
      empty="Nothing in the last 7 days."
      sort={[{ id: "day", desc: true }]}
    />
  )
}

function seconds(millis: number): string {
  return millis < 1000 ? `${Math.round(millis)} ms` : `${(millis / 1000).toFixed(1)} s`
}

/** Today's requests and tokens across every key, in a line. */
export function TodayLine() {
  const usage = useLoad<Usage[]>("/api/v1/api-usage?days=1")
  if (!usage.data) return null
  const requests = usage.data.reduce((sum, row) => sum + row.requests, 0)
  const tokens = usage.data.reduce((sum, row) => sum + row.prompt_tokens + row.completion_tokens, 0)
  const images = usage.data.reduce((sum, row) => sum + row.images, 0)
  return (
    <p className="text-sm text-muted-foreground">
      Today: {number(requests)} requests, {number(tokens)} tokens{images > 0 && `, ${number(images)} images`}
    </p>
  )
}

const PAGE = 50

const moment = new Intl.DateTimeFormat(undefined, { dateStyle: "medium", timeStyle: "medium" })

const requestColumns = columnsOf<APIRequest>()

/** A line for each request, newest first: never what was asked or answered. */
export function RequestLog({ app }: { app: App }) {
  const [lines, setLines] = useState<APIRequest[] | null>(null)
  const [more, setMore] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const load = useCallback(async (before = 0) => {
    setBusy(true)
    try {
      const page = await api<APIRequest[]>(`/api/v1/api-requests?limit=${PAGE}${before ? `&before=${before}` : ""}`)
      setLines((current) => (before ? [...(current ?? []), ...page] : page))
      setMore(page.length === PAGE)
      setError(null)
    } catch (failure) {
      setError(plainError(failure))
    } finally {
      setBusy(false)
    }
  }, [])
  useEffect(() => void load(), [load])
  const columns = useMemo(() => {
    const title = (name: string) => app.apis.find((item) => item.name === name)?.title ?? name
    return requestColumns.columns([
      requestColumns.accessor((line) => Date.parse(line.at), {
        id: "at",
        header: "Time",
        enableGlobalFilter: false,
        meta: { className: "text-xs text-muted-foreground tabular-nums" },
        cell: (cell) => moment.format(new Date(cell.row.original.at)),
      }),
      requestColumns.accessor((line) => title(line.api), { id: "api", header: "API", meta: { facet: {} } }),
      requestColumns.accessor(keyName, { id: "key", header: "Key", meta: { facet: {} } }),
      requestColumns.accessor("endpoint", { header: "Endpoint", meta: { className: "font-mono text-xs" } }),
      requestColumns.accessor("model", { header: "Model", meta: { facet: {} } }),
      requestColumns.accessor("status", {
        header: "Status",
        meta: { facet: {}, numeric: true },
        cell: (cell) => <span className={cn(cell.getValue() >= 400 && "text-destructive")}>{cell.getValue()}</span>,
      }),
      requestColumns.accessor("prompt_tokens", { header: "Tokens in", meta: { numeric: true }, cell: (cell) => number(cell.getValue()) }),
      requestColumns.accessor("completion_tokens", { header: "Tokens out", meta: { numeric: true }, cell: (cell) => number(cell.getValue()) }),
      requestColumns.accessor("images", { header: "Images", meta: { numeric: true }, cell: (cell) => number(cell.getValue()) }),
      requestColumns.accessor("millis", {
        header: "Took",
        enableGlobalFilter: false,
        meta: { numeric: true },
        cell: (cell) => seconds(cell.getValue()),
      }),
    ])
  }, [app.apis])
  return (
    <>
      <ErrorNote>{error}</ErrorNote>
      {lines && (
        <DataTable
          id="requests"
          label="Requests"
          data={lines}
          columns={columns}
          rowId={(line) => String(line.id)}
          empty="No requests yet."
          hidden={["endpoint", "images"]}
          sort={[{ id: "at", desc: true }]}
          more={
            <>
              <Button size="sm" variant="ghost" disabled={busy} onClick={() => void load()}>
                <RefreshCw aria-hidden />
                Refresh
              </Button>
              {more && (
                <Button size="sm" variant="outline" disabled={busy} onClick={() => void load(lines[lines.length - 1].id)}>
                  Load older
                </Button>
              )}
            </>
          }
        />
      )}
    </>
  )
}

"use client"

import { useCallback, useEffect, useState } from "react"

import { ErrorNote, Muted } from "@/components/common"
import { Button } from "@/components/ui/button"
import { api, plainError } from "@/lib/api"
import { useLoad } from "@/lib/hooks"
import type { App, APIRequest, Usage } from "@/lib/types"
import { cn } from "@/lib/utils"

const number = (value: number) => value.toLocaleString()

/** Each key's use by day, for the last week. */
export function UsageTable() {
  const usage = useLoad<Usage[]>("/api/v1/api-usage?days=7")
  if (usage.error) return <ErrorNote>{usage.error}</ErrorNote>
  if (!usage.data) return null
  if (usage.data.length === 0) return <Muted>Nothing in the last 7 days.</Muted>
  return (
    <div className="-mx-1 overflow-x-auto px-1">
      <table className="w-full text-sm tabular-nums">
        <thead className="text-left text-xs text-muted-foreground">
          <tr>
            <th className="py-1 pr-3 font-normal">Day (UTC)</th>
            <th className="py-1 pr-3 font-normal">Key</th>
            <th className="py-1 pr-3 text-right font-normal">Requests</th>
            <th className="py-1 pr-3 text-right font-normal">Failed</th>
            <th className="py-1 pr-3 text-right font-normal">Tokens in</th>
            <th className="py-1 pr-3 text-right font-normal">Tokens out</th>
            <th className="py-1 pr-3 text-right font-normal">Images</th>
            <th className="py-1 text-right font-normal">Average</th>
          </tr>
        </thead>
        <tbody className="divide-y">
          {usage.data.map((row) => (
            <tr key={row.day + row.key_id}>
              <td className="py-1 pr-3 whitespace-nowrap">{row.day}</td>
              <td className="py-1 pr-3 wrap-anywhere">{row.key_id === 0 ? `${row.key} (console)` : row.key}</td>
              <td className="py-1 pr-3 text-right">{number(row.requests)}</td>
              <td className="py-1 pr-3 text-right">{number(row.errors)}</td>
              <td className="py-1 pr-3 text-right">{number(row.prompt_tokens)}</td>
              <td className="py-1 pr-3 text-right">{number(row.completion_tokens)}</td>
              <td className="py-1 pr-3 text-right">{number(row.images)}</td>
              <td className="py-1 text-right whitespace-nowrap">{seconds(row.millis / Math.max(row.requests, 1))}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
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

/** A line for each request, newest first: never what was asked or answered. */
export function RequestLog({ app }: { app: App }) {
  const [lines, setLines] = useState<APIRequest[]>([])
  const [more, setMore] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const load = useCallback(async (before = 0) => {
    try {
      const page = await api<APIRequest[]>(`/api/v1/api-requests?limit=${PAGE}${before ? `&before=${before}` : ""}`)
      setLines((current) => (before ? [...current, ...page] : page))
      setMore(page.length === PAGE)
      setError(null)
    } catch (failure) {
      setError(plainError(failure))
    }
  }, [])
  useEffect(() => void load(), [load])
  const title = (name: string) => app.apis.find((item) => item.name === name)?.title ?? name
  return (
    <>
      <ErrorNote>{error}</ErrorNote>
      <div className="flex justify-end">
        <Button size="sm" variant="ghost" onClick={() => void load()}>
          Refresh
        </Button>
      </div>
      {lines.length === 0 && !error && <Muted>No requests yet.</Muted>}
      <ul className="grid divide-y text-sm">
        {lines.map((line) => {
          const failed = line.status >= 400
          const used = [
            line.prompt_tokens + line.completion_tokens > 0 && `${number(line.prompt_tokens)} in, ${number(line.completion_tokens)} out`,
            line.images > 0 && `${line.images} ${line.images === 1 ? "image" : "images"}`,
            seconds(line.millis),
          ].filter(Boolean)
          return (
            <li key={line.id} className="grid gap-0.5 py-1.5 sm:grid-cols-[11rem_1fr_auto] sm:gap-3">
              <span className="text-xs text-muted-foreground tabular-nums">{new Date(line.at).toLocaleString()}</span>
              <span className="min-w-0 wrap-anywhere">
                {title(line.api)} · {line.key_id === 0 ? `${line.key} (console)` : line.key}
                <span className="block text-xs text-muted-foreground">{[line.endpoint, line.model].filter(Boolean).join(" · ")}</span>
              </span>
              <span className="text-xs tabular-nums sm:text-right">
                <span className={cn(failed && "text-destructive")}>{line.status}</span> · {used.join(" · ")}
              </span>
            </li>
          )
        })}
      </ul>
      {more && (
        <Button size="sm" variant="outline" onClick={() => void load(lines[lines.length - 1].id)}>
          Older
        </Button>
      )}
    </>
  )
}

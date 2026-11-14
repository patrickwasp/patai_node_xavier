"use client"

import { useCallback, useEffect, useId, useLayoutEffect, useRef, useState } from "react"
import { Download, Pause, Play } from "lucide-react"

import { AuditLog } from "@/components/audit"
import { ErrorNote, Muted, Notice, PageTitle, Panel, Spinner } from "@/components/common"
import { useIdentity } from "@/components/gate"
import { isOwner, useSession } from "@/components/session"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { api, download, plainError } from "@/lib/api"
import { useStatus } from "@/lib/hooks"
import { logSource } from "@/lib/labels"
import type { LogBoot, LogEntry, LogIndex, LogPage } from "@/lib/types"
import { cn } from "@/lib/utils"

/** How many lines a look asks for, and then each "Show more". The API gives at most the last. */
const COUNTS = [200, 1000, 2000]
/** Following stops keeping the oldest past this, so a busy log can't fill the browser. */
const KEEP = 5000

type View = "unit" | "audit"

export function LogsView({ initial }: { initial: View }) {
  const session = useSession()
  const [view, setView] = useState<View>(isOwner(session) ? initial : "unit")
  if (session.role !== "operator" && session.role !== "owner") {
    return (
      <>
        <PageTitle>Logs</PageTitle>
        <Muted>Only an operator or an owner can see these.</Muted>
      </>
    )
  }
  return (
    <>
      <PageTitle hint="What the unit has written about itself, and who did what on it.">Logs</PageTitle>
      {isOwner(session) && (
        <div className="flex gap-1 rounded-lg border p-1 sm:w-fit" role="group" aria-label="Which log">
          {(["unit", "audit"] as const).map((each) => (
            <Button
              key={each}
              variant={view === each ? "secondary" : "ghost"}
              size="sm"
              className="flex-1 sm:flex-none"
              aria-pressed={view === each}
              onClick={() => setView(each)}
            >
              {each === "unit" ? "The unit's logs" : "Audit"}
            </Button>
          ))}
        </div>
      )}
      {view === "unit" ? (
        <>
          <LogViewer />
          {isOwner(session) && <SupportBundle />}
        </>
      ) : (
        <AuditLog />
      )}
    </>
  )
}

const timeFormat = new Intl.DateTimeFormat(undefined, { hour: "2-digit", minute: "2-digit", second: "2-digit" })
const dayFormat = new Intl.DateTimeFormat(undefined, { month: "short", day: "numeric" })

function bootLabel(boot: LogBoot): string {
  if (boot.index === 0) return "This start"
  const from = boot.first ? new Date(boot.first) : null
  const to = boot.last ? new Date(boot.last) : null
  const span =
    from && to && !Number.isNaN(from.getTime())
      ? ` · ${dayFormat.format(from)} ${timeFormat.format(from)} to ${timeFormat.format(to)}`
      : ""
  return `${boot.index === -1 ? "The start before" : `${-boot.index} starts ago`}${span}`
}

function Select({
  label,
  value,
  onChange,
  options,
  disabled,
}: {
  label: string
  value: string
  onChange: (value: string) => void
  options: { value: string; label: string }[]
  disabled?: boolean
}) {
  const id = useId()
  return (
    <div className="grid min-w-0 gap-1.5">
      <Label htmlFor={id}>{label}</Label>
      <select
        id={id}
        value={value}
        disabled={disabled}
        onChange={(event) => onChange(event.target.value)}
        className="h-8 w-full min-w-0 rounded-lg border border-input bg-transparent px-2 text-sm outline-none focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50 disabled:opacity-50 dark:bg-input/30"
      >
        {options.map((option) => (
          <option key={option.value} value={option.value}>
            {option.label}
          </option>
        ))}
      </select>
    </div>
  )
}

type Follow = { state: "off" } | { state: "on" } | { state: "lost" } | { state: "ended"; reason: string }

function LogViewer() {
  const { status } = useStatus()
  // A link can open an app's output: /logs/?source=output&app=<app>. Read once the page is in the browser.
  const [linked, setLinked] = useState<string | null>(null)
  const stack = linked ?? status?.report?.workload?.stack ?? null
  const [index, setIndex] = useState<LogIndex | null>(null)
  const [indexError, setIndexError] = useState<string | null>(null)
  const [source, setSource] = useState("all")
  const [boot, setBoot] = useState("")
  const [important, setImportant] = useState(false)
  const [count, setCount] = useState(COUNTS[0])
  const [search, setSearch] = useState("")
  const [page, setPage] = useState<LogPage | null>(null)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [follow, setFollow] = useState<Follow>({ state: "off" })
  const [paused, setPaused] = useState(false)
  const events = useRef<EventSource | null>(null)
  const box = useRef<HTMLOListElement | null>(null)
  const stick = useRef(true)

  const output = source === "output"
  const current = boot === "" || output

  useEffect(() => {
    const asked = new URLSearchParams(window.location.search)
    if (asked.get("source") === "output") setSource("output")
    if (asked.get("app")) setLinked(asked.get("app"))
  }, [])

  useEffect(() => {
    api<LogIndex>("/api/v1/logs")
      .then(setIndex)
      .catch((failure) => setIndexError(plainError(failure)))
  }, [])

  const stopFollowing = useCallback((next: Follow = { state: "off" }) => {
    events.current?.close()
    events.current = null
    setFollow(next)
  }, [])

  const query = useCallback(
    (extra: Record<string, string>) => {
      const params = new URLSearchParams(extra)
      if (important) params.set("important", "1")
      if (output && stack) params.set("stack", stack)
      if (!output && boot) params.set("boot", boot)
      return params.toString()
    },
    [important, output, stack, boot]
  )

  // Another log's lines go as soon as it's chosen, so nothing follows from a cursor that isn't this log's. Only the
  // latest look is shown, however the answers come back.
  const latest = useRef(0)
  const showing = useRef("")
  const load = useCallback(async () => {
    const which = `${source}?${query({})}`
    const look = ++latest.current
    if (which !== showing.current) setPage(null)
    if (output && !stack) {
      showing.current = which
      setPage({ entries: [], cursor: "", more: false })
      return
    }
    setBusy(true)
    setError(null)
    try {
      const next = await api<LogPage>(`/api/v1/logs/${source}?${query({ count: String(count) })}`)
      if (look !== latest.current) return
      stick.current = true
      showing.current = which
      setPage(next)
    } catch (failure) {
      if (look === latest.current) setError(plainError(failure))
    } finally {
      if (look === latest.current) setBusy(false)
    }
  }, [source, count, query, output, stack])

  // A new choice is a new look, and ends following the old one.
  useEffect(() => {
    stopFollowing()
    void load()
  }, [load, stopFollowing])

  useEffect(() => () => events.current?.close(), [])

  const startFollowing = useCallback(
    (after: string) => {
      events.current?.close()
      const stream = new EventSource(`/api/v1/logs/${source}/follow?${query({ after })}`)
      events.current = stream
      setFollow({ state: "on" })
      stream.addEventListener("entry", (event) => {
        const entry = JSON.parse((event as MessageEvent<string>).data) as LogEntry
        const list = box.current
        stick.current = !list || list.scrollHeight - list.scrollTop - list.clientHeight < 48
        setPage((previous) => {
          if (!previous) return previous
          const entries = [...previous.entries, entry]
          return { ...previous, entries: entries.slice(-KEEP), cursor: entry.cursor ?? previous.cursor }
        })
      })
      stream.addEventListener("gap", () => setPage((previous) => previous && { ...previous, gap: true }))
      stream.addEventListener("end", (event) => {
        const reason = JSON.parse((event as MessageEvent<string>).data) as string
        stream.close()
        events.current = null
        setFollow({ state: "ended", reason })
      })
      stream.onopen = () => setFollow({ state: "on" })
      stream.onerror = () => {
        // It tries again by itself, from the last line it had, unless the unit said no.
        if (stream.readyState === EventSource.CLOSED) {
          events.current = null
          setFollow({ state: "ended", reason: "refused" })
        } else {
          setFollow({ state: "lost" })
        }
      }
    },
    [source, query]
  )

  // Hidden, it stops asking the unit, and carries on from where it was when it's back.
  useEffect(() => {
    function changed() {
      if (document.hidden && events.current) {
        events.current.close()
        events.current = null
        setPaused(true)
      } else if (!document.hidden && paused) {
        setPaused(false)
        if (page) startFollowing(page.cursor || (output ? "1970-01-01T00:00:00Z" : ""))
      }
    }
    document.addEventListener("visibilitychange", changed)
    return () => document.removeEventListener("visibilitychange", changed)
  }, [paused, page, output, startFollowing])

  useLayoutEffect(() => {
    if (stick.current && box.current) box.current.scrollTop = box.current.scrollHeight
  }, [page])

  const sources = (index?.sources ?? []).map((each) => ({ value: each.id, label: logSource(each.id).label }))
  const boots = (index?.boots ?? []).map((each) => ({ value: each.index === 0 ? "" : each.id, label: bootLabel(each) }))
  const needle = search.trim().toLowerCase()
  const shown = (page?.entries ?? []).filter(
    (entry) => !needle || `${entry.ident ?? ""} ${entry.message}`.toLowerCase().includes(needle)
  )
  const followFrom = page?.cursor || (output ? "1970-01-01T00:00:00Z" : "")
  const canFollow = current && !!page && !!followFrom && !(output && !stack)

  return (
    <Panel
      title={logSource(source).label}
      action={
        canFollow &&
        (follow.state === "on" || follow.state === "lost" ? (
          <Button variant="outline" size="sm" onClick={() => stopFollowing()}>
            <Pause aria-hidden />
            Stop following
          </Button>
        ) : (
          <Button variant="outline" size="sm" onClick={() => startFollowing(followFrom)}>
            <Play aria-hidden />
            Follow
          </Button>
        ))
      }
    >
      <ErrorNote>{indexError}</ErrorNote>
      <div className="grid gap-3 sm:grid-cols-2">
        <Select label="Log" value={source} onChange={setSource} options={sources} disabled={!index} />
        <Select
          label="Start"
          value={output ? "" : boot}
          onChange={setBoot}
          options={boots.length ? boots : [{ value: "", label: "This start" }]}
          disabled={!index || output}
        />
      </div>
      <div className="flex flex-wrap items-center gap-x-4 gap-y-2">
        {!output && (
          <Label className="font-normal">
            <Checkbox checked={important} onCheckedChange={(checked) => setImportant(checked === true)} />
            Only warnings and errors
          </Label>
        )}
        <Input
          type="search"
          aria-label="Find in these lines"
          placeholder="Find in these lines"
          value={search}
          onChange={(event) => setSearch(event.target.value)}
          className="h-8 min-w-0 flex-1 basis-48"
        />
      </div>
      {logSource(source).hint && <Muted>{logSource(source).hint}</Muted>}
      {output && !stack && <Muted>No app has been turned on, so there&apos;s nothing it printed.</Muted>}
      {output && page?.absent === "disk" && <Muted>The data disk isn&apos;t in use, so no app is.</Muted>}
      {output && page?.absent === "container" && (
        <Muted>{stack} has no output now: it goes when the app is turned off.</Muted>
      )}
      {page?.gap && <Notice level="warn">Some lines came and went faster than this page could follow them.</Notice>}
      <ErrorNote>{error}</ErrorNote>
      {follow.state === "lost" && <Notice level="warn">Lost the unit for a moment. Trying again…</Notice>}
      {follow.state === "ended" && (
        <Muted>
          {follow.reason === "time"
            ? "Stopped following after 30 minutes. Follow again to carry on."
            : follow.reason === "login"
              ? "Stopped following: you're logged out."
              : "The unit stopped following. Too many may be following logs on it now."}
        </Muted>
      )}
      {paused && <Muted>Paused while this page is hidden.</Muted>}
      {!page && busy && (
        <div role="status" className="flex justify-center py-8">
          <Spinner />
          <span className="sr-only">Loading</span>
        </div>
      )}
      {page && (
        <>
          <ol
            ref={box}
            aria-label="Log lines"
            className="max-h-[65svh] overflow-auto rounded-lg border bg-muted/30 p-2 font-mono text-xs leading-relaxed"
          >
            {shown.length === 0 && (
              <li className="p-2 text-muted-foreground">{needle ? "No line has that in it." : "Nothing here."}</li>
            )}
            {shown.map((entry, i) => (
              <Line key={entry.cursor ? `${entry.cursor}-${entry.stream ?? ""}-${i}` : i} entry={entry} />
            ))}
          </ol>
          <div className="flex flex-wrap items-center justify-between gap-2">
            <p className="text-xs text-muted-foreground">
              {needle ? `${shown.length} of ${page.entries.length} lines` : `${page.entries.length} lines`}, oldest
              first, in this browser&apos;s time by the unit&apos;s clock.
            </p>
            {page.more && count < COUNTS[COUNTS.length - 1] && follow.state !== "on" && (
              <Button
                variant="outline"
                size="sm"
                disabled={busy}
                onClick={() => setCount(COUNTS[COUNTS.indexOf(count) + 1])}
              >
                {busy ? "Loading…" : "Show older lines"}
              </Button>
            )}
          </div>
        </>
      )}
    </Panel>
  )
}

function Line({ entry }: { entry: LogEntry }) {
  const at = new Date(entry.time_us / 1000)
  const who = entry.ident ?? entry.stream ?? ""
  return (
    <li
      className={cn(
        "grid gap-x-2 px-1 py-0.5 sm:grid-cols-[auto_1fr] sm:py-px",
        entry.priority <= 3 && "text-destructive",
        entry.priority === 4 && "text-warn"
      )}
    >
      <time dateTime={at.toISOString()} title={at.toString()} className="text-muted-foreground tabular-nums">
        {timeFormat.format(at)}
      </time>
      <span className="min-w-0 break-words whitespace-pre-wrap">
        {who && (
          <span className="text-muted-foreground">
            {who}
            {entry.pid ? `[${entry.pid}]` : ""}:{" "}
          </span>
        )}
        {entry.message}
      </span>
    </li>
  )
}

function SupportBundle() {
  const { identity } = useIdentity()
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [done, setDone] = useState(false)

  async function make() {
    setBusy(true)
    setError(null)
    setDone(false)
    try {
      const now = new Date()
      const stamp = `${now.getFullYear()}-${String(now.getMonth() + 1).padStart(2, "0")}-${String(now.getDate()).padStart(2, "0")}`
      await download("/api/v1/support-bundle", `${identity.name}-support-${stamp}.zip`)
      setDone(true)
    } catch (failure) {
      setError(plainError(failure))
    } finally {
      setBusy(false)
    }
  }

  return (
    <Panel title="Support bundle">
      <Muted>Logs and settings as a ZIP, for whoever is helping you. Keys, passwords and what apps printed stay out.</Muted>
      <ErrorNote>{error}</ErrorNote>
      {done && <Notice level="good">Downloaded.</Notice>}
      <div>
        <Button variant="outline" disabled={busy} onClick={make}>
          {busy ? <Spinner className="size-4" /> : <Download aria-hidden />}
          {busy ? "Making it…" : "Download"}
        </Button>
      </div>
    </Panel>
  )
}

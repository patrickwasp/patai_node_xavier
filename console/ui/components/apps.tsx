"use client"

import { useCallback, useEffect, useState } from "react"
import Link from "next/link"

import { Choice } from "@/components/choice"
import { CopyButton, Field, Muted, Notice, ProgressBar, Row, Spinner, tones } from "@/components/common"
import { FormDialog } from "@/components/form-dialog"
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
import { buttonVariants, Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { api, jobFinished, plainError, sendFile } from "@/lib/api"
import { bytes, capitalise, duration } from "@/lib/format"
import type { Level } from "@/lib/health"
import { field, useJob, usePoll } from "@/lib/hooks"
import { jobTitle } from "@/lib/labels"
import type { App, Job, WorkloadFetch, WorkloadModel, WorkloadPart, WorkloadService, WorkloadStack, WorkloadStatus } from "@/lib/types"
import { cn } from "@/lib/utils"

const WATCHING_MS = 2_000
const RESTING_MS = 10_000

/** Where to reach an app: its own web pages on a port of their own, or its APIs through the console's gateway. */
export function address(app: App): { url: string; note: string; open: boolean } | null {
  if (app.ui) {
    return { url: `https://${window.location.hostname}:${app.ui.port}/`, note: "Each person signs in with their own account.", open: true }
  }
  if (app.apis.length > 0 && !app.key) {
    return { url: `https://${window.location.host}/v1`, note: "OpenAI-compatible. Programs send an API key as a bearer token.", open: false }
  }
  return null
}

const FETCHING: WorkloadFetch["step"][] = ["image", "model", "checking", "loading"]

/** The download going on for one of these services, if there is one. */
export function fetching(fetch: WorkloadFetch | null | undefined, services: string[]): WorkloadFetch | null {
  return fetch && services.includes(fetch.stack) && FETCHING.includes(fetch.step) ? fetch : null
}

/** The services an app runs now, as the status has them. */
export function stacksOf(app: App, info: WorkloadStatus): WorkloadStack[] {
  return app.running.map((name) => info.stacks.find((stack) => stack.name === name)).filter((stack) => stack !== undefined)
}

/** What an app is doing, from its services as they run now. */
export function appState(app: App, info: WorkloadStatus): { text: string; level: Level } {
  if (fetching(info.fetch, app.running)) return { text: "Downloading", level: "unknown" }
  if (!app.on) return { text: "Off", level: "unknown" }
  const stacks = stacksOf(app, info)
  const running = stacks.filter((stack) => stack.services.some((service) => service.state === "running"))
  if (running.length === 0) {
    if (info.fetch?.step === "failed" && app.running.includes(info.fetch.stack)) return { text: "Couldn't start", level: "warn" }
    return { text: "Starting", level: "unknown" }
  }
  if (running.length < app.running.length) {
    const down = stacks.filter((stack) => !running.includes(stack)).map((stack) => serviceTitle(app, stack.name))
    return { text: `Partly running: ${down.join(", ")} not yet`, level: "warn" }
  }
  if (stacks.some((stack) => stack.services.some((service) => service.healthy === false))) {
    return { text: "Running, but not answering yet", level: "warn" }
  }
  return { text: "Running and answering", level: "good" }
}

/** A service by what it does for the app: the API it serves, or its own title. */
export function serviceTitle(app: App, service: string, stack?: WorkloadStack): string {
  const api = app.apis.find((item) => item.service === service)
  return api ? api.title : (stack?.title ?? service)
}

/** The status as it was last read, so a page opened from another shows it at once while it asks again. */
let lastInfo: WorkloadStatus | null = null

/** The apps and how they're doing, kept fresh, with the jobs that start and stop them. */
export function useApps() {
  const session = useSession()
  const [info, setInfo] = useState<WorkloadStatus | null>(lastInfo)
  const [infoError, setInfoError] = useState<string | null>(null)
  const reload = useCallback(async () => {
    try {
      lastInfo = await api<WorkloadStatus>("/api/v1/workloads/status")
      setInfo(lastInfo)
      setInfoError(null)
    } catch (failure) {
      setInfoError(plainError(failure))
    }
  }, [])
  const { job, busy, error, start, follow } = useJob(() => void reload())
  const starting = info?.apps.some((app) => {
    const state = appState(app, info).text
    return state === "Starting" || state === "Downloading" || state.startsWith("Partly") || state.startsWith("Running, but")
  })
  usePoll(reload, info?.busy || busy || starting ? WATCHING_MS : RESTING_MS)

  // Pick up a job that was already going when this page opened.
  useEffect(() => {
    api<Job[]>("/api/v1/jobs")
      .then((jobs) => {
        const open = jobs.find((item) => /^(workload|model|app)-/.test(item.kind) && !jobFinished(item))
        if (open) void follow(open)
      })
      .catch(() => {})
  }, [follow])

  return { info, infoError, reload, job, busy, error, start, operate: canOperate(session), owner: isOwner(session) }
}

export type Apps = ReturnType<typeof useApps>

/** Turn on and off, and what's in the way: a download or a failure. */
export function AppControls({ app, apps }: { app: App; apps: Apps }) {
  const { info, job, busy, start, operate } = apps
  if (!info) return null
  const stacks = stacksOf(app, info)
  const fetch = info.fetch && app.running.includes(info.fetch.stack) ? info.fetch : null
  const downloading = fetching(info.fetch, app.running)
  const missing = stacks.some((stack) => !stack.image.here || stack.models.some((model) => model.kept && !model.here))
  const ready = info.disk === "ready"
  const base = `/api/v1/apps/${encodeURIComponent(app.name)}`
  const account = accountFor(app, info)
  return (
    <>
      {downloading && <Downloading fetch={downloading} />}
      {fetch?.step === "failed" && !fetch.adding && (
        <Notice level="warn" title={`${serviceTitle(app, fetch.stack)} couldn't get ready`}>
          <p>{capitalise(fetch.detail)}.</p>
          {app.on && <p className="mt-1">The unit tries again every 5 minutes.</p>}
        </Notice>
      )}
      {fetch?.step === "stopped" && <Muted>The download stopped. It carries on from there next time.</Muted>}
      {operate && (
        <div className="grid gap-1.5">
          {app.on ? (
            <Button variant="outline" disabled={busy} onClick={() => start(`${base}/stop`)}>
              Turn off
            </Button>
          ) : account ? (
            <OwnerAccount app={app} stack={account} disabled={busy || !ready} onDone={() => start(`${base}/start`)} />
          ) : (
            <Button disabled={busy || !ready} onClick={() => start(`${base}/start`)}>
              Turn on
            </Button>
          )}
          {!app.on && downloading && (
            <Button variant="outline" disabled={busy} onClick={() => start(`${base}/fetch/cancel`)}>
              Stop downloading
            </Button>
          )}
          {!app.on && !downloading && missing && (
            <Button variant="outline" disabled={busy || !ready} onClick={() => start(`${base}/fetch`)}>
              Download only
            </Button>
          )}
        </div>
      )}
      {job?.target === app.name && (job.kind.includes("fetch") ? job.state === "failed" : true) && <JobResult job={job} />}
    </>
  )
}

/** The service that makes the app's owner account at its first start, while that's still to be given: its secrets
 * given once, none of them used. */
function accountFor(app: App, info: WorkloadStatus): WorkloadStack | null {
  for (const name of app.services ?? []) {
    const stack = info.stacks.find((item) => item.name === name)
    const once = stack?.secrets.filter((secret) => secret.once) ?? []
    if (stack && once.length > 0 && !once.some((secret) => secret.used) && !once.every((secret) => secret.set)) return stack
  }
  return null
}

/** Turn on, giving the owner's account first: the app makes it at its first start, and the node keeps it only until
 * then. */
function OwnerAccount({ app, stack, disabled, onDone }: { app: App; stack: WorkloadStack; disabled: boolean; onDone: () => Promise<void> }) {
  const [open, setOpen] = useState(false)
  const once = stack.secrets.filter((secret) => secret.once)
  const email = once.find((secret) => secret.name.endsWith("email"))
  const username = once.find((secret) => secret.name.endsWith("username"))
  const password = once.find((secret) => secret.name.endsWith("password"))
  return (
    <>
      <Button disabled={disabled} onClick={() => setOpen(true)}>
        Turn on
      </Button>
      <FormDialog
        open={open}
        onOpenChange={setOpen}
        title={`Your ${app.title} account`}
        description="The owner's account. You add everyone else in the app."
        submit="Turn on"
        action={async (form) => {
          const given = field(form, "password")
          if (given !== field(form, "again")) throw new Error("The passwords don't match.")
          for (const [secret, value] of [
            [email, email && field(form, "email").trim()],
            [username, username && field(form, "username").trim()],
            [password, given],
          ] as const) {
            if (!secret || !value) continue
            await api(`/api/v1/workloads/${encodeURIComponent(stack.name)}/secrets/${encodeURIComponent(secret.name)}`, {
              method: "PUT",
              body: { value },
            })
          }
          setOpen(false)
          await onDone()
        }}
      >
        {email && <Field label="Email" name="email" type="email" autoComplete="off" required />}
        {username && (
          <Field label="Username" name="username" autoComplete="off" pattern="[A-Za-z0-9_.]+" hint="Letters, digits, underscores and dots." required />
        )}
        <Field label="Password" name="password" type="password" autoComplete="new-password" minLength={8} required />
        <Field label="Password again" name="again" type="password" autoComplete="new-password" required />
      </FormDialog>
    </>
  )
}

/** Where to reach an app, if it has a way in. */
export function Reach({ app }: { app: App }) {
  const reach = address(app)
  if (!reach) return null
  return (
    <div className="grid gap-1.5">
      <code className="font-mono text-sm select-all">{reach.url}</code>
      <p className="text-xs text-muted-foreground">{reach.note}</p>
      {reach.open ? (
        <a href={reach.url} target="_blank" rel="noreferrer" className={buttonVariants({ variant: "outline" })}>
          Open {app.title}
        </a>
      ) : (
        <CopyButton text={reach.url} label="Copy address" />
      )}
    </div>
  )
}

/** An app's APIs, each on or off, with the model it runs and how its service is doing. */
export function APIs({ app, apps }: { app: App; apps: Apps }) {
  const { info, busy, start, operate, job } = apps
  if (!info) return null
  return (
    <ul className="grid divide-y">
      {app.apis.map((item) => {
        const on = item.always || app.apis_on.includes(item.name)
        const stack = info.stacks.find((candidate) => candidate.name === item.service)
        const model = stack?.models.find((candidate) => candidate.in_use) ?? (stack && !stack.one_model ? stack.models.find((m) => m.kept) : undefined)
        const service = stack?.services[0]
        const people = info.people?.[item.service]?.length ?? 0
        const words = [
          !app.key && item.routes.join(", "),
          model && (model.title || model.name),
          app.on && on && item.person && (people ? `Running for ${people} ${people === 1 ? "person" : "people"}` : "Running for nobody now"),
          app.on && on && !item.person && service && (service.state === "running" ? (service.healthy ? "answering" : "not answering yet") : "not running"),
        ].filter(Boolean)
        const path = `/api/v1/apps/${encodeURIComponent(app.name)}/apis/${encodeURIComponent(item.name)}`
        return (
          <li key={item.name} className="grid gap-2 py-2 first:pt-0 last:pb-0 sm:flex sm:items-start sm:justify-between">
            <div className="grid min-w-0 gap-0.5">
              <span className="font-medium">
                {item.title}
                <span className={cn("ml-2 text-xs font-normal", on ? tones.good.text : "text-muted-foreground")}>
                  {item.always ? "Always on" : on ? "On" : "Off"}
                </span>
              </span>
              <span className="text-xs wrap-anywhere text-muted-foreground">{words.join(" · ")}</span>
              {item.about && <span className="text-xs text-muted-foreground">{item.about}</span>}
            </div>
            {operate && !item.always && (
              <Button size="sm" variant="outline" className="shrink-0" disabled={busy} onClick={() => void start(`${path}/${on ? "off" : "on"}`)}>
                {on ? "Turn off" : "Turn on"}
              </Button>
            )}
          </li>
        )
      })}
      {job?.kind.startsWith("app-api-") && job.target.startsWith(`${app.name}/`) && <JobResult job={job} />}
    </ul>
  )
}

/** The services an app runs, each with its state, health and memory. */
export function Services({ app, info }: { app: App; info: WorkloadStatus }) {
  const stacks = stacksOf(app, info)
  if (stacks.length === 0) return <Muted>None.</Muted>
  if (stacks.every((stack) => stack.services.length === 0)) return <Muted>The data disk isn&apos;t in use, so nothing runs.</Muted>
  return (
    <ul className="grid divide-y">
      {stacks.flatMap((stack) => {
        // A service runs once for every app that needs it.
        const others = info.apps.filter((other) => other.on && other.name !== app.name && other.running.includes(stack.name))
        const note = others.length > 0 ? `Also for ${others.map((other) => other.title).join(", ")}` : undefined
        return stack.services.map((service) => (
          <ServiceRow key={stack.name + service.name} app={stack.name} title={serviceTitle(app, stack.name, stack)} note={note} service={service} />
        ))
      })}
    </ul>
  )
}

const SERVICE_STATES: Record<string, string> = {
  running: "Running",
  restarting: "Restarting",
  exited: "Stopped",
  created: "Made, not started",
  paused: "Paused",
  dead: "Failed",
  absent: "Not started",
}

function ServiceRow({ app, title, note, service }: { app: string; title: string; note?: string; service: WorkloadService }) {
  const running = service.state === "running"
  const level: Level = !running ? "unknown" : service.healthy === false ? "warn" : "good"
  const health = running ? (service.healthy ? "answering" : "not answering yet") : null
  const since = service.started_at ? (Date.now() - Date.parse(service.started_at)) / 1000 : null
  const memory = [
    service.memory !== null && `${bytes(service.memory)} of its own`,
    service.gpu_memory !== null && `${bytes(service.gpu_memory)} GPU`,
  ].filter(Boolean)
  return (
    <li className="grid gap-1 py-2 first:pt-0 last:pb-0">
      <div className="flex items-baseline justify-between gap-3">
        <span className="font-medium">{title}</span>
        <span className={cn("shrink-0 text-sm", tones[level].text)}>
          {SERVICE_STATES[service.state] ?? capitalise(service.state)}
          {health && `, ${health}`}
        </span>
      </div>
      {note && <p className="text-xs text-muted-foreground">{note}</p>}
      <dl className="grid gap-1 text-sm">
        <Row label="Image">
          <span className="font-mono text-xs wrap-anywhere">{service.image}</span>
        </Row>
        {since !== null && since > 0 && <Row label="Up for">{duration(since)}</Row>}
        {memory.length > 0 && <Row label="Memory">{memory.join(", ")}</Row>}
        {service.restarts > 0 && <Row label="Restarts">{service.restarts}</Row>}
      </dl>
      {running && (
        <Link
          href={`/logs/?source=output&app=${encodeURIComponent(app)}`}
          prefetch={false}
          className="text-sm text-muted-foreground underline underline-offset-4"
        >
          What it printed
        </Link>
      )}
    </li>
  )
}

export function partState(part: WorkloadPart): string {
  if (part.here) return "On this unit"
  if (part.unchecked) return "On this unit, to be checked"
  if (part.partial) return `${bytes(part.partial)} of ${bytes(part.size)} so far`
  return `${bytes(part.size)} to download`
}

function modelState(model: WorkloadModel): string {
  if (model.here) return `${bytes(model.size)}, on this unit`
  if (model.unchecked) return "On this unit, to be checked"
  if (model.partial) return `${bytes(model.partial)} of ${bytes(model.size)} so far`
  if (model.kept) return `${bytes(model.size)}, downloads at the next start`
  return `${bytes(model.size)}, not downloaded`
}

/** An app's models, each to use, download or remove. Adding one is AddModel, beside it. */
export function Models({
  stack,
  operate,
  busy,
  start,
}: {
  stack: WorkloadStack
  operate: boolean
  busy: boolean
  start: (path: string) => Promise<void>
}) {
  const base = `/api/v1/workloads/${encodeURIComponent(stack.name)}/models`
  if (stack.models.length === 0) return <Muted>None yet.</Muted>
  return (
    <ul className="grid divide-y">
      {stack.models.map((model) => {
        const path = `${base}/${encodeURIComponent(model.name)}`
        const words = [
          model.title ? model.name : null,
          model.own ? (model.source ? `yours, from ${model.source}` : "yours, uploaded") : null,
          modelState(model),
        ]
        return (
          <li key={model.name} className="grid gap-2 py-2 first:pt-0 last:pb-0 sm:flex sm:items-start sm:justify-between">
            <div className="grid min-w-0 gap-0.5">
              <span className="font-medium wrap-anywhere">
                {model.title || model.name}
                {model.in_use && <span className={cn("ml-2 text-xs font-normal", tones.good.text)}>In use</span>}
              </span>
              <span className="text-xs wrap-anywhere text-muted-foreground">{words.filter(Boolean).join(" · ")}</span>
            </div>
            {operate && !model.in_use && (
              <div className="flex shrink-0 gap-1.5">
                {stack.one_model && (
                  <Button size="sm" variant="outline" disabled={busy} onClick={() => void start(`${path}/use`)}>
                    Use
                  </Button>
                )}
                {!model.here && !model.partial && (
                  <Button size="sm" variant="outline" disabled={busy} onClick={() => void start(`${path}/fetch`)}>
                    Download
                  </Button>
                )}
                {(model.here || model.kept || model.own) && (
                  <RemoveModel name={model.title || model.name} disabled={busy} onConfirm={() => void start(`${path}/remove`)} />
                )}
              </div>
            )}
          </li>
        )
    })}
    </ul>
  )
}

function RemoveModel({ name, disabled, onConfirm }: { name: string; disabled: boolean; onConfirm: () => void }) {
  const [asking, setAsking] = useState(false)
  return (
    <AlertDialog open={asking} onOpenChange={setAsking}>
      <AlertDialogTrigger render={<Button size="sm" variant="ghost" disabled={disabled} />}>Remove</AlertDialogTrigger>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>Remove {name}?</AlertDialogTitle>
          <AlertDialogDescription>It&apos;s deleted from this unit.</AlertDialogDescription>
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
            Remove
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  )
}

const FORMAT_NAMES: Record<string, { label: string; extension: string }> = {
  gguf: { label: "GGUF", extension: ".gguf" },
  safetensors: { label: "safetensors", extension: ".safetensors" },
}

export function AddModel({ stack, onAdded }: { stack: WorkloadStack; onAdded: () => Promise<void> }) {
  const [open, setOpen] = useState(false)
  const [from, setFrom] = useState("address")
  const [sent, setSent] = useState<number | null>(null)
  const formats = stack.formats.map((form) => FORMAT_NAMES[form]).filter(Boolean)
  const kinds = formats.map((form) => form.label).join(" or ")
  const extensions = formats.map((form) => form.extension).join(",")
  const base = `/api/v1/workloads/${encodeURIComponent(stack.name)}/models`
  return (
    <>
      <Button size="sm" variant="outline" onClick={() => setOpen(true)}>
        Add a model
      </Button>
      <FormDialog
        open={open}
        onOpenChange={(next) => {
          setOpen(next)
          setSent(null)
        }}
        title={`Add a model to ${stack.title}`}
        description={`${kinds} only.`}
        submit="Add it"
        action={async (form) => {
          if (from === "address") {
            const url = field(form, "url").trim()
            const name = field(form, "name").trim() || decodeURIComponent(url.split("?")[0].split("/").pop() ?? "")
            await api(base, { method: "POST", body: { name, url } })
          } else {
            const file = form.get("file")
            if (!(file instanceof File) || file.size === 0) throw new Error("Choose a file.")
            // Before it's sent: a model can take a long time to arrive only to be refused for its name.
            if (!formats.some((form) => file.name.endsWith(form.extension))) {
              throw new Error(`Its name has to end in ${formats.map((form) => form.extension).join(" or ")}.`)
            }
            setSent(0)
            try {
              await sendFile(`${base}/upload/${encodeURIComponent(file.name)}`, file, setSent, "PUT")
            } finally {
              setSent(null)
            }
          }
          setOpen(false)
          await onAdded()
        }}
      >
        <Choice
          name="from"
          label="From"
          defaultValue={from}
          onChange={setFrom}
          options={[
            { value: "address", label: "An address", hint: "The unit downloads it." },
            { value: "file", label: "A file", hint: "From this computer." },
          ]}
        />
        {from === "address" ? (
          <>
            <Field label="Address" name="url" type="url" inputMode="url" placeholder="https://" required />
            <Field label="Name" name="name" placeholder={`From the address, ending in ${formats[0]?.extension ?? ""}`} />
          </>
        ) : (
          <Input name="file" type="file" accept={extensions} aria-label="Model file" className="h-auto py-2" required />
        )}
        {sent !== null && <ProgressBar percent={Math.round(sent * 100)} label="How much of the file has been sent" />}
      </FormDialog>
    </>
  )
}

export function Downloading({ fetch }: { fetch: WorkloadFetch }) {
  if (fetch.step === "checking" || fetch.step === "loading") {
    return (
      <div role="status" className="flex items-center gap-2 text-sm">
        <Spinner className="size-4" />
        {fetch.step === "checking" ? "Checking it" : "Unpacking its software"}
      </div>
    )
  }
  const what = fetch.step === "model" ? "the model" : "its software"
  const percent = fetch.total > 0 ? Math.round((fetch.done / fetch.total) * 100) : 0
  return (
    <div className="grid gap-1.5">
      {fetch.total > 0 && <ProgressBar percent={percent} label={`How much of ${what} has arrived`} />}
      <p className="text-sm text-muted-foreground">
        Downloading {what} from {fetch.source}: {bytes(fetch.done)}
        {fetch.total > 0 ? ` of ${bytes(fetch.total)}` : " so far"}
      </p>
    </div>
  )
}

export function JobResult({ job }: { job: Job }) {
  const title = jobTitle(job)
  if (!jobFinished(job)) {
    return (
      <div role="status" className="flex items-center gap-2 text-sm">
        <Spinner className="size-4" />
        {title}: {job.state === "queued" ? "waiting its turn" : "in progress"}…
      </div>
    )
  }
  if (job.state === "failed") {
    return (
      <Notice level="bad" title={`${title}: that didn't work`}>
        {job.result && <p className="font-mono text-xs">{job.result}</p>}
      </Notice>
    )
  }
  return (
    <Notice level="good" title={`${title}: done`}>
      The state shown here catches up within a minute.
    </Notice>
  )
}

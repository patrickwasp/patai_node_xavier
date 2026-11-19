"use client"

import { useCallback, useEffect, useMemo, useState } from "react"

import { ErrorNote, Field, Muted, Notice, PageTitle, Panel, ProgressBar, Row, Spinner, tones } from "@/components/common"
import { columnsOf, DataTable } from "@/components/data-table"
import { CurrentPassword, FormDialog } from "@/components/form-dialog"
import { useIdentity } from "@/components/gate"
import { RestartButton, Restarting, useRestart } from "@/components/restart"
import { Rollbacks } from "@/components/rollbacks"
import { canOperate, isOwner, useSession } from "@/components/session"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { api, plainError, sendFile, startJob } from "@/lib/api"
import { bytes, when } from "@/lib/format"
import { field, useJob, usePoll, useStatus } from "@/lib/hooks"
import { installSource, installState } from "@/lib/labels"
import type { UpdateInstall, UploadedBundle, Updates } from "@/lib/types"

const WATCHING_MS = 2_000
const RESTING_MS = 30_000

type Sections = { updates: Updates; reload: () => Promise<void> }
type JobSections = Sections & { release: string; onBusy: (busy: boolean) => void }

export default function UpdatesPage() {
  const { identity } = useIdentity()
  const { status } = useStatus()
  const session = useSession()
  const restart = useRestart()
  const [updates, setUpdates] = useState<Updates | null>(null)
  const [error, setError] = useState<string | null>(null)
  // The unit says it's busy only once a job has got going, so a job here counts too.
  const [watching, setWatching] = useState(false)

  const reload = useCallback(async () => {
    try {
      setUpdates(await api<Updates>("/api/v1/updates"))
      setError(null)
    } catch (failure) {
      setError(plainError(failure))
    }
  }, [])
  usePoll(reload, updates?.busy || watching ? WATCHING_MS : RESTING_MS)

  const waiting = updates?.slots.find((slot) => slot.primary && !slot.booted)

  return (
    <>
      <PageTitle>Updates</PageTitle>
      {restart.restarting && (
        <Restarting slow={restart.slow}>
          About two minutes. This page carries on by itself.
        </Restarting>
      )}
      <ErrorNote>{error}</ErrorNote>
      <ErrorNote>{restart.error}</ErrorNote>
      {updates?.installing && <Installing updates={updates} />}
      {updates && !updates.busy && waiting && (
        <Notice level="good" title={`${waiting.version || "A new release"} is installed`}>
          <div className="grid justify-items-start gap-3">
            <p>Starts at the next restart. If it fails, the unit goes back to {identity.release}.</p>
            {canOperate(session) && (
              <RestartButton
                label="Restart now"
                title="Restart into the new release?"
                variant="default"
                onConfirm={() => void restart.restart()}
              >
                Apps stop, and the console is away for about two minutes.
              </RestartButton>
            )}
          </div>
        </Notice>
      )}
      {updates && (
        <div className="grid items-start gap-4 md:grid-cols-2">
          <Releases updates={updates} release={identity.release} />
          <NewReleases updates={updates} reload={reload} release={identity.release} onBusy={setWatching} />
          <Channel updates={updates} reload={reload} />
          {isOwner(session) && (
            <FromFile updates={updates} reload={reload} release={identity.release} onBusy={setWatching} />
          )}
          {status?.report && <Rollbacks report={status.report} wide />}
        </div>
      )}
      {updates && <History updates={updates} />}
    </>
  )
}

function Installing({ updates }: { updates: Updates }) {
  const version = updates.history[0]?.version
  return (
    <Panel title={version ? `Installing ${version}` : "Installing an update"}>
      <ProgressBar percent={updates.percent} label="How far the install has got" />
      {updates.step && <Muted>{updates.step.replace(/\.+$/, "")}</Muted>}
    </Panel>
  )
}

function Releases({ updates, release }: { updates: Updates; release: string }) {
  const other = updates.slots.find((slot) => !slot.booted)
  const waiting = other?.primary
  let previous = "Not known"
  if (other?.state === "bad") previous = "None"
  else if (other) previous = other.version || "The one it was set up with"
  return (
    <Panel title="Releases">
      <dl className="grid gap-1.5">
        <Row label="Running">{release}</Row>
        {!updates.installing && !waiting && <Row label="Previous">{previous}</Row>}
      </dl>
    </Panel>
  )
}

function NewReleases({ updates, reload, release, onBusy }: JobSections) {
  const session = useSession()
  const { job, busy, error, start } = useJob(() => void reload())
  useEffect(() => onBusy(busy), [busy, onBusy])

  const check = updates.check
  const offered = check?.offered && check.offered !== release ? check.offered : null
  const installed = updates.slots.some((slot) => slot.primary && !slot.booted && slot.version === offered)
  const tried = offered !== null && offered === updates.tried && !installed
  const working = busy || updates.busy

  return (
    <Panel title="New releases">
      {!updates.channel && <Muted>This unit has no update channel, so it doesn&apos;t look for new releases.</Muted>}
      {updates.channel && !check && <Muted>Not checked yet.</Muted>}
      {check && !check.ok && (
        <Notice level="warn" title="The last check didn't work">
          {check.detail}
        </Notice>
      )}
      {check?.ok && !offered && <Muted>This unit is up to date.</Muted>}
      {offered && installed && <Muted>{offered} is installed and waiting for a restart.</Muted>}
      {offered && !installed && !tried && (
        <p>
          <span className="font-medium">{offered}</span> is available.
        </p>
      )}
      {tried && (
        <Notice level="warn" title={`${offered} was installed before and didn't stay`}>
          This unit went back from it, so it won&apos;t install it by itself again.
        </Notice>
      )}
      <ErrorNote>{error}</ErrorNote>
      {job?.state === "failed" && <ErrorNote>{job.result || "That didn't work."}</ErrorNote>}
      {(check?.time || (canOperate(session) && updates.channel)) && (
        <div className="flex flex-wrap items-center gap-x-3 gap-y-2">
          {canOperate(session) && updates.channel && offered && !installed && (
            <Button disabled={working} onClick={() => void start("/api/v1/updates/install", { version: offered })}>
              {tried ? "Install it again" : `Install ${offered}`}
            </Button>
          )}
          {canOperate(session) && updates.channel && (
            <Button variant="outline" disabled={working} onClick={() => void start("/api/v1/updates/check")}>
              {busy && !updates.installing && <Spinner className="size-4" />}
              Check now
            </Button>
          )}
          {check?.time && <span className="text-xs text-muted-foreground">Last checked {when(check.time)}</span>}
        </div>
      )}
    </Panel>
  )
}

function Channel({ updates, reload }: Sections) {
  const session = useSession()
  const [asking, setAsking] = useState(false)
  const { job, error, start } = useJob(() => void reload())
  return (
    <Panel title="Update channel">
      <dl className="grid gap-1.5">
        <Row label="Address">
          {updates.channel ? <code className="font-mono text-xs break-all">{updates.channel}</code> : "None"}
        </Row>
        {updates.channel && (
          <>
            <Row label="Name">{updates.channel_name || "None, so nothing installs"}</Row>
            <Row label="New releases">{updates.auto_install ? "Install by themselves" : "Wait for a person"}</Row>
            <Row label="Then">{updates.auto_restart ? "Restart straight away" : "Wait for the next restart"}</Row>
          </>
        )}
      </dl>
      <ErrorNote>{error}</ErrorNote>
      {job?.state === "failed" && <ErrorNote>{job.result || "The check didn't work."}</ErrorNote>}
      {isOwner(session) && (
        <>
          <Button variant="outline" className="justify-self-start" onClick={() => setAsking(true)}>
            Change
          </Button>
          <FormDialog
            open={asking}
            onOpenChange={setAsking}
            title="Update channel"
            description="The file server this unit gets its releases and apps from."
            submit="Save"
            action={async (form) => {
              const channel = field(form, "channel").trim()
              await api("/api/v1/updates/settings", {
                method: "PUT",
                body: {
                  channel,
                  channel_name: field(form, "channel_name").trim(),
                  auto_install: field(form, "auto_install") !== "",
                  auto_restart: field(form, "auto_restart") !== "",
                  current_password: field(form, "current_password"),
                },
              })
              setAsking(false)
              await reload()
              if (channel) void start("/api/v1/updates/check")
            }}
          >
            <Field
              label="Address"
              name="channel"
              defaultValue={updates.channel}
              placeholder="http://files.example.lan/patai"
              hint="The folder with latest.json. Empty for none."
              inputMode="url"
              autoCapitalize="none"
              autoCorrect="off"
              spellCheck={false}
            />
            <Field
              label="Name"
              name="channel_name"
              defaultValue={updates.channel_name}
              placeholder="stable"
              hint="Given with the address."
              autoCapitalize="none"
              autoCorrect="off"
              spellCheck={false}
            />
            <div className="grid gap-2">
              <Label className="items-center gap-3 leading-snug">
                <Checkbox name="auto_install" defaultChecked={updates.auto_install} />
                Install new releases by themselves
              </Label>
              <Label className="items-center gap-3 leading-snug">
                <Checkbox name="auto_restart" defaultChecked={updates.auto_restart} />
                Restart straight after, which stops any app
              </Label>
            </div>
            <CurrentPassword />
          </FormDialog>
        </>
      )}
    </Panel>
  )
}

function FromFile({ updates, reload, release, onBusy }: JobSections) {
  const [sent, setSent] = useState<number | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [asking, setAsking] = useState(false)
  const { job, busy, follow } = useJob(() => void reload())
  useEffect(() => onBusy(busy), [busy, onBusy])
  const upload = updates.upload

  async function send(file: File) {
    setError(null)
    setSent(0)
    try {
      await sendFile<UploadedBundle>("/api/v1/updates/upload", file, setSent)
      await reload()
    } catch (failure) {
      setError(plainError(failure))
    } finally {
      setSent(null)
    }
  }

  async function discard() {
    setError(null)
    try {
      await api("/api/v1/updates/upload", { method: "DELETE" })
    } catch (failure) {
      setError(plainError(failure))
    }
    await reload()
  }

  return (
    <Panel title="Install from a file">
      <ErrorNote>{error}</ErrorNote>
      {job?.state === "failed" && <ErrorNote>{job.result || "The install didn't work."}</ErrorNote>}
      {sent !== null && (
        <>
          <ProgressBar percent={Math.round(sent * 100)} label="How much of the file has been sent" />
          <Muted>{sent < 1 ? "Sending the file. Stay on this page until it's finished." : "Checking its signature."}</Muted>
        </>
      )}
      {sent === null && upload && (
        <>
          <p>
            <span className="font-medium">{upload.version}</span> is here, and its signature is good.{" "}
            <span className="text-muted-foreground">{bytes(upload.size)}</span>
          </p>
          <div className="flex flex-wrap gap-2">
            <Button disabled={busy || updates.busy} onClick={() => setAsking(true)}>
              Install {upload.version}
            </Button>
            <Button variant="outline" disabled={busy || updates.busy} onClick={() => void discard()}>
              Discard
            </Button>
          </div>
          <FormDialog
            open={asking}
            onOpenChange={setAsking}
            title={`Install ${upload.version}?`}
            description={`This unit runs ${release}. It doesn't restart by itself.`}
            submit="Install"
            action={async (form) => {
              const first = await startJob("/api/v1/updates/upload/install", {
                version: upload.version,
                current_password: field(form, "current_password"),
              })
              setAsking(false)
              await reload()
              void follow(first)
            }}
          >
            <CurrentPassword />
          </FormDialog>
        </>
      )}
      {sent === null && !upload && (
        <>
          <Muted>An update file (.raucb).</Muted>
          <Input
            type="file"
            aria-label="Update file"
            className="h-auto py-2"
            disabled={updates.busy}
            onChange={(event) => {
              const file = event.target.files?.[0]
              event.target.value = ""
              if (file) void send(file)
            }}
          />
        </>
      )}
    </Panel>
  )
}

const installColumns = columnsOf<UpdateInstall & { order: number }>()

const HISTORY = installColumns.columns([
  installColumns.accessor("version", { header: "Release", meta: { className: "font-medium" } }),
  installColumns.accessor((install) => installState(install.state), {
    id: "state",
    header: "State",
    meta: { facet: {} },
    cell: (cell) => {
      const state = cell.row.original.state
      return <span className={state === "failed" ? tones.bad.text : state === "ok" ? "" : tones.warn.text}>{cell.getValue()}</span>
    },
  }),
  installColumns.accessor((install) => installSource(install.source), { id: "source", header: "How", meta: { facet: {} } }),
  installColumns.accessor("time", {
    header: "When",
    enableGlobalFilter: false,
    // Newest first in the list, so a later one has a smaller place in it.
    sortFn: (a, b) => b.original.order - a.original.order,
    meta: { className: "text-muted-foreground" },
    cell: (cell) => when(cell.getValue()),
  }),
  installColumns.accessor((install) => (install.state === "failed" ? install.detail : ""), {
    id: "detail",
    header: "Why it failed",
    meta: { className: "font-mono text-xs whitespace-normal wrap-anywhere" },
  }),
])

function History({ updates }: { updates: Updates }) {
  // A unit may not know the date, so the order the unit lists installs in is the true one.
  const history = useMemo(() => updates.history.map((install, order) => ({ ...install, order })), [updates.history])
  if (history.length === 0) return null
  return (
    <Panel title="History">
      <DataTable
        id="installs"
        label="History"
        data={history}
        columns={HISTORY}
        rowId={(install) => String(install.order)}
        empty="Nothing yet."
        sort={[{ id: "time", desc: true }]}
      />
    </Panel>
  )
}

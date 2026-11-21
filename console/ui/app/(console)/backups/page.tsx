"use client"

import { type ChangeEvent, useEffect, useMemo, useRef, useState } from "react"
import { ArchiveRestore, Download, FileCheck, Play, RefreshCw } from "lucide-react"

import { Choice } from "@/components/choice"
import { CopyButton, ErrorNote, Field, Muted, Notice, PageTitle, Panel, ProgressBar, Row, Spinner, tones } from "@/components/common"
import { ActionsHeader, columnsOf, DataTable, RowActions } from "@/components/data-table"
import { CurrentPassword, FormDialog } from "@/components/form-dialog"
import { useIdentity } from "@/components/gate"
import { isOwner, useSession } from "@/components/session"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { DropdownMenuItem } from "@/components/ui/dropdown-menu"
import { Label } from "@/components/ui/label"
import { api, ApiError, FormError, jobFinished, plainError, startJob } from "@/lib/api"
import { downloadText } from "@/lib/browser"
import { bytes, when } from "@/lib/format"
import { field, sleep, usePoll } from "@/lib/hooks"
import type { Identity, Job } from "@/lib/types"

type Kind = "disk" | "smb" | "sftp" | "webdav" | "s3"

type Destination = {
  kind: Kind
  folder: string
  endpoint?: string
  region?: string
  bucket?: string
  access_key?: string
  path_style?: boolean
  host?: string
  share?: string
  domain?: string
  user?: string
  host_key?: string
  unit_key?: boolean
  url?: string
  disk?: string
  disk_name?: string
  has_secret: boolean
}

type Filesystem = { id: string; label?: string; type: string; size: number; usable: boolean; why?: string }
type Disk = { device: string; model?: string; transport?: string; size: number; filesystems: Filesystem[] | null }

const KINDS: { value: Kind; label: string; hint: string }[] = [
  { value: "disk", label: "Disk", hint: "A USB drive plugged into the unit" },
  { value: "smb", label: "Shared folder", hint: "SMB: Synology, QNAP, Windows, macOS" },
  { value: "sftp", label: "SFTP", hint: "A server over SSH, a Hetzner Storage Box" },
  { value: "webdav", label: "WebDAV", hint: "Nextcloud, ownCloud, a NAS, pCloud, Koofr" },
  { value: "s3", label: "Bucket", hint: "S3: Amazon, Backblaze B2, Cloudflare R2, Wasabi" },
]

function kindLabel(kind?: Kind): string {
  return KINDS.find((k) => k.value === kind)?.label ?? "Destination"
}

type Schedule = {
  on: boolean
  at: string
  zone: string
  keep_days: number
  keep_weeks: number
  keep_months: number
}

type Backups = {
  key: { id: string; made: string } | null
  destination: Destination | null
  where?: string
  ssh_key?: string
  schedule: Schedule
  next: string | null
  running: { job: number; phase: string; bytes: number; size?: number } | null
  pending_key: boolean
  last: Job | null
  last_good: string | null
  restored: { unit?: string; created?: string; asked?: string; platform?: string; skipped?: string[] | null } | null
}

type Listed = {
  name: string
  created: string
  unit?: string
  release?: string
  key_id?: string
  size: number
  contents?: number
  this_unit: boolean
  this_key: boolean
  record: boolean
}

const PHASES: Record<string, string> = {
  gathering: "Copying the unit's state",
  sending: "Sending",
  fetching: "Fetching",
  checking: "Checking",
  restoring: "Restoring",
}

function browserZone(): string {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC"
  } catch {
    return "UTC"
  }
}

export default function BackupsPage() {
  const session = useSession()
  const [state, setState] = useState<Backups | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [restoring, setRestoring] = useState<Job | null>(null)
  const [listVersion, setListVersion] = useState(0)

  async function load() {
    try {
      setState(await api<Backups>("/api/v1/backups"))
      setError(null)
    } catch (failure) {
      setError(plainError(failure))
    }
  }
  usePoll(load, state?.running ? 2_000 : 15_000)

  if (!isOwner(session)) {
    return (
      <>
        <PageTitle>Backups</PageTitle>
        <Muted>Only an owner manages backups.</Muted>
      </>
    )
  }
  if (restoring) return <Restoring job={restoring} />

  const ready = Boolean(state?.key && state?.destination)
  return (
    <>
      <PageTitle hint="Everything that makes this unit what it is, encrypted with a key only you keep.">Backups</PageTitle>
      <ErrorNote>{error}</ErrorNote>
      {state && !state.key && !state.destination && <Muted>To restore a backup, choose where it is.</Muted>}
      {state?.restored && (
        <Notice level="good" title={`Restored from ${state.restored.unit ?? "a backup"}`}>
          Backup of {when(state.restored.created)}, restored {when(state.restored.asked)}.
          {state.restored.skipped && state.restored.skipped.length > 0 && <> Not taken: {state.restored.skipped.join("; ")}.</>}
        </Notice>
      )}
      {state && (
        <div className="grid items-start gap-4 md:grid-cols-2">
          <KeyPanel state={state} onChange={setState} />
          <DestinationPanel state={state} onChange={(next) => {
            setState(next)
            setListVersion((v) => v + 1)
          }} />
          <SchedulePanel state={state} ready={ready} onChange={setState} />
          <LastPanel state={state} ready={ready} onRan={() => {
            void load()
            setListVersion((v) => v + 1)
          }} />
        </div>
      )}
      {state?.destination && <BackupList version={listVersion} state={state} onRestoring={setRestoring} />}
    </>
  )
}

function KeyPanel({ state, onChange }: { state: Backups; onChange: (s: Backups) => void }) {
  const [asking, setAsking] = useState(false)
  const [made, setMade] = useState<{ secret: string; id: string; unit: string } | null>(null)
  const [checking, setChecking] = useState(false)
  return (
    <Panel
      id="key"
      title="Backup key"
      action={
        state.key && (
          <Button size="sm" variant="ghost" onClick={() => setChecking(true)}>
            Check a key
          </Button>
        )
      }
    >
      {state.key ? (
        <dl className="grid gap-1.5">
          <Row label="Key">
            <code className="font-mono">{state.key.id}</code>
          </Row>
          <Row label="Made">{when(state.key.made)}</Row>
        </dl>
      ) : (
        <Muted>Backups are encrypted with a key you keep. The unit keeps only what encrypts, never what opens.</Muted>
      )}
      <Button variant="outline" onClick={() => setAsking(true)}>
        {state.key ? "Replace key" : "Make a key"}
      </Button>
      <FormDialog
        open={asking}
        onOpenChange={setAsking}
        title={state.key ? "Replace the backup key?" : "Make a backup key"}
        description={state.key ? "Backups made before stay with the old key." : undefined}
        submit="Make a key"
        action={async (form) => {
          const result = await api<{ secret: string; id: string; unit: string }>("/api/v1/backups/key", {
            method: "POST",
            body: { current_password: field(form, "current_password") },
          })
          setAsking(false)
          setMade(result)
        }}
      >
        <CurrentPassword />
      </FormDialog>
      {made && <KeyShown made={made} onDone={(s) => { setMade(null); onChange(s) }} onCancel={() => setMade(null)} />}
      <FormDialog
        open={checking}
        onOpenChange={setChecking}
        title="Check a key"
        submit="Check"
        action={async (form) => {
          const result = await api<{ id: string; matches: boolean }>("/api/v1/backups/key/check", {
            method: "POST",
            body: { secret: field(form, "secret") },
          })
          if (!result.matches) throw new FormError(`That's key ${result.id}, not this unit's.`)
          setChecking(false)
        }}
      >
        <KeyInput />
      </FormDialog>
    </Panel>
  )
}

function keyFile(made: { secret: string; id: string; unit: string }): string {
  return [
    `# patai backup key for ${made.unit}, key ${made.id}, made ${new Date().toISOString().slice(0, 10)}`,
    "# It opens this unit's backups, on any unit. Nobody else has it, and the unit keeps none of it.",
    "# Keep it in a password manager, and a copy away from the unit. Lost, the backups can't be opened.",
    "# Without a unit: age -d -i <this file> <backup> | zstd -d | tar -x",
    made.secret,
    "",
  ].join("\n")
}

/** The key, shown once. It's kept only once it comes back, which shows it was saved somewhere. */
function KeyShown({
  made,
  onDone,
  onCancel,
}: {
  made: { secret: string; id: string; unit: string }
  onDone: (s: Backups) => void
  onCancel: () => void
}) {
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  const [given, setGiven] = useState("")
  return (
    <Dialog open onOpenChange={(open) => !open && onCancel()}>
      <DialogContent>
        <form
          className="grid gap-4"
          onSubmit={async (event) => {
            event.preventDefault()
            setBusy(true)
            setError(null)
            try {
              onDone(await api<Backups>("/api/v1/backups/key/confirm", { method: "POST", body: { secret: given } }))
            } catch (failure) {
              setError(plainError(failure))
            } finally {
              setBusy(false)
            }
          }}
        >
          <DialogHeader>
            <DialogTitle>Save your backup key</DialogTitle>
            <DialogDescription>Shown once. Lost, its backups can&apos;t be opened.</DialogDescription>
          </DialogHeader>
          <code className="block rounded-lg border bg-muted p-3 font-mono text-xs break-all select-all">{made.secret}</code>
          <div className="grid gap-2 sm:grid-cols-2">
            <CopyButton text={made.secret} label="Copy key" />
            <Button type="button" variant="outline" onClick={() => downloadText(`${made.unit}-backup-key-${made.id}.txt`, keyFile(made))}>
              <Download aria-hidden />
              Download
            </Button>
          </div>
          <KeyInput label="Give it back from where you saved it" value={given} onValue={setGiven} />
          <ErrorNote>{error}</ErrorNote>
          <DialogFooter>
            <Button type="button" variant="outline" onClick={onCancel}>
              Cancel
            </Button>
            <Button type="submit" disabled={busy || !given.trim()}>
              {busy ? "Working…" : "Use this key"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

/** A key pasted, or read from its file. */
function KeyInput({ label = "Backup key", value, onValue }: { label?: string; value?: string; onValue?: (v: string) => void }) {
  const [own, setOwn] = useState("")
  const text = value ?? own
  const set = onValue ?? setOwn
  const file = useRef<HTMLInputElement>(null)
  async function read(event: ChangeEvent<HTMLInputElement>) {
    const chosen = event.target.files?.[0]
    if (chosen && chosen.size < 64 * 1024) set(await chosen.text())
    event.target.value = ""
  }
  return (
    <div className="grid gap-1.5">
      <Label htmlFor="backup-key">{label}</Label>
      <textarea
        id="backup-key"
        name="secret"
        value={text}
        onChange={(event) => set(event.target.value)}
        rows={3}
        autoComplete="off"
        spellCheck={false}
        placeholder="AGE-SECRET-KEY-PQ-1…"
        className="w-full rounded-md border border-input bg-transparent px-3 py-2 font-mono text-xs outline-none focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50"
        required
      />
      <input ref={file} type="file" accept=".txt,text/plain" className="hidden" onChange={read} />
      <Button type="button" size="sm" variant="ghost" className="justify-self-start" onClick={() => file.current?.click()}>
        Choose its file
      </Button>
    </div>
  )
}

function DestinationPanel({ state, onChange }: { state: Backups; onChange: (s: Backups) => void }) {
  const destination = state.destination
  const [editing, setEditing] = useState(false)
  return (
    <Panel id="destination" title="Where backups go">
      {destination ? (
        <dl className="grid gap-1.5">
          <Row label="Kind">{kindLabel(destination.kind)}</Row>
          <Row label="Where">
            <span className="font-mono text-xs break-all">{state.where}</span>
          </Row>
        </dl>
      ) : (
        <Muted>Nowhere yet.</Muted>
      )}
      <Button variant="outline" onClick={() => setEditing(true)}>
        {destination ? "Change" : "Choose"}
      </Button>
      <Dialog open={editing} onOpenChange={setEditing}>
        <DialogContent className="max-h-[90vh] overflow-y-auto">
          {editing && <DestinationForm state={state} onSaved={(next) => {
            onChange(next)
            setEditing(false)
          }} onCancel={() => setEditing(false)} />}
        </DialogContent>
      </Dialog>
    </Panel>
  )
}

type Scanned = { host: string; host_key: string; fingerprint: string }

function DestinationForm({ state, onSaved, onCancel }: { state: Backups; onSaved: (s: Backups) => void; onCancel: () => void }) {
  const destination = state.destination
  const [kind, setKind] = useState<Kind>(destination?.kind ?? "disk")
  const [scanned, setScanned] = useState<Scanned | null>(null)
  const [unitKey, setUnitKey] = useState(destination?.unit_key ?? false)
  const [sshKey, setSSHKey] = useState(state.ssh_key)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const same = destination?.kind === kind ? destination : undefined

  async function save(form: FormData) {
    const body: Record<string, unknown> = { kind, folder: field(form, "folder"), current_password: field(form, "current_password") }
    for (const name of ["endpoint", "region", "bucket", "access_key", "host", "share", "domain", "user", "url", "secret", "disk", "disk_name"]) {
      if (form.has(name)) body[name] = field(form, name)
    }
    body.path_style = form.get("path_style") === "on"
    if (kind === "sftp") {
      const host = String(body.host)
      body.unit_key = unitKey
      if (scanned?.host === host && form.get("trust_host_key") === "on") {
        body.host_key = scanned.host_key
      } else if (same?.host === host && same.host_key) {
        body.host_key = same.host_key
      } else {
        setScanned({ host, ...(await api<Omit<Scanned, "host">>("/api/v1/backups/host-key", { method: "POST", body: { host } })) })
        throw new FormError("Check the server's key, then save.")
      }
    }
    onSaved(await api<Backups>("/api/v1/backups/destination", { method: "PUT", timeoutMs: 120_000, body }))
  }

  async function makeKey() {
    try {
      const made = await api<{ public: string }>("/api/v1/backups/ssh-key", { method: "POST" })
      setSSHKey(made.public)
    } catch (failure) {
      setError(plainError(failure))
    }
  }

  const secretHint = same?.has_secret ? "Leave empty to keep the one set." : undefined
  const off = { autoComplete: "off", spellCheck: false } as const
  return (
    <form
      className="grid gap-4"
      onSubmit={async (event) => {
        event.preventDefault()
        setBusy(true)
        setError(null)
        try {
          await save(new FormData(event.currentTarget))
        } catch (failure) {
          setError(failure instanceof FormError ? failure.message : plainError(failure))
        } finally {
          setBusy(false)
        }
      }}
    >
      <DialogHeader>
        <DialogTitle>Where backups go</DialogTitle>
        <DialogDescription>Tested with a small file before it&apos;s kept.</DialogDescription>
      </DialogHeader>
      <Choice name="kind" label="Kind" options={KINDS} defaultValue={kind} onChange={(value) => {
        setKind(value as Kind)
        setScanned(null)
      }} />
      {kind === "disk" && <DiskPicker chosen={same?.disk} />}
      {kind === "smb" && (
        <>
          <div className="grid gap-4 sm:grid-cols-2">
            <Field label="Server" name="host" defaultValue={same?.host} placeholder="192.168.1.20" {...off} required />
            <Field label="Share" name="share" defaultValue={same?.share} placeholder="backups" {...off} required />
          </div>
          <Field label="Folder" name="folder" defaultValue={same?.folder ?? "patai"} {...off} />
          <div className="grid gap-4 sm:grid-cols-2">
            <Field label="User" name="user" defaultValue={same?.user} {...off} required />
            <Field label="Password" name="secret" type="password" autoComplete="off" hint={secretHint} required={!same?.has_secret} />
          </div>
          <Field label="Domain" name="domain" defaultValue={same?.domain} hint="Usually empty." {...off} />
        </>
      )}
      {kind === "sftp" && (
        <>
          <div className="grid gap-4 sm:grid-cols-2">
            <Field label="Server" name="host" defaultValue={same?.host} placeholder="u123456.your-storagebox.de:23" {...off} required />
            <Field label="User" name="user" defaultValue={same?.user} {...off} required />
          </div>
          <Field label="Folder" name="folder" defaultValue={same?.folder ?? "patai"} {...off} />
          <Label className="items-center gap-3 font-normal">
            <Checkbox checked={unitKey} onCheckedChange={(checked) => setUnitKey(checked === true)} />
            Sign in with the unit&apos;s key
          </Label>
          {unitKey ? (
            sshKey ? (
              <div className="grid gap-1.5">
                <p className="text-sm font-medium">The unit&apos;s key, for the server&apos;s authorized_keys</p>
                <code className="rounded-md bg-muted p-2 font-mono text-xs break-all">{sshKey}</code>
                <CopyButton text={sshKey} />
              </div>
            ) : (
              <Button type="button" variant="outline" onClick={makeKey}>Make the unit&apos;s key</Button>
            )
          ) : (
            <Field label="Password" name="secret" type="password" autoComplete="off" hint={secretHint} required={!same?.has_secret} />
          )}
          {scanned && (
            <div className="grid gap-2 rounded-lg border p-3">
              <p className="text-sm font-medium">The server&apos;s key</p>
              <code className="font-mono text-xs break-all">{scanned.fingerprint}</code>
              <Label className="items-center gap-3 font-normal">
                <Checkbox name="trust_host_key" required />
                It&apos;s the server&apos;s own
              </Label>
            </div>
          )}
        </>
      )}
      {kind === "webdav" && (
        <>
          <Field label="Address" name="url" defaultValue={same?.url} placeholder="https://cloud.example.com/remote.php/dav/files/me/" {...off} required />
          <Field label="Folder" name="folder" defaultValue={same?.folder ?? "patai"} {...off} />
          <div className="grid gap-4 sm:grid-cols-2">
            <Field label="User" name="user" defaultValue={same?.user} {...off} />
            <Field label="Password" name="secret" type="password" autoComplete="off" hint={secretHint} />
          </div>
        </>
      )}
      {kind === "s3" && (
        <>
          <Field label="Address" name="endpoint" defaultValue={same?.endpoint} placeholder="https://s3.eu-central-003.backblazeb2.com" {...off} required />
          <div className="grid gap-4 sm:grid-cols-2">
            <Field label="Bucket" name="bucket" defaultValue={same?.bucket} {...off} required />
            <Field label="Region" name="region" defaultValue={same?.region} placeholder="us-east-1" {...off} />
          </div>
          <Field label="Folder" name="folder" defaultValue={same?.folder ?? "patai"} hint="Empty for the top." {...off} />
          <Field label="Access key" name="access_key" defaultValue={same?.access_key} {...off} required />
          <Field label="Secret key" name="secret" type="password" autoComplete="off" hint={secretHint} required={!same?.has_secret} />
          <Label className="items-center gap-3 font-normal">
            <Checkbox name="path_style" defaultChecked={same?.path_style ?? true} />
            Bucket in the path, not the host name
          </Label>
        </>
      )}
      <CurrentPassword />
      <ErrorNote>{error}</ErrorNote>
      <DialogFooter>
        <Button type="button" variant="outline" onClick={onCancel}>Cancel</Button>
        <Button type="submit" disabled={busy}>{busy ? "Testing…" : "Test and save"}</Button>
      </DialogFooter>
    </form>
  )
}

function diskName(d: Disk, f?: Filesystem): string {
  const name = [f?.label, d.model].filter(Boolean).join(", ") || d.device
  return `${name} (${bytes(f?.size ?? d.size)})`
}

/** The disks plugged into the unit that backups could go on, and a way to format one that can't take them yet. */
function DiskPicker({ chosen }: { chosen?: string }) {
  const [disks, setDisks] = useState<Disk[] | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [selected, setSelected] = useState(chosen ?? "")
  const [formatting, setFormatting] = useState<Disk | null>(null)

  async function load(select?: string) {
    try {
      const list = (await api<{ disks: Disk[] }>("/api/v1/backups/disks")).disks
      setDisks(list)
      setError(null)
      if (select) setSelected(select)
    } catch (failure) {
      setError(plainError(failure))
    }
  }
  useEffect(() => {
    void load()
  }, [])

  const usable = (disks ?? []).flatMap((d) => (d.filesystems ?? []).filter((f) => f.usable).map((f) => ({ d, f })))
  const current = usable.find((u) => u.f.id === selected)
  return (
    <div className="grid gap-3">
      <ErrorNote>{error}</ErrorNote>
      {!disks && !error && <Spinner />}
      {disks && disks.length === 0 && <Muted>No disk is plugged in.</Muted>}
      {usable.length > 0 && (
        <Choice
          name="disk"
          label="Disk"
          defaultValue={selected}
          options={usable.map(({ d, f }) => ({ value: f.id, label: diskName(d, f), hint: [f.type, d.transport].filter(Boolean).join(", ") }))}
          onChange={setSelected}
        />
      )}
      <input type="hidden" name="disk_name" value={current ? diskName(current.d, current.f) : ""} />
      {(disks ?? []).filter((d) => !(d.filesystems ?? []).some((f) => f.usable)).map((d) => (
        <div key={d.device} className="flex items-center justify-between gap-3 rounded-lg border p-3 text-sm">
          <span className="grid gap-0.5">
            <span className="font-medium">{diskName(d)}</span>
            <span className="text-muted-foreground">{(d.filesystems ?? [])[0]?.why ?? "No filesystem."}</span>
          </span>
          <Button type="button" variant="outline" size="sm" onClick={() => setFormatting(d)}>Format</Button>
        </div>
      ))}
      <Button type="button" variant="ghost" size="sm" className="justify-self-start" onClick={() => void load()}>
        <RefreshCw /> Look again
      </Button>
      <FormDialog
        open={formatting !== null}
        onOpenChange={(open) => !open && setFormatting(null)}
        title={`Erase ${formatting ? diskName(formatting) : ""}?`}
        description="Everything on it is lost. It's made a disk for backups."
        submit="Erase and format"
        destructive
        action={async (form) => {
          const made = await api<{ id: string }>("/api/v1/backups/disks/format", {
            method: "POST",
            timeoutMs: 1_800_000,
            body: { device: formatting?.device, current_password: field(form, "current_password") },
          })
          setFormatting(null)
          await load(made.id)
        }}
      >
        <CurrentPassword />
      </FormDialog>
    </div>
  )
}

function SchedulePanel({ state, ready, onChange }: { state: Backups; ready: boolean; onChange: (s: Backups) => void }) {
  const [editing, setEditing] = useState(false)
  const s = state.schedule
  const zone = s.on ? s.zone : browserZone()
  return (
    <Panel id="schedule" title="Schedule">
      <dl className="grid gap-1.5">
        <Row label="Backups">{s.on ? `Daily at ${s.at}` : "Off"}</Row>
        {s.on && <Row label="Time zone">{s.zone}</Row>}
        {state.next && <Row label="Next">{when(state.next)}</Row>}
        <Row label="Kept">{`${s.keep_days} days, ${s.keep_weeks} weeks, ${s.keep_months} months`}</Row>
      </dl>
      <Button variant="outline" disabled={!ready && !s.on} onClick={() => setEditing(true)}>
        Change
      </Button>
      {!ready && !s.on && <Muted>Make a key and choose where backups go first.</Muted>}
      <FormDialog
        open={editing}
        onOpenChange={setEditing}
        title="Schedule"
        submit="Save"
        action={async (form) => {
          const number = (name: string) => Number(field(form, name))
          onChange(
            await api<Backups>("/api/v1/backups/schedule", {
              method: "PUT",
              body: {
                on: form.get("on") === "on",
                at: field(form, "at"),
                zone: field(form, "zone"),
                keep_days: number("keep_days"),
                keep_weeks: number("keep_weeks"),
                keep_months: number("keep_months"),
              },
            })
          )
          setEditing(false)
        }}
      >
        <Label className="items-center gap-3 font-normal">
          <Checkbox name="on" defaultChecked={s.on || ready} />
          Back up every day
        </Label>
        <div className="grid gap-4 sm:grid-cols-2">
          <Field label="At" name="at" type="time" defaultValue={s.at} required />
          <Field label="Time zone" name="zone" defaultValue={zone} autoComplete="off" spellCheck={false} required />
        </div>
        <div className="grid grid-cols-3 gap-4">
          <Field label="Days kept" name="keep_days" type="number" min={1} max={90} defaultValue={s.keep_days} required />
          <Field label="Weeks" name="keep_weeks" type="number" min={0} max={52} defaultValue={s.keep_weeks} required />
          <Field label="Months" name="keep_months" type="number" min={0} max={120} defaultValue={s.keep_months} required />
        </div>
        <Muted>The newest of each day, week and month is kept; older ones go.</Muted>
      </FormDialog>
    </Panel>
  )
}

function LastPanel({ state, ready, onRan }: { state: Backups; ready: boolean; onRan: () => void }) {
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  const running = state.running
  const last = state.last
  return (
    <Panel
      title="Last backup"
      action={
        <Button
          size="sm"
          disabled={!ready || busy || Boolean(running)}
          onClick={async () => {
            setBusy(true)
            setError(null)
            try {
              await startJob("/api/v1/backups/run")
              onRan()
            } catch (failure) {
              setError(plainError(failure))
            } finally {
              setBusy(false)
            }
          }}
        >
          <Play aria-hidden />
          Run now
        </Button>
      }
    >
      <ErrorNote>{error}</ErrorNote>
      {running && (
        <div className="grid gap-1.5">
          <p className="text-sm">
            {PHASES[running.phase] ?? running.phase}
            {running.bytes > 0 && ` · ${bytes(running.bytes)}`}
          </p>
          {running.size ? <ProgressBar percent={(running.bytes / running.size) * 100} label="Backup" /> : <Spinner />}
        </div>
      )}
      {!running && !last && <Muted>None yet.</Muted>}
      {!running && last && (
        <dl className="grid gap-1.5">
          <Row label="When">{when(last.updated_at)}</Row>
          <Row label="Result">
            <span className={last.state === "failed" ? tones.bad.text : ""}>{last.state === "failed" ? "Failed" : "Done"}</span>
          </Row>
          {last.state === "failed" && (
            <p className="font-mono text-xs wrap-anywhere text-muted-foreground">{last.result}</p>
          )}
          {last.state === "failed" && state.last_good && <Row label="Last good">{when(state.last_good)}</Row>}
        </dl>
      )}
    </Panel>
  )
}

const columns = columnsOf<Listed>()

function BackupList({ version, state, onRestoring }: { version: number; state: Backups; onRestoring: (job: Job) => void }) {
  const [list, setList] = useState<Listed[] | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [loading, setLoading] = useState(false)
  const [restoring, setRestoring] = useState<Listed | null>(null)
  const [checking, setChecking] = useState<Listed | null>(null)
  const [checked, setChecked] = useState<{ name: string; job: Job } | null>(null)

  async function load() {
    setLoading(true)
    try {
      setList((await api<{ backups: Listed[] }>("/api/v1/backups/list", { timeoutMs: 90_000 })).backups)
      setError(null)
    } catch (failure) {
      setError(plainError(failure))
    } finally {
      setLoading(false)
    }
  }
  const lastGood = state.last_good
  useEffect(() => {
    void load()
  }, [version, lastGood])

  const table = useMemo(
    () =>
      columns.columns([
        columns.accessor("created", {
          header: "Made",
          enableGlobalFilter: false,
          meta: { className: "font-medium" },
          cell: (cell) => when(cell.getValue()),
        }),
        columns.accessor((b) => b.unit ?? "", {
          id: "unit",
          header: "From",
          meta: { facet: {} },
          cell: (cell) => (
            <span className="flex items-center gap-2">
              {cell.getValue() || "?"}
              {!cell.row.original.this_unit && cell.getValue() && <Badge variant="outline">Other</Badge>}
            </span>
          ),
        }),
        columns.accessor("size", { header: "Size", meta: { numeric: true }, cell: (cell) => bytes(cell.getValue()) }),
        columns.accessor((b) => b.key_id ?? "", {
          id: "key",
          header: "Key",
          meta: { facet: {}, className: "font-mono text-xs" },
          cell: (cell) => (
            <span className={cell.row.original.this_key ? "" : "text-muted-foreground"}>{cell.getValue() || "?"}</span>
          ),
        }),
        columns.accessor((b) => b.release ?? "", { id: "release", header: "Release", meta: { className: "text-muted-foreground" } }),
        columns.display({
          id: "actions",
          header: ActionsHeader,
          cell: ({ row: { original: b } }) => (
            <RowActions label={`Actions for the backup of ${when(b.created)}`}>
              <DropdownMenuItem onClick={() => setChecking(b)}>
                <FileCheck aria-hidden />
                Check
              </DropdownMenuItem>
              <DropdownMenuItem onClick={() => setRestoring(b)}>
                <ArchiveRestore aria-hidden />
                Restore
              </DropdownMenuItem>
            </RowActions>
          ),
        }),
      ]),
    []
  )

  return (
    <Panel
      id="restore"
      title="Backups"
      action={
        <Button size="sm" variant="ghost" disabled={loading} onClick={() => void load()}>
          <RefreshCw aria-hidden />
          Refresh
        </Button>
      }
    >
      <ErrorNote>{error}</ErrorNote>
      {checked && (
        <Notice level={checked.job.state === "failed" ? "bad" : "good"} title={checked.job.state === "failed" ? "Check failed" : "Checked"}>
          {checked.job.result}
        </Notice>
      )}
      {list === null && loading && <Spinner />}
      {list && (
        <DataTable
          id="backups"
          label="Backups"
          data={list}
          columns={table}
          rowId={(b) => b.name}
          empty="None there yet."
          hidden={["release"]}
          sort={[{ id: "created", desc: true }]}
        />
      )}
      <FormDialog
        open={checking !== null}
        onOpenChange={(open) => !open && setChecking(null)}
        title="Check this backup"
        description="Read back and checked against its record. With the key, opened and checked file by file."
        submit="Check"
        action={async (form) => {
          const target = checking!
          const secret = field(form, "secret")
          let job = await startJob("/api/v1/backups/check", { name: target.name, secret })
          setChecking(null)
          setChecked(null)
          while (!jobFinished(job)) {
            await sleep(2_000)
            job = await api<Job>(`/api/v1/jobs/${job.id}`)
          }
          setChecked({ name: target.name, job })
        }}
      >
        <OptionalKey />
      </FormDialog>
      <FormDialog
        open={restoring !== null}
        onOpenChange={(open) => !open && setRestoring(null)}
        title="Restore this backup?"
        description={`Everything on this unit is replaced with the backup of ${when(restoring?.created)}, then it restarts. People sign in with the passwords they had then.`}
        submit="Restore"
        destructive
        action={async (form) => {
          const job = await startJob("/api/v1/backups/restore", {
            name: restoring!.name,
            secret: field(form, "secret"),
            keep_identity: form.get("keep_identity") === "on",
            current_password: field(form, "current_password"),
          })
          setRestoring(null)
          onRestoring(job)
        }}
      >
        <KeyInput />
        <Label className="items-start gap-3 font-normal leading-snug">
          <Checkbox name="keep_identity" className="mt-0.5" />
          <span className="grid gap-0.5">
            <span>The unit it came from is still running</span>
            <span className="text-muted-foreground">Keeps this unit&apos;s name, network settings, certificate and SSH host keys.</span>
          </span>
        </Label>
        <CurrentPassword />
      </FormDialog>
    </Panel>
  )
}

function OptionalKey() {
  const [using, setUsing] = useState(false)
  if (!using) {
    return (
      <Button type="button" size="sm" variant="ghost" className="justify-self-start" onClick={() => setUsing(true)}>
        Use the key too
      </Button>
    )
  }
  return <KeyInput />
}

/** Follows a restore: the backup arriving, then the unit going away and coming back, then its login. */
function Restoring({ job: first }: { job: Job }) {
  const { identity } = useIdentity()
  const [job, setJob] = useState(first)
  const [running, setRunning] = useState<Backups["running"]>(null)
  const [stage, setStage] = useState<"arriving" | "restarting" | "slow">("arriving")
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    let alive = true
    void (async () => {
      let current = first
      while (alive && !jobFinished(current)) {
        await sleep(2_000)
        try {
          current = await api<Job>(`/api/v1/jobs/${current.id}`, { anonymous: true, timeoutMs: 5_000 })
          setJob(current)
          setRunning((await api<Backups>("/api/v1/backups", { anonymous: true, timeoutMs: 5_000 })).running)
        } catch (failure) {
          if (failure instanceof ApiError && failure.status === 0) break
        }
      }
      if (!alive) return
      if (current.state === "failed") {
        setError(current.result || "The restore didn't finish.")
        return
      }
      setStage("restarting")
      const since = Date.now()
      let away = false
      while (alive) {
        await sleep(2_000)
        const back = await api<Identity>("/api/identity", { anonymous: true, timeoutMs: 5_000 }).catch(() => null)
        if (!back) away = true
        else if (away && back.state === "claimed") {
          window.location.assign("/login/")
          return
        }
        if (Date.now() - since > 5 * 60_000) setStage("slow")
      }
    })()
    return () => {
      alive = false
    }
  }, [first])

  return (
    <div role="status" className="fixed inset-0 z-50 grid place-items-center overflow-y-auto bg-background p-6">
      <div className="grid w-full max-w-md gap-4">
        {error ? (
          <>
            <p className="text-lg font-semibold">Not restored</p>
            <ErrorNote>{error}</ErrorNote>
            <Muted>Nothing on the unit was changed.</Muted>
            <Button onClick={() => window.location.reload()}>Back to backups</Button>
          </>
        ) : (
          <>
            <div className="flex items-center gap-3">
              <Spinner />
              <p className="text-lg font-semibold">{stage === "arriving" ? "Restoring" : "Restarting"}</p>
            </div>
            {stage === "arriving" && running && (
              <div className="grid gap-1.5">
                <Muted>
                  {PHASES[running.phase] ?? running.phase}
                  {running.bytes > 0 && ` · ${bytes(running.bytes)}`}
                </Muted>
                {running.size ? <ProgressBar percent={(running.bytes / running.size) * 100} label="Restore" /> : null}
              </div>
            )}
            {stage === "arriving" && !running && <Muted>{job.state === "queued" ? "Waiting to start." : "Working."}</Muted>}
            {stage !== "arriving" && (
              <Muted>
                A few minutes. Then log in with the backup&apos;s people and passwords. If it took the name or address of the
                unit it came from, this page can&apos;t follow: open https://&lt;that name&gt;.local:9443.
              </Muted>
            )}
            {stage === "slow" && (
              <Notice level="warn" title="Taking longer than it should">
                It may be at another address now. Its screen shows where.
              </Notice>
            )}
            <p className="font-mono text-xs text-muted-foreground">{identity.name}</p>
          </>
        )}
      </div>
    </div>
  )
}

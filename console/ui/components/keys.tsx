"use client"

import { useMemo, useState } from "react"
import { Pencil, Plus, Trash2 } from "lucide-react"

import { CopyButton, ErrorNote, Field } from "@/components/common"
import { ActionsHeader, columnsOf, DataTable, RowActions } from "@/components/data-table"
import { CurrentPassword, FormDialog } from "@/components/form-dialog"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { DropdownMenuItem } from "@/components/ui/dropdown-menu"
import { Label } from "@/components/ui/label"
import { api, FormError } from "@/lib/api"
import { when } from "@/lib/format"
import { field, useLoad } from "@/lib/hooks"
import type { APIKey, App, KeyLimits } from "@/lib/types"

const MIB = 1 << 20

const keyColumns = columnsOf<APIKey>()

function expired(key: APIKey): boolean {
  return key.expires_at !== null && new Date(key.expires_at).getTime() < Date.now()
}

/** A key's limits other than its APIs, in a line. */
function limitWords(limits: KeyLimits): string {
  const words = [
    limits.max_concurrent > 0 && `${limits.max_concurrent} at once`,
    limits.max_body > 0 && `requests to ${Math.round((limits.max_body / MIB) * 10) / 10} MiB`,
    limits.tokens_per_day > 0 && `${limits.tokens_per_day.toLocaleString()} tokens a day`,
    limits.images_per_day > 0 && `${limits.images_per_day.toLocaleString()} images a day`,
  ]
  return words.filter(Boolean).join(" · ") || "None"
}

/** A whole number from a form field, with empty as 0, which is no limit. */
function count(form: FormData, name: string, label: string): number {
  const text = field(form, name).trim()
  const value = text === "" ? 0 : Number(text)
  if (!Number.isFinite(value) || value < 0) throw new FormError(`${label} has to be a number, or empty for no limit.`)
  return value
}

function readLimits(app: App, form: FormData): KeyLimits {
  const apis = app.apis.map((item) => item.name).filter((name) => form.get(`api-${name}`) === "on")
  return {
    // All of them is the same as none: every API, including any to come.
    apis: apis.length === app.apis.length ? [] : apis,
    max_concurrent: Math.round(count(form, "max_concurrent", "Requests at once")),
    max_body: Math.round(count(form, "max_body", "Largest request") * MIB),
    tokens_per_day: Math.round(count(form, "tokens_per_day", "Tokens a day")),
    images_per_day: Math.round(count(form, "images_per_day", "Images a day")),
  }
}

function LimitFields({ app, limits }: { app: App; limits?: KeyLimits }) {
  const allowed = (name: string) => !limits || limits.apis.length === 0 || limits.apis.includes(name)
  const shown = (value: number | undefined, scale = 1) => (value ? String(Math.round((value / scale) * 10) / 10) : "")
  return (
    <>
      <fieldset className="grid gap-1.5">
        <legend className="mb-1.5 text-sm font-medium">APIs it can use</legend>
        {app.apis.map((item) => (
          <Label key={item.name} className="font-normal">
            <Checkbox name={`api-${item.name}`} defaultChecked={allowed(item.name)} />
            {item.title}
          </Label>
        ))}
      </fieldset>
      <div className="grid grid-cols-2 gap-3">
        <Field label="Requests at once" name="max_concurrent" type="number" min={0} max={64} step={1}
          defaultValue={shown(limits?.max_concurrent)} placeholder="No limit" />
        <Field label="Largest request (MiB)" name="max_body" type="number" min={0} max={64} step={0.1}
          defaultValue={shown(limits?.max_body, MIB)} placeholder="No limit" />
        <Field label="Tokens a day" name="tokens_per_day" type="number" min={0} step={1}
          defaultValue={shown(limits?.tokens_per_day)} placeholder="No limit" />
        <Field label="Images a day" name="images_per_day" type="number" min={0} step={1}
          defaultValue={shown(limits?.images_per_day)} placeholder="No limit" />
      </div>
      <p className="text-xs text-muted-foreground">Empty is no limit. A day starts at midnight UTC.</p>
    </>
  )
}

/** The API platform's keys: made with limits, shown once, revoked at once. */
export function Keys({ app }: { app: App }) {
  const keys = useLoad<APIKey[]>("/api/v1/keys")
  const [creating, setCreating] = useState(false)
  const [editing, setEditing] = useState<APIKey | null>(null)
  const [removing, setRemoving] = useState<APIKey | null>(null)
  const [created, setCreated] = useState<string | null>(null)
  const columns = useMemo(() => {
    const apiTitle = (name: string) => app.apis.find((item) => item.name === name)?.title ?? name
    return keyColumns.columns([
      keyColumns.accessor("name", {
        header: "Name",
        meta: { className: "font-medium" },
        cell: (cell) => (
          <span className="flex items-center gap-2">
            {cell.getValue()}
            {expired(cell.row.original) && <Badge variant="destructive">Expired</Badge>}
          </span>
        ),
      }),
      keyColumns.accessor((key) => (key.limits.apis.length > 0 ? key.limits.apis.map(apiTitle).join(", ") : "Every API"), {
        id: "apis",
        header: "APIs",
        meta: { className: "whitespace-normal" },
      }),
      keyColumns.accessor((key) => limitWords(key.limits), {
        id: "limits",
        header: "Limits",
        meta: { className: "text-xs text-muted-foreground whitespace-normal" },
      }),
      keyColumns.accessor((key) => key.today?.requests ?? 0, {
        id: "requests",
        header: "Requests today",
        meta: { numeric: true },
        cell: (cell) => cell.getValue().toLocaleString(),
      }),
      keyColumns.accessor((key) => (key.today ? key.today.prompt_tokens + key.today.completion_tokens : 0), {
        id: "tokens",
        header: "Tokens today",
        meta: { numeric: true },
        cell: (cell) => cell.getValue().toLocaleString(),
      }),
      keyColumns.accessor((key) => key.expires_at ?? "", {
        id: "expires",
        header: "Expires",
        enableGlobalFilter: false,
        cell: (cell) => {
          const key = cell.row.original
          return key.expires_at ? `${expired(key) ? "Expired" : ""} ${when(key.expires_at)}`.trim() : "Never"
        },
      }),
      keyColumns.accessor("created_at", { header: "Made", enableGlobalFilter: false, cell: (cell) => when(cell.getValue()) }),
      keyColumns.display({
        id: "actions",
        header: ActionsHeader,
        cell: ({ row: { original: key } }) => (
          <RowActions label={`Actions for ${key.name}`}>
            <DropdownMenuItem onClick={() => setEditing(key)}>
              <Pencil aria-hidden />
              Change limits
            </DropdownMenuItem>
            <DropdownMenuItem variant="destructive" onClick={() => setRemoving(key)}>
              <Trash2 aria-hidden />
              Revoke
            </DropdownMenuItem>
          </RowActions>
        ),
      }),
    ])
  }, [app.apis])

  return (
    <>
      <div className="flex justify-end">
        <Button size="sm" variant="outline" onClick={() => setCreating(true)}>
          <Plus aria-hidden />
          Make a key
        </Button>
      </div>
      <ErrorNote>{keys.error}</ErrorNote>
      {keys.data && (
        <DataTable
          id="api-keys"
          label="API keys"
          data={keys.data}
          columns={columns}
          rowId={(key) => String(key.id)}
          empty="No keys yet."
          hidden={["created_at", "tokens"]}
          sort={[{ id: "name", desc: false }]}
        />
      )}

      <FormDialog
        open={creating}
        onOpenChange={setCreating}
        title="Make an API key"
        submit="Make it"
        action={async (form) => {
          const days = field(form, "expires_days").trim()
          const expires = days === "" ? 0 : Number(days)
          if (!Number.isInteger(expires) || expires < 0) throw new FormError("Days has to be a whole number.")
          const limits = readLimits(app, form)
          if (app.apis.every((item) => form.get(`api-${item.name}`) !== "on")) throw new FormError("Choose at least one API.")
          const result = await api<{ secret: string }>("/api/v1/keys", {
            method: "POST",
            body: { name: field(form, "name"), limits, expires_days: expires, current_password: field(form, "current_password") },
          })
          setCreating(false)
          setCreated(result.secret)
          await keys.reload()
        }}
      >
        <Field label="Name" name="name" hint="What uses it. Letters, numbers and . _ @ - only." autoComplete="off"
          autoCapitalize="none" spellCheck={false} required />
        <LimitFields app={app} />
        <Field label="Expires after (days)" name="expires_days" type="number" inputMode="numeric" min={1} max={3650}
          step={1} hint="Leave empty for no expiry." />
        <CurrentPassword />
      </FormDialog>

      {editing && (
        <FormDialog
          open
          onOpenChange={() => setEditing(null)}
          title={`${editing.name}'s limits`}
          submit="Save"
          action={async (form) => {
            if (app.apis.every((item) => form.get(`api-${item.name}`) !== "on")) throw new FormError("Choose at least one API.")
            await api(`/api/v1/keys/${editing.id}/limits`, { method: "PUT", body: readLimits(app, form) })
            setEditing(null)
            await keys.reload()
          }}
        >
          <LimitFields app={app} limits={editing.limits} />
        </FormDialog>
      )}

      {removing && (
        <FormDialog
          open
          onOpenChange={() => setRemoving(null)}
          title={`Revoke ${removing.name}?`}
          description="Anything using it stops working at once. Its usage stays in the log."
          submit="Revoke"
          destructive
          action={async () => {
            await api(`/api/v1/keys/${removing.id}`, { method: "DELETE" })
            setRemoving(null)
            await keys.reload()
          }}
        />
      )}

      {created && (
        <Dialog open disablePointerDismissal onOpenChange={() => setCreated(null)}>
          <DialogContent showCloseButton={false}>
            <DialogHeader>
              <DialogTitle>Copy the key now</DialogTitle>
              <DialogDescription>It isn&apos;t shown again: the unit keeps only its hash.</DialogDescription>
            </DialogHeader>
            <code className="rounded-lg border bg-muted/50 p-3 font-mono text-sm break-all select-all">{created}</code>
            <CopyButton text={created} label="Copy key" />
            <DialogFooter>
              <Button onClick={() => setCreated(null)}>Done</Button>
            </DialogFooter>
          </DialogContent>
        </Dialog>
      )}
    </>
  )
}

"use client"

import { useRef, useState } from "react"
import { KeyRound, Plus, Trash2 } from "lucide-react"

import { Choice } from "@/components/choice"
import { CopyButton, ErrorNote, Field, Muted, Notice, PageTitle, Panel } from "@/components/common"
import { CurrentPassword, FormDialog } from "@/components/form-dialog"
import { isOwner, useSession } from "@/components/session"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { api, FormError } from "@/lib/api"
import { when } from "@/lib/format"
import { field, useLoad, useSubmit } from "@/lib/hooks"
import { roleLabel, ROLES, SCOPES } from "@/lib/labels"
import type { SshKey, Token, User } from "@/lib/types"

export default function AccessPage() {
  const session = useSession()
  return (
    <>
      <PageTitle>{isOwner(session) ? "Access" : "My password"}</PageTitle>
      {isOwner(session) && (
        <>
          <People />
          <Tokens />
          <SshKeys />
        </>
      )}
      <ChangePassword />
    </>
  )
}

function People() {
  const session = useSession()
  const users = useLoad<User[]>("/api/v1/users")
  const [adding, setAdding] = useState(false)
  const [removing, setRemoving] = useState<User | null>(null)
  const [resetting, setResetting] = useState<User | null>(null)

  return (
    <Panel
      title="People"
      action={
        <Button size="sm" variant="outline" onClick={() => setAdding(true)}>
          <Plus aria-hidden />
          Add
        </Button>
      }
    >
      <ErrorNote>{users.error}</ErrorNote>
      <ul className="grid divide-y">
        {users.data?.map((user) => (
          <li key={user.id} className="flex flex-wrap items-center gap-x-3 gap-y-1 py-2 first:pt-0 last:pb-0">
            <div className="grid min-w-0 flex-1 basis-40 gap-0.5">
              <p className="font-medium">
                {user.name}
                {user.name === session.name && <span className="font-normal text-muted-foreground"> (you)</span>}
              </p>
              <p className="text-xs text-muted-foreground">{roleLabel(user.role)}</p>
            </div>
            <div className="flex gap-2">
              {user.name !== session.name && (
                <Button size="sm" variant="outline" onClick={() => setResetting(user)}>
                  Reset password
                </Button>
              )}
              <Button
                size="icon-sm"
                variant="ghost"
                aria-label={`Remove ${user.name}`}
                onClick={() => setRemoving(user)}
              >
                <Trash2 aria-hidden />
              </Button>
            </div>
          </li>
        ))}
      </ul>

      <FormDialog
        open={adding}
        onOpenChange={setAdding}
        title="Add a person"
        submit="Add"
        action={async (form) => {
          await api("/api/v1/users", {
            method: "POST",
            body: {
              name: field(form, "name"),
              role: field(form, "role"),
              password: field(form, "password"),
              current_password: field(form, "current_password"),
            },
          })
          setAdding(false)
          await users.reload()
        }}
      >
        <Field
          label="Their name"
          name="name"
          hint="Letters, numbers and . _ @ - only."
          autoComplete="off"
          autoCapitalize="none"
          spellCheck={false}
          required
        />
        <Choice name="role" label="What they can do" options={ROLES} defaultValue="viewer" />
        <Field
          label="Their password"
          name="password"
          type="password"
          autoComplete="new-password"
          hint="At least 10 characters. Tell them what it is; they can change it."
          required
        />
        <CurrentPassword />
      </FormDialog>

      {resetting && (
        <FormDialog
          open
          onOpenChange={() => setResetting(null)}
          title={`Reset ${resetting.name}'s password`}
          description="They'll be logged out everywhere, and will need the new password."
          submit="Reset"
          action={async (form) => {
            await api(`/api/v1/users/${resetting.id}/password`, {
              method: "POST",
              body: { password: field(form, "password"), current_password: field(form, "current_password") },
            })
            setResetting(null)
          }}
        >
          <Field
            label="New password"
            name="password"
            type="password"
            autoComplete="new-password"
            hint="At least 10 characters."
            required
          />
          <CurrentPassword />
        </FormDialog>
      )}

      {removing && (
        <FormDialog
          open
          onOpenChange={() => setRemoving(null)}
          title={`Remove ${removing.name}?`}
          description={
            removing.name === session.name
              ? "That's you. You'll be logged out and won't be able to log back in."
              : "They'll be logged out and won't be able to log back in."
          }
          submit="Remove"
          destructive
          action={async (form) => {
            await api(`/api/v1/users/${removing.id}`, {
              method: "DELETE",
              body: { current_password: field(form, "current_password") },
            })
            setRemoving(null)
            await users.reload()
          }}
        >
          <CurrentPassword />
        </FormDialog>
      )}
    </Panel>
  )
}

/** A token for managing the unit, not an API key. */
function manages(token: Token): boolean {
  return token.scope !== "key"
}

function expired(token: Token): boolean {
  return token.expires_at !== null && new Date(token.expires_at).getTime() < Date.now()
}

function Tokens() {
  const tokens = useLoad<Token[]>("/api/v1/tokens")
  const [creating, setCreating] = useState(false)
  const [removing, setRemoving] = useState<Token | null>(null)
  const [created, setCreated] = useState<{ secret: string } | null>(null)

  return (
    <Panel
      title="API tokens"
      action={
        <Button size="sm" variant="outline" onClick={() => setCreating(true)}>
          <Plus aria-hidden />
          Create
        </Button>
      }
    >
      <Muted>
        For programs that manage the unit. A token can&apos;t manage people or other tokens. Keys for the apps&apos; APIs
        are on the API platform&apos;s page.
      </Muted>
      <ErrorNote>{tokens.error}</ErrorNote>
      {tokens.data?.filter(manages).length === 0 && <Muted>No tokens yet.</Muted>}
      <ul className="grid divide-y">
        {tokens.data?.filter(manages).map((token) => (
          <li key={token.id} className="flex items-center gap-3 py-2 first:pt-0 last:pb-0">
            <div className="grid min-w-0 flex-1 gap-0.5">
              <p className="flex flex-wrap items-center gap-2 font-medium">
                {token.name}
                {expired(token) && <Badge variant="destructive">Expired</Badge>}
              </p>
              <p className="text-xs text-muted-foreground">
                {roleLabel(token.scope)} ·{" "}
                {token.expires_at ? `${expired(token) ? "expired" : "expires"} ${when(token.expires_at)}` : "no expiry"}
              </p>
            </div>
            <Button
              size="icon-sm"
              variant="ghost"
              aria-label={`Delete ${token.name}`}
              onClick={() => setRemoving(token)}
            >
              <Trash2 aria-hidden />
            </Button>
          </li>
        ))}
      </ul>

      <FormDialog
        open={creating}
        onOpenChange={setCreating}
        title="Create an API token"
        submit="Create"
        action={async (form) => {
          const days = field(form, "expires_days").trim()
          const expires = days === "" ? 0 : Number(days)
          if (!Number.isInteger(expires) || expires < 0) throw new FormError("Days has to be a whole number.")
          const result = await api<{ secret: string }>("/api/v1/tokens", {
            method: "POST",
            body: {
              name: field(form, "name"),
              scope: field(form, "scope"),
              expires_days: expires,
              current_password: field(form, "current_password"),
            },
          })
          setCreating(false)
          setCreated({ secret: result.secret })
          await tokens.reload()
        }}
      >
        <Field
          label="Name"
          name="name"
          hint="What it's for. Letters, numbers and . _ @ - only."
          autoComplete="off"
          autoCapitalize="none"
          spellCheck={false}
          required
        />
        <Choice name="scope" label="What it can do" options={SCOPES} defaultValue="viewer" />
        <Field
          label="Expires after (days)"
          name="expires_days"
          type="number"
          inputMode="numeric"
          min={1}
          max={36500}
          step={1}
          hint="Leave empty for no expiry."
        />
        <CurrentPassword />
      </FormDialog>

      {removing && (
        <FormDialog
          open
          onOpenChange={() => setRemoving(null)}
          title={`Delete ${removing.name}?`}
          description="Anything using this token stops working at once."
          submit="Delete"
          destructive
          action={async () => {
            await api(`/api/v1/tokens/${removing.id}`, { method: "DELETE" })
            setRemoving(null)
            await tokens.reload()
          }}
        />
      )}

      {created && (
        <Dialog open disablePointerDismissal onOpenChange={() => setCreated(null)}>
          <DialogContent showCloseButton={false}>
            <DialogHeader>
              <DialogTitle>Copy your token now</DialogTitle>
              <DialogDescription>
                You won&apos;t see this token again: the unit keeps no copy it can show you.
              </DialogDescription>
            </DialogHeader>
            <code className="rounded-lg border bg-muted/50 p-3 font-mono text-sm break-all select-all">
              {created.secret}
            </code>
            <CopyButton text={created.secret} label="Copy token" />
            <DialogFooter>
              <Button onClick={() => setCreated(null)}>Done</Button>
            </DialogFooter>
          </DialogContent>
        </Dialog>
      )}
    </Panel>
  )
}

function SshKeys() {
  const keys = useLoad<SshKey[]>("/api/v1/ssh-keys")
  const [adding, setAdding] = useState(false)
  const [removing, setRemoving] = useState<SshKey | null>(null)

  return (
    <Panel
      title="SSH keys"
      action={
        <Button size="sm" variant="outline" onClick={() => setAdding(true)}>
          <Plus aria-hidden />
          Add
        </Button>
      }
    >
      <Muted>
        Each key can log in as <code className="font-mono">admin</code>, which has full control of the unit. Without a
        key, a forgotten password and no recovery codes can mean reflashing it.
      </Muted>
      <ErrorNote>{keys.error}</ErrorNote>
      {keys.data?.length === 0 && <Muted>No keys yet.</Muted>}
      <ul className="grid divide-y">
        {keys.data?.map((key) => (
          <li key={key.id} className="flex items-center gap-3 py-2 first:pt-0 last:pb-0">
            <div className="grid min-w-0 flex-1 gap-0.5">
              <p className="truncate font-medium">{key.comment || key.type}</p>
              <p className="font-mono text-xs break-all text-muted-foreground">{key.fingerprint}</p>
            </div>
            <Button
              size="icon-sm"
              variant="ghost"
              aria-label={`Remove ${key.comment || key.fingerprint}`}
              onClick={() => setRemoving(key)}
            >
              <Trash2 aria-hidden />
            </Button>
          </li>
        ))}
      </ul>

      <FormDialog
        open={adding}
        onOpenChange={setAdding}
        title="Add an SSH key"
        submit="Add"
        action={async (form) => {
          await api("/api/v1/ssh-keys", {
            method: "POST",
            body: { key: field(form, "key"), current_password: field(form, "current_password") },
          })
          setAdding(false)
          await keys.reload()
        }}
      >
        <Field
          label="Public key"
          name="key"
          hint="One line, usually from a file ending in .pub. It starts with something like ssh-ed25519."
          autoComplete="off"
          autoCapitalize="none"
          spellCheck={false}
          className="font-mono"
          required
        />
        <CurrentPassword />
      </FormDialog>

      {removing && (
        <FormDialog
          open
          onOpenChange={() => setRemoving(null)}
          title={`Remove ${removing.comment || "this key"}?`}
          description="Anyone using this key won't be able to log in any more. A session already open stays open until it ends."
          submit="Remove"
          destructive
          action={async (form) => {
            await api(`/api/v1/ssh-keys/${removing.id}`, {
              method: "DELETE",
              body: { current_password: field(form, "current_password") },
            })
            setRemoving(null)
            await keys.reload()
          }}
        >
          <CurrentPassword />
        </FormDialog>
      )}
    </Panel>
  )
}

function ChangePassword() {
  const formRef = useRef<HTMLFormElement>(null)
  const [changed, setChanged] = useState(false)
  const { busy, error, onSubmit } = useSubmit(async (form) => {
    setChanged(false)
    const password = field(form, "new_password")
    if (password !== field(form, "confirm")) throw new FormError("The two new passwords don't match.")
    await api("/api/v1/me/password", {
      method: "POST",
      body: { current_password: field(form, "current_password"), new_password: password },
    })
    setChanged(true)
    formRef.current?.reset()
  })

  return (
    <Panel id="password" title="Change my password">
      <form ref={formRef} onSubmit={onSubmit} className="grid gap-4 md:max-w-sm">
        <Field
          label="Current password"
          name="current_password"
          type="password"
          autoComplete="current-password"
          required
        />
        <Field
          label="New password"
          name="new_password"
          type="password"
          autoComplete="new-password"
          hint="At least 10 characters."
          required
        />
        <Field label="New password again" name="confirm" type="password" autoComplete="new-password" required />
        <ErrorNote>{error}</ErrorNote>
        {changed && <Notice level="good">Password changed. You&apos;re logged out on your other devices.</Notice>}
        <Button type="submit" variant="outline" disabled={busy}>
          <KeyRound aria-hidden />
          {busy ? "Changing…" : "Change password"}
        </Button>
      </form>
    </Panel>
  )
}

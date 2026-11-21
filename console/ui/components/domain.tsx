"use client"

import { useCallback, useId, useState } from "react"

import { ErrorNote, Field, Muted, Notice, Row, Spinner } from "@/components/common"
import { CurrentPassword, FormDialog } from "@/components/form-dialog"
import { isOwner, useSession } from "@/components/session"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import { Label } from "@/components/ui/label"
import { api, plainError } from "@/lib/api"
import { when } from "@/lib/format"
import { field, usePoll } from "@/lib/hooks"
import type { DomainProvider, DomainStatus } from "@/lib/types"

const SUBSCRIBER_AGREEMENT = "https://letsencrypt.org/repository/"
const SELECT =
  "h-8 w-full min-w-0 rounded-lg border border-input bg-transparent px-2 text-sm outline-none focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50 dark:bg-input/30"

/** The owner's domain, for the Status page's banner as well as the card. */
export function useDomain(enabled: boolean) {
  const [status, setStatus] = useState<DomainStatus | null>(null)
  const [error, setError] = useState<string | null>(null)
  const load = useCallback(async () => {
    try {
      setStatus(await api<DomainStatus>("/api/v1/domain"))
      setError(null)
    } catch (failure) {
      setError(plainError(failure))
    }
  }, [])
  usePoll(enabled ? load : async () => {}, status?.state === "getting" ? 5_000 : 60_000)
  return { status, setStatus, error }
}

/**
 * Your own domain (docs/console.md, Remote access): a name the owner has, a certificate the unit gets for it from
 * Let's Encrypt by DNS-01 with the owner's DNS credentials, and renews itself. Under More, below Tailscale.
 */
export function DomainSection() {
  const session = useSession()
  const owner = isOwner(session)
  const { status, setStatus, error } = useDomain(owner)
  const [dialog, setDialog] = useState<"set" | "remove" | null>(null)
  if (!owner || (status && !status.available)) return null
  const set = status && status.state !== "off"

  return (
    <details open={!!set} className="grid gap-3 border-t pt-3 text-sm">
      <summary className="cursor-pointer text-muted-foreground">More</summary>
      <div className="mt-3 grid gap-3">
        <p className="font-medium">Your own domain</p>
        <ErrorNote>{error}</ErrorNote>
        {!status && !error && <Spinner />}
        {status && !set && (
          <>
            <Muted>A name you own. Needs your DNS provider&apos;s API token.</Muted>
            <div>
              <Button variant="outline" onClick={() => setDialog("set")}>
                Set up
              </Button>
            </div>
          </>
        )}
        {status && set && <DomainState status={status} />}
        {status && set && (
          <div className="flex flex-wrap gap-2">
            <Button variant="outline" onClick={() => setDialog("set")}>
              Change
            </Button>
            <Button variant="outline" onClick={() => setDialog("remove")}>
              Remove
            </Button>
          </div>
        )}
      </div>
      {status && dialog === "set" && (
        <SetDialog
          status={status}
          onClose={() => setDialog(null)}
          onDone={(next) => {
            setStatus(next)
            setDialog(null)
          }}
        />
      )}
      <FormDialog
        open={dialog === "remove"}
        onOpenChange={(open) => setDialog(open ? "remove" : null)}
        title={`Stop using ${status?.name ?? "the domain"}?`}
        description="The unit forgets the name and its DNS credentials, and revokes the certificate."
        submit="Remove"
        destructive
        action={async (form) => {
          setStatus(
            await api<DomainStatus>("/api/v1/domain", {
              method: "DELETE",
              body: { current_password: field(form, "current_password") },
            }),
          )
          setDialog(null)
        }}
      >
        <CurrentPassword />
      </FormDialog>
    </details>
  )
}

function DomainState({ status }: { status: DomainStatus }) {
  const name = status.name ?? ""
  return (
    <>
      <dl className="grid gap-1.5">
        <Row label="Name">
          {status.state === "in_use" ? (
            <a href={`https://${name}/`} className="font-mono break-all underline underline-offset-4">
              {name}
            </a>
          ) : (
            <span className="font-mono break-all">{name}</span>
          )}
        </Row>
        <Row label="Certificate">
          {status.state === "in_use" ? `Let's Encrypt's, until ${when(status.not_after)}` : "Not yet"}
        </Row>
        {status.renew_at && <Row label="Renews">{when(status.renew_at)}</Row>}
        <Row label="Points to">
          <span className="grid gap-0.5 font-mono text-xs">
            {status.resolves.length > 0 ? status.resolves.map((a) => <span key={a}>{a}</span>) : "Nothing"}
          </span>
        </Row>
      </dl>
      {status.state === "getting" && !status.problem && !status.waiting && <Muted>Getting a certificate…</Muted>}
      {status.waiting && <Muted>{status.waiting}</Muted>}
      {status.problem && (
        <Notice level={status.warn ? "bad" : "warn"} title={status.state === "in_use" ? "Renewal failing" : undefined}>
          {status.problem}
        </Notice>
      )}
      {!status.points_here && status.addresses.length > 0 && (
        <Notice level="warn" title={`Point ${name} at this unit`}>
          <span className="font-mono text-xs">{status.addresses.join(", ")}</span>
        </Notice>
      )}
    </>
  )
}

function SetDialog({
  status,
  onClose,
  onDone,
}: {
  status: DomainStatus
  onClose: () => void
  onDone: (next: DomainStatus) => void
}) {
  const id = useId()
  const [providerId, setProviderId] = useState(status.provider ?? status.providers[0]?.id ?? "")
  const [agree, setAgree] = useState(false)
  const provider: DomainProvider | undefined = status.providers.find((p) => p.id === providerId)
  const kept = status.provider === providerId ? status.credentials : []
  return (
    <FormDialog
      open
      onOpenChange={(open) => !open && onClose()}
      title="Your own domain"
      description="The unit gets a certificate for this name from Let's Encrypt, and renews it itself."
      submit="Save"
      action={async (form) => {
        const credentials: Record<string, string> = {}
        for (const f of provider?.fields ?? []) credentials[f.name] = field(form, `cred_${f.name}`)
        onDone(
          await api<DomainStatus>("/api/v1/domain", {
            method: "PUT",
            body: {
              current_password: field(form, "current_password"),
              name: field(form, "name"),
              provider: providerId,
              credentials,
              agree,
            },
            timeoutMs: 90_000,
          }),
        )
      }}
    >
      <Field label="Name" name="name" defaultValue={status.name ?? ""} placeholder="patai.example.com" autoComplete="off" />
      <div className="grid gap-1.5">
        <Label htmlFor={`${id}-provider`}>DNS provider</Label>
        <select id={`${id}-provider`} value={providerId} onChange={(e) => setProviderId(e.target.value)} className={SELECT}>
          {status.providers.map((p) => (
            <option key={p.id} value={p.id}>
              {p.title}
            </option>
          ))}
        </select>
      </div>
      {provider?.fields.map((f) => {
        const isKept = kept.some((c) => c.name === f.name && c.set)
        const hint = [f.hint, isKept ? "Kept. Empty keeps it." : f.optional ? "Optional." : ""].filter(Boolean).join(" ")
        return f.multiline ? (
          <div key={f.name} className="grid gap-1.5">
            <Label htmlFor={`${id}-${f.name}`}>{f.label}</Label>
            <textarea
              id={`${id}-${f.name}`}
              name={`cred_${f.name}`}
              rows={4}
              autoComplete="off"
              spellCheck={false}
              className="min-h-20 w-full rounded-lg border border-input bg-transparent px-2 py-1 font-mono text-xs outline-none focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50 dark:bg-input/30"
            />
            {hint && <p className="text-xs text-muted-foreground">{hint}</p>}
          </div>
        ) : (
          <Field
            key={f.name}
            label={f.label}
            name={`cred_${f.name}`}
            type={f.secret ? "password" : "text"}
            autoComplete="off"
            hint={hint || undefined}
          />
        )
      })}
      {provider?.narrow && <Muted>{provider.narrow} A stolen unit gives up what&apos;s kept here.</Muted>}
      <label className="flex items-start gap-2 text-sm">
        <Checkbox checked={agree} onCheckedChange={(checked) => setAgree(checked === true)} />
        <span>
          I accept Let&apos;s Encrypt&apos;s{" "}
          <a href={SUBSCRIBER_AGREEMENT} target="_blank" rel="noreferrer" className="underline underline-offset-4">
            Subscriber Agreement
          </a>
          .
        </span>
      </label>
      <CurrentPassword />
    </FormDialog>
  )
}

/** On the Status page, only when the domain's certificate needs the owner. */
export function DomainBanner() {
  const session = useSession()
  const { status } = useDomain(isOwner(session))
  if (!status?.warn) return null
  return (
    <Notice level="bad" title={`The certificate for ${status.name} isn't renewing`}>
      {status.problem ?? "Remote access, on the System page."}
    </Notice>
  )
}

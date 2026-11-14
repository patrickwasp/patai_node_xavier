"use client"

import { useCallback, useEffect, useState } from "react"

import { ErrorNote, Field, Muted, Notice, Panel, Row } from "@/components/common"
import { CurrentPassword, FormDialog } from "@/components/form-dialog"
import { isOwner, useSession } from "@/components/session"
import { Button } from "@/components/ui/button"
import { api, plainError } from "@/lib/api"
import { capitalise, duration, when } from "@/lib/format"
import { field } from "@/lib/hooks"
import type { CertificateStatus, OwnerCertificate } from "@/lib/types"

/** How long before it runs out the page starts saying so. */
const RENEW_DAYS = 30

/** What's left of it by this browser's clock, which is the one a browser checks it with. */
function lifeLeft(owner: OwnerCertificate): { level: "bad" | "warn" | null; text: string } {
  const left = (Date.parse(owner.not_after) - Date.now()) / 1000
  if (left <= 0) return { level: "bad", text: `It ran out ${duration(-left)} ago, so browsers refuse it at its names.` }
  if (left < RENEW_DAYS * 86_400) return { level: "warn", text: `It runs out in ${duration(left)}. The unit doesn't renew it.` }
  return { level: null, text: "" }
}

function Details({ owner }: { owner: OwnerCertificate }) {
  return (
    <dl className="grid gap-1.5">
      <Row label="Used at">
        <span className="grid gap-0.5">
          {owner.names.map((name) => (
            <a key={name} href={`https://${name}/`} className="font-mono break-all underline underline-offset-4">
              {name}
            </a>
          ))}
        </span>
      </Row>
      <Row label="Issued by">
        <span className="break-all">{owner.issuer}</span>
      </Row>
      <Row label="Valid until">{when(owner.not_after)}</Row>
      <div className="grid gap-1">
        <dt className="text-muted-foreground">Fingerprint</dt>
        <dd>
          <code className="font-mono text-xs break-all select-all">{owner.fingerprint}</code>
        </dd>
      </div>
    </dl>
  )
}

export function CertificatePanel() {
  const session = useSession()
  const [status, setStatus] = useState<CertificateStatus | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [adding, setAdding] = useState(false)
  const [removing, setRemoving] = useState(false)
  const load = useCallback(async () => {
    try {
      setStatus(await api<CertificateStatus>("/api/v1/certificate"))
      setError(null)
    } catch (failure) {
      setError(plainError(failure))
    }
  }, [])
  useEffect(() => {
    void load()
  }, [load])

  const owner = status?.owner ?? null
  const life = owner ? lifeLeft(owner) : null
  return (
    <Panel id="certificate" title="Certificate">
      <ErrorNote>{error}</ErrorNote>
      {status && !owner && (
        <Muted>Browsers warn because the unit made its own certificate. Add one for a name that points here.</Muted>
      )}
      {status?.problem && (
        <Notice level="warn" title="The certificate kept here isn't in use">
          {capitalise(status.problem)}. The unit&apos;s own certificate answers everywhere until one is added again.
        </Notice>
      )}
      {owner && (
        <>
          <Details owner={owner} />
          {life?.level && <Notice level={life.level === "bad" ? "bad" : "warn"}>{life.text}</Notice>}
          {(owner.unused ?? []).length > 0 && (
            <Muted>Also named in it, but answered with the unit&apos;s own certificate: {owner.unused.join(", ")}.</Muted>
          )}
          <Muted>The unit&apos;s addresses and its .local name keep its own certificate, whose fingerprint is on its screen.</Muted>
        </>
      )}
      {status && isOwner(session) && status.available && (
        <div className="flex flex-wrap gap-2">
          <Button variant="outline" onClick={() => setAdding(true)}>
            {owner ? "Replace it" : "Add a certificate"}
          </Button>
          {(owner || status.problem) && (
            <Button variant="outline" onClick={() => setRemoving(true)}>
              Remove it
            </Button>
          )}
        </div>
      )}
      {adding && (
        <AddCertificate
          onClose={() => setAdding(false)}
          onDone={(next) => {
            setAdding(false)
            setStatus((current) => current && { ...current, owner: next, problem: "" })
          }}
        />
      )}
      <FormDialog
        open={removing}
        onOpenChange={setRemoving}
        title="Remove the certificate?"
        description="Its names stop working here at once. The unit's addresses and its .local name carry on as they are."
        submit="Remove it"
        destructive
        action={async (form) => {
          await api("/api/v1/certificate/remove", {
            method: "POST",
            body: { current_password: field(form, "current_password") },
          })
          setRemoving(false)
          await load()
        }}
      >
        <CurrentPassword />
      </FormDialog>
    </Panel>
  )
}

async function readFile(input: HTMLInputElement): Promise<string> {
  const file = input.files?.[0]
  return file ? file.text() : ""
}

function AddCertificate({ onClose, onDone }: { onClose: () => void; onDone: (owner: OwnerCertificate) => void }) {
  const [chain, setChain] = useState("")
  const [key, setKey] = useState("")
  const [preview, setPreview] = useState<OwnerCertificate | null>(null)
  const [problem, setProblem] = useState<string | null>(null)

  // What it would do, as soon as both files are here. Nothing changes until the owner says so.
  useEffect(() => {
    setPreview(null)
    setProblem(null)
    if (!chain || !key) return
    let current = true
    api<OwnerCertificate>("/api/v1/certificate/check", { method: "POST", body: { chain, key } })
      .then((owner) => current && setPreview(owner))
      .catch((failure) => current && setProblem(plainError(failure)))
    return () => {
      current = false
    }
  }, [chain, key])

  return (
    <FormDialog
      open
      onOpenChange={(open) => !open && onClose()}
      title="Add a certificate"
      description="For a name that points at this unit. Give its certificate, followed by any that issued it, and its private key, both as PEM files."
      submit="Use this certificate"
      action={async (form) => {
        if (!preview) throw new Error(problem ?? "Choose both files first.")
        onDone(
          await api<OwnerCertificate>("/api/v1/certificate", {
            method: "PUT",
            body: { chain, key, current_password: field(form, "current_password") },
          })
        )
      }}
    >
      <Field
        label="Certificate and chain"
        name="chain"
        type="file"
        accept=".pem,.crt,.cer,.txt"
        onChange={(event) => void readFile(event.currentTarget).then(setChain)}
        required
      />
      <Field
        label="Private key"
        name="key"
        type="file"
        accept=".pem,.key,.txt"
        hint="Without a passphrase. Use one made for this unit alone: its storage isn't encrypted, so a stolen unit gives up the key."
        onChange={(event) => void readFile(event.currentTarget).then(setKey)}
        required
      />
      <ErrorNote>{problem}</ErrorNote>
      {preview && (
        <div className="grid gap-2 rounded-lg border p-3">
          <Details owner={preview} />
          {preview.dates_unsure && (
            <Notice level="warn">
              The unit&apos;s clock may be wrong, so it couldn&apos;t check the dates. Browsers check them with their own.
            </Notice>
          )}
          {lifeLeft(preview).level && <Notice level="warn">{lifeLeft(preview).text}</Notice>}
          {(preview.unused ?? []).length > 0 && (
            <Muted>Not used for {preview.unused.join(", ")}: those keep the unit&apos;s own certificate.</Muted>
          )}
          <Muted>
            At its names you&apos;ll log in again, because each address has its own login. The unit doesn&apos;t renew it,
            so replace it before it runs out.
          </Muted>
        </div>
      )}
      <CurrentPassword />
    </FormDialog>
  )
}

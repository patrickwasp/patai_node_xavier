"use client"

import { useEffect, useRef, useState } from "react"
import { useRouter } from "next/navigation"
import { Download } from "lucide-react"

import { CopyButton, ErrorNote, Field, Muted, Notice, SoloPage, Spinner } from "@/components/common"
import { DiskQuestion, diskTitle, needsAnswer } from "@/components/disk-question"
import { useIdentity } from "@/components/gate"
import { Button } from "@/components/ui/button"
import { Card, CardContent } from "@/components/ui/card"
import { Checkbox } from "@/components/ui/checkbox"
import { Label } from "@/components/ui/label"
import { api, ApiError, FormError } from "@/lib/api"
import { downloadText, takeClaimCode } from "@/lib/browser"
import { field, useSubmit } from "@/lib/hooks"
import type { DiskStatus } from "@/lib/types"

/** Eight pairs to a line, so two copies can be compared line by line. */
function fingerprintLines(fingerprint: string): string[] {
  const pairs = fingerprint.split(":")
  const lines: string[] = []
  for (let i = 0; i < pairs.length; i += 8) lines.push(pairs.slice(i, i + 8).join(":"))
  return lines
}

export default function ClaimPage() {
  const router = useRouter()
  const { identity, refresh } = useIdentity()
  const [codes, setCodes] = useState<string[] | null>(null)
  const [disk, setDisk] = useState<DiskStatus | null>(null)
  const [settled, setSettled] = useState(false)
  const [prefilled] = useState(takeClaimCode)
  // Held for one more step, so the disk question doesn't ask for what was typed a moment ago.
  const chosenPassword = useRef("")

  // Only the person who has just claimed it stays here, to save their codes.
  const claimed = identity.state === "claimed" && codes === null
  useEffect(() => {
    if (claimed) router.replace("/")
  }, [claimed, router])

  const { busy, error, onSubmit } = useSubmit(async (form) => {
    const password = field(form, "password")
    if (password !== field(form, "confirm")) throw new FormError("The two passwords don't match.")
    try {
      const result = await api<{ recovery_codes: string[] }>("/api/v1/claim", {
        method: "POST",
        anonymous: true,
        body: { code: field(form, "code"), name: field(form, "name"), password },
      })
      chosenPassword.current = password
      setCodes(result.recovery_codes)
    } catch (failure) {
      if (failure instanceof ApiError && failure.code === "not-claimable") await refresh()
      throw failure
    }
  })

  async function finish() {
    chosenPassword.current = ""
    await refresh()
    setDisk(null)
    setSettled(true)
  }

  if (settled) {
    return (
      <SoloPage name={identity.name} title="Start from a backup?">
        <Muted>A backup of a unit brings back its people, settings, apps and their data.</Muted>
        <Button size="lg" onClick={() => router.replace("/backups/#restore")}>
          Restore a backup
        </Button>
        <Button size="lg" variant="outline" onClick={() => router.replace("/")}>
          Start fresh
        </Button>
      </SoloPage>
    )
  }

  if (disk !== null) {
    return (
      <SoloPage name={identity.name} title={diskTitle(disk)}>
        <Card>
          <CardContent>
            <DiskQuestion disk={disk} password={chosenPassword.current} onDone={finish} />
          </CardContent>
        </Card>
        <Button variant="ghost" onClick={finish}>
          Decide later
        </Button>
      </SoloPage>
    )
  }

  if (codes !== null) {
    return (
      <RecoveryCodes
        codes={codes}
        name={identity.name}
        onDone={async () => {
          const found = await api<DiskStatus>("/api/v1/disk").catch(() => null)
          if (found !== null && needsAnswer(found)) setDisk(found)
          else await finish()
        }}
      />
    )
  }
  if (claimed) {
    return (
      <div className="grid min-h-svh place-items-center">
        <Spinner />
      </div>
    )
  }

  return (
    <SoloPage name={identity.name} title="Claim this unit">
      <Muted>You&apos;ll become its owner, and only you will be able to manage it.</Muted>

      {identity.fingerprint && (
        <Notice level="warn" title="First, check this fingerprint">
          <p>
            It must match the one on the unit&apos;s screen or card. If it doesn&apos;t, stop here: this may not be your
            unit.
          </p>
          <code className="mt-2 block font-mono text-sm select-all">
            {fingerprintLines(identity.fingerprint).map((line) => (
              <span key={line} className="block">
                {line}
              </span>
            ))}
          </code>
        </Notice>
      )}

      <Card>
        <CardContent>
          <form onSubmit={onSubmit} className="grid gap-4">
            <Field
              label="Claim code"
              name="code"
              defaultValue={prefilled}
              hint={prefilled ? "Filled in from the QR code." : "It's on the unit's screen or card."}
              autoComplete="off"
              autoCapitalize="characters"
              spellCheck={false}
              className="font-mono"
              required
            />
            <Field
              label="Your name"
              name="name"
              hint="Letters, numbers and . _ @ - only. You'll log in with this."
              autoComplete="username"
              autoCapitalize="none"
              spellCheck={false}
              required
            />
            <Field
              label="Password"
              name="password"
              type="password"
              autoComplete="new-password"
              hint="At least 10 characters."
              required
            />
            <Field label="Password again" name="confirm" type="password" autoComplete="new-password" required />
            <ErrorNote>{error}</ErrorNote>
            <Button type="submit" size="lg" disabled={busy}>
              {busy ? "Claiming…" : "Claim this unit"}
            </Button>
          </form>
        </CardContent>
      </Card>
    </SoloPage>
  )
}

function RecoveryCodes({ codes, name, onDone }: { codes: string[]; name: string; onDone: () => Promise<void> }) {
  const [saved, setSaved] = useState(false)
  const [leaving, setLeaving] = useState(false)

  // A reload here would lose the codes for good.
  useEffect(() => {
    if (saved) return
    const warn = (event: BeforeUnloadEvent) => event.preventDefault()
    window.addEventListener("beforeunload", warn)
    return () => window.removeEventListener("beforeunload", warn)
  }, [saved])

  const plain = codes.join("\n")
  const file = [
    `Recovery codes for ${name}`,
    "",
    "Each code works once, to set a new password if you forget yours.",
    "Keep them somewhere safe. The unit can't show them again.",
    "",
    plain,
    "",
  ].join("\n")

  return (
    <SoloPage name={name} title="Save your recovery codes">
      <Notice level="warn" title="You will never see these again">
        If you forget your password, one of these codes is how you get back in. Each one works once. Without them, a
        forgotten password can mean reflashing the unit.
      </Notice>

      <Card>
        <CardContent className="grid gap-4">
          <ol className="grid gap-x-6 gap-y-1 font-mono text-sm select-all sm:grid-cols-2">
            {codes.map((code) => (
              <li key={code}>{code}</li>
            ))}
          </ol>
          <div className="grid gap-2 sm:grid-cols-2">
            <CopyButton text={plain} label="Copy codes" />
            <Button
              type="button"
              variant="outline"
              onClick={() => downloadText(`${name}-recovery-codes.txt`, file)}
            >
              <Download aria-hidden />
              Download as a file
            </Button>
          </div>
        </CardContent>
      </Card>

      <Label className="items-center gap-3 rounded-lg border bg-card p-3 leading-snug">
        <Checkbox checked={saved} onCheckedChange={setSaved} />
        I&apos;ve saved these
      </Label>

      <Button
        size="lg"
        disabled={!saved || leaving}
        onClick={async () => {
          setLeaving(true)
          await onDone()
        }}
      >
        Continue
      </Button>
    </SoloPage>
  )
}

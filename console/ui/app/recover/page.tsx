"use client"

import { useState } from "react"
import Link from "next/link"

import { ErrorNote, Field, Muted, Notice, SoloPage } from "@/components/common"
import { useIdentity } from "@/components/gate"
import { Button, buttonVariants } from "@/components/ui/button"
import { Card, CardContent } from "@/components/ui/card"
import { api, FormError } from "@/lib/api"
import { field, useSubmit } from "@/lib/hooks"

export default function RecoverPage() {
  const { identity } = useIdentity()
  const [done, setDone] = useState(false)

  const { busy, error, onSubmit } = useSubmit(async (form) => {
    const password = field(form, "password")
    if (password !== field(form, "confirm")) throw new FormError("The two new passwords don't match.")
    await api("/api/v1/recover", {
      method: "POST",
      anonymous: true,
      body: { name: field(form, "name"), code: field(form, "code"), password },
    })
    setDone(true)
  })

  if (done) {
    return (
      <SoloPage name={identity.name} title="Password changed">
        <Notice level="good">Your new password is set. That recovery code won&apos;t work again.</Notice>
        <Link href="/login/" className={buttonVariants({ size: "lg" })}>
          Log in
        </Link>
      </SoloPage>
    )
  }

  return (
    <SoloPage name={identity.name} title="Set a new password">
      <Muted>
        Use one of the recovery codes you saved when you claimed this unit. Each code works once, and using one
        logs you out everywhere.
      </Muted>
      <Card>
        <CardContent>
          <form onSubmit={onSubmit} className="grid gap-4">
            <Field label="Name" name="name" autoComplete="username" autoCapitalize="none" spellCheck={false} required />
            <Field
              label="Recovery code"
              name="code"
              autoComplete="off"
              autoCapitalize="characters"
              spellCheck={false}
              className="font-mono"
              required
            />
            <Field
              label="New password"
              name="password"
              type="password"
              autoComplete="new-password"
              hint="At least 10 characters."
              required
            />
            <Field
              label="New password again"
              name="confirm"
              type="password"
              autoComplete="new-password"
              required
            />
            <ErrorNote>{error}</ErrorNote>
            <Button type="submit" size="lg" disabled={busy}>
              {busy ? "Setting it…" : "Set new password"}
            </Button>
          </form>
        </CardContent>
      </Card>
      <Muted>
        No codes left? With access to the unit itself, run{" "}
        <code className="font-mono">sudo patai-console reset-owner &lt;name&gt;</code>. Failing that, the unit has to
        be reflashed.
      </Muted>
      <Link href="/login/" className="text-center text-sm text-muted-foreground underline underline-offset-3">
        Back to log in
      </Link>
    </SoloPage>
  )
}

"use client"

import { useEffect } from "react"
import Link from "next/link"
import { useRouter } from "next/navigation"

import { ErrorNote, Field, SoloPage } from "@/components/common"
import { useIdentity } from "@/components/gate"
import { Button } from "@/components/ui/button"
import { Card, CardContent } from "@/components/ui/card"
import { api } from "@/lib/api"
import { field, useSubmit } from "@/lib/hooks"

export default function LoginPage() {
  const router = useRouter()
  const { identity } = useIdentity()

  // Someone already logged in has no business here.
  useEffect(() => {
    api("/api/v1/session", { anonymous: true })
      .then(() => router.replace("/"))
      .catch(() => {})
  }, [router])

  const { busy, error, onSubmit } = useSubmit(async (form) => {
    await api("/api/v1/session", {
      method: "POST",
      anonymous: true,
      body: { name: field(form, "name"), password: field(form, "password") },
    })
    router.replace("/")
  })

  return (
    <SoloPage name={identity.name} title="Log in">
      <Card>
        <CardContent>
          <form onSubmit={onSubmit} className="grid gap-4">
            <Field label="Name" name="name" autoComplete="username" autoCapitalize="none" spellCheck={false} required />
            <Field label="Password" name="password" type="password" autoComplete="current-password" required />
            <ErrorNote>{error}</ErrorNote>
            <Button type="submit" size="lg" disabled={busy}>
              {busy ? "Logging in…" : "Log in"}
            </Button>
          </form>
        </CardContent>
      </Card>
      <Link href="/recover/" className="text-center text-sm text-muted-foreground underline underline-offset-3">
        Forgotten your password?
      </Link>
    </SoloPage>
  )
}

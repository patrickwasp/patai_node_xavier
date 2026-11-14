"use client"

import { createContext, type ReactNode, useCallback, useContext, useEffect, useState } from "react"
import { useRouter } from "next/navigation"

import { Notice, SoloPage, Spinner } from "@/components/common"
import { api, RECHECK_EVENT } from "@/lib/api"
import { takeClaimCode } from "@/lib/browser"
import { usePath } from "@/lib/hooks"
import type { Identity } from "@/lib/types"

type IdentityContextValue = {
  identity: Identity
  refresh: () => Promise<Identity | null>
}

const IdentityContext = createContext<IdentityContextValue | null>(null)

export function useIdentity(): IdentityContextValue {
  const value = useContext(IdentityContext)
  if (value === null) throw new Error("useIdentity outside the gate")
  return value
}

/** Decides what a visitor may see from what the unit says it is. Nothing renders before it knows. */
export function Gate({ children }: { children: ReactNode }) {
  const path = usePath()
  const router = useRouter()
  const [identity, setIdentity] = useState<Identity | null>(null)
  const [lost, setLost] = useState(false)

  const refresh = useCallback(async () => {
    try {
      const next = await api<Identity>("/api/identity", { anonymous: true, timeoutMs: 8_000 })
      setIdentity(next)
      setLost(false)
      return next
    } catch {
      setLost(true)
      return null
    }
  }, [])

  // First of all, before a redirect can carry the code into another address.
  useEffect(() => {
    takeClaimCode()
  }, [])

  useEffect(() => {
    void refresh()
  }, [path, refresh])

  useEffect(() => {
    window.addEventListener(RECHECK_EVENT, refresh)
    return () => window.removeEventListener(RECHECK_EVENT, refresh)
  }, [refresh])

  useEffect(() => {
    if (!lost || identity) return
    const timer = setInterval(refresh, 5_000)
    return () => clearInterval(timer)
  }, [lost, identity, refresh])

  const name = identity?.name
  useEffect(() => {
    if (name) document.title = `${name} console`
  }, [name])

  const mustClaim = identity?.state === "unclaimed" && path !== "/claim"
  useEffect(() => {
    if (mustClaim) router.replace("/claim/")
  }, [mustClaim, router])

  if (identity === null) return <Waiting lost={lost} />
  if (mustClaim) return <Waiting lost={false} />

  if (identity.state === "recovery") {
    return (
      <SoloPage name={identity.name} title="This unit needs service">
        {identity.fault && <p className="font-mono text-sm">{identity.fault}</p>}
      </SoloPage>
    )
  }

  if (identity.state === "unclaimable") {
    return (
      <SoloPage name={identity.name} title="This unit has no claim code">
        <Notice level="warn">
          Run <code className="font-mono">sudo patai-console new-claim</code> on the unit, or reflash it.
        </Notice>
      </SoloPage>
    )
  }

  return <IdentityContext.Provider value={{ identity, refresh }}>{children}</IdentityContext.Provider>
}

function Waiting({ lost }: { lost: boolean }) {
  return (
    <main className="grid min-h-svh place-items-center px-4">
      <div role="status" className="flex max-w-sm items-center gap-3 text-sm text-muted-foreground">
        <Spinner />
        {lost ? "The unit isn't answering. Still trying…" : "Connecting to the unit…"}
      </div>
    </main>
  )
}

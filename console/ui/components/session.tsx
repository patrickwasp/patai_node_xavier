"use client"

import { createContext, type ReactNode, useCallback, useContext, useEffect, useState } from "react"

import { ErrorNote, Spinner } from "@/components/common"
import { Button } from "@/components/ui/button"
import { api, plainError } from "@/lib/api"
import type { Session } from "@/lib/types"

const SessionContext = createContext<Session | null>(null)

export function useSession(): Session {
  const session = useContext(SessionContext)
  if (session === null) throw new Error("useSession outside the provider")
  return session
}

export const canOperate = (session: Session) => session.role === "operator" || session.role === "owner"
export const isOwner = (session: Session) => session.role === "owner"

/** Everything past the login. A 401 from here sends the visitor to the login page. */
export function SessionProvider({ children }: { children: ReactNode }) {
  const [session, setSession] = useState<Session | null>(null)
  const [error, setError] = useState<string | null>(null)

  const load = useCallback(async () => {
    setError(null)
    try {
      setSession(await api<Session>("/api/v1/session"))
    } catch (failure) {
      setError(plainError(failure))
    }
  }, [])

  useEffect(() => {
    void load()
  }, [load])

  if (session === null) {
    return (
      <main className="mx-auto grid min-h-svh max-w-sm content-center gap-3 px-4">
        {error ? (
          <>
            <ErrorNote>{error}</ErrorNote>
            <Button variant="outline" onClick={load}>
              Try again
            </Button>
          </>
        ) : (
          <div role="status" className="flex justify-center">
            <Spinner />
            <span className="sr-only">Loading</span>
          </div>
        )}
      </main>
    )
  }

  return <SessionContext.Provider value={session}>{children}</SessionContext.Provider>
}

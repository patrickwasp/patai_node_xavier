"use client"

import { type ReactNode, useEffect, useRef, useState } from "react"

import { Muted, Notice, Spinner } from "@/components/common"
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
  AlertDialogTrigger,
} from "@/components/ui/alert-dialog"
import { Button } from "@/components/ui/button"
import { api, ApiError, plainError, startJob } from "@/lib/api"
import { sleep } from "@/lib/hooks"
import type { Identity, Job } from "@/lib/types"

const RESTART_POLL_MS = 2_000
const RESTART_SLOW_MS = 4 * 60_000

/** Restarts the unit and follows it, until its console is back and this page can go to the login. */
export function useRestart() {
  const [restarting, setRestarting] = useState(false)
  const [slow, setSlow] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const alive = useRef(true)
  useEffect(() => {
    alive.current = true
    return () => {
      alive.current = false
    }
  }, [])

  async function restart() {
    setError(null)
    setRestarting(true)
    let job: Job
    try {
      job = await startJob("/api/v1/reboot")
    } catch (failure) {
      setRestarting(false)
      setError(plainError(failure))
      return
    }
    await follow(job)
  }

  /** Follows a job that restarts the unit, however it was started. */
  async function follow(job: Job) {
    setError(null)
    setRestarting(true)
    setSlow(false)
    // The job stays open across the restart, so it can only say that it failed. What shows the unit
    // has gone and come back: its console stops answering, or it answers without knowing this session.
    const since = Date.now()
    let away = false
    while (alive.current) {
      await sleep(RESTART_POLL_MS)
      try {
        const current = await api<Job>(`/api/v1/jobs/${job.id}`, { anonymous: true, timeoutMs: 5_000 })
        if (current.state === "failed") {
          setRestarting(false)
          setError(current.result ? `The unit couldn't restart: ${current.result}` : "The unit couldn't restart.")
          return
        }
        if (current.state === "succeeded") away = true
      } catch (failure) {
        if (failure instanceof ApiError && (failure.status === 0 || failure.status === 401)) away = true
      }
      if (away) {
        const back = await api<Identity>("/api/identity", { anonymous: true, timeoutMs: 5_000 }).catch(() => null)
        if (back?.release) {
          window.location.assign("/login/")
          return
        }
      }
      if (Date.now() - since > RESTART_SLOW_MS) setSlow(true)
    }
  }

  return { restarting, slow, error, restart, follow }
}

export function Restarting({ slow, children }: { slow: boolean; children: ReactNode }) {
  return (
    <div role="status" className="fixed inset-0 z-50 grid place-items-center bg-background p-6">
      <div className="grid max-w-sm justify-items-center gap-3 text-center">
        <Spinner className="size-8" />
        <p className="text-lg font-semibold">The unit is restarting</p>
        <Muted>{children}</Muted>
        {slow && (
          <Notice level="warn" title="This is taking longer than it should" className="text-left">
            Check that the unit has power and is on the network. If it came back with a new address, its screen shows
            it.
          </Notice>
        )}
      </div>
    </div>
  )
}

export function RestartButton({
  label,
  title,
  variant = "outline",
  onConfirm,
  children,
}: {
  label: string
  title: string
  variant?: "outline" | "default"
  onConfirm: () => void
  children: ReactNode
}) {
  const [asking, setAsking] = useState(false)
  return (
    <AlertDialog open={asking} onOpenChange={setAsking}>
      <AlertDialogTrigger render={<Button variant={variant} />}>{label}</AlertDialogTrigger>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>{title}</AlertDialogTitle>
          <AlertDialogDescription>{children}</AlertDialogDescription>
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogCancel>Cancel</AlertDialogCancel>
          <AlertDialogAction
            variant="destructive"
            onClick={() => {
              setAsking(false)
              onConfirm()
            }}
          >
            Restart
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  )
}

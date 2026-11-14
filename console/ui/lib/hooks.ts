"use client"

import { type FormEvent, useCallback, useEffect, useRef, useState } from "react"
import { usePathname } from "next/navigation"

import { api, jobFinished, plainError, startJob } from "@/lib/api"
import { readStatus, type Status } from "@/lib/report"
import type { Job } from "@/lib/types"

const STATUS_EVERY_MS = 10_000
const JOB_EVERY_MS = 2_000
/** How many polls in a row may fail before a job is given up on: a minute of no answer. */
const JOB_MISSES = 30

export const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms))

/** The current path without its trailing slash. */
export function usePath(): string {
  const path = usePathname() ?? "/"
  return path.length > 1 ? path.replace(/\/$/, "") : path
}

/** Runs a task now and on a timer, never overlapping, resting while the page is hidden. */
export function usePoll(task: () => Promise<void>, everyMs: number) {
  const latest = useRef(task)
  useEffect(() => {
    latest.current = task
  })

  useEffect(() => {
    let timer: ReturnType<typeof setTimeout> | undefined
    let stopped = false
    let running = false

    async function run() {
      if (running) return
      running = true
      await latest.current().catch(() => {})
      running = false
      if (!stopped) timer = setTimeout(tick, everyMs)
    }
    function tick() {
      if (document.hidden) timer = setTimeout(tick, everyMs)
      else void run()
    }
    function onVisibility() {
      if (!document.hidden) {
        clearTimeout(timer)
        void run()
      }
    }

    void run()
    document.addEventListener("visibilitychange", onVisibility)
    return () => {
      stopped = true
      clearTimeout(timer)
      document.removeEventListener("visibilitychange", onVisibility)
    }
  }, [everyMs])
}

/** The latest status, and what went wrong if the last attempt to fetch it failed. */
export function useStatus() {
  const [status, setStatus] = useState<Status | null>(null)
  const [error, setError] = useState<string | null>(null)
  usePoll(async () => {
    try {
      setStatus(readStatus(await api<unknown>("/api/v1/status")))
      setError(null)
    } catch (failure) {
      setError(plainError(failure))
    }
  }, STATUS_EVERY_MS)
  return { status, error }
}

/** Loads something once and can load it again. */
export function useLoad<T>(path: string) {
  const [data, setData] = useState<T | null>(null)
  const [error, setError] = useState<string | null>(null)
  const reload = useCallback(async () => {
    try {
      setData(await api<T>(path))
      setError(null)
    } catch (failure) {
      setError(plainError(failure))
    }
  }, [path])
  useEffect(() => {
    void reload()
  }, [reload])
  return { data, error, reload }
}

/** Starts jobs and follows one at a time until it's finished. */
export function useJob(onFinished?: (job: Job) => void) {
  const [job, setJob] = useState<Job | null>(null)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const alive = useRef(true)
  const finished = useRef(onFinished)
  useEffect(() => {
    finished.current = onFinished
  })
  useEffect(() => {
    alive.current = true
    return () => {
      alive.current = false
    }
  }, [])

  const follow = useCallback(async (first: Job) => {
    setJob(first)
    setBusy(true)
    let current = first
    let misses = 0
    while (alive.current && !jobFinished(current)) {
      await sleep(JOB_EVERY_MS)
      try {
        current = await api<Job>(`/api/v1/jobs/${current.id}`)
        misses = 0
        if (alive.current) setJob(current)
      } catch (failure) {
        misses += 1
        if (misses >= JOB_MISSES) {
          if (alive.current) setError(plainError(failure))
          break
        }
      }
    }
    if (!alive.current) return
    setBusy(false)
    if (jobFinished(current)) finished.current?.(current)
  }, [])

  const start = useCallback(
    async (path: string, body?: unknown) => {
      setBusy(true)
      setError(null)
      setJob(null)
      try {
        await follow(await startJob(path, body))
      } catch (failure) {
        setBusy(false)
        setError(plainError(failure))
      }
    },
    [follow]
  )

  return { job, busy, error, start, follow }
}

/** Runs a form's action, tracks that it's running, and keeps what went wrong to show. */
export function useSubmit(action: (form: FormData) => Promise<void>) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  async function onSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (busy) return
    const form = new FormData(event.currentTarget)
    setBusy(true)
    setError(null)
    try {
      await action(form)
    } catch (failure) {
      setError(plainError(failure))
    } finally {
      setBusy(false)
    }
  }

  return { busy, error, onSubmit }
}

export function field(form: FormData, name: string): string {
  const value = form.get(name)
  return typeof value === "string" ? value : ""
}

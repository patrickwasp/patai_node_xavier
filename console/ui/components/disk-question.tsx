"use client"

import { useState } from "react"

import { Choice } from "@/components/choice"
import { ErrorNote, Muted } from "@/components/common"
import { CurrentPassword } from "@/components/form-dialog"
import { Button } from "@/components/ui/button"
import { bytes } from "@/lib/format"
import { field, useJob, useSubmit } from "@/lib/hooks"
import type { DiskStatus } from "@/lib/types"

export const needsAnswer = (disk: DiskStatus) => disk.state === "undecided" || disk.state === "foreign"

export const diskTitle = (disk: DiskStatus) =>
  disk.state === "foreign" ? "This unit's disk isn't set up" : "This unit has a disk from before"

const ANSWERS = [
  {
    value: "keep",
    label: "Keep models and outputs",
    hint: "Old containers, images and recipes are removed, and the rest stays. Only do this if you trust whoever used the disk: a model can run code.",
  },
  {
    value: "erase",
    label: "Erase the disk",
    hint: "Everything on it is gone for good. Apps download what they need again.",
  },
]

function describe(disk: DiskStatus): string {
  const used = disk.used > 0 ? `about ${bytes(disk.used)}` : "data"
  return `The NVMe has ${used} on it from before this unit was flashed or reset.`
}

/**
 * Keep or erase. Until someone answers, the disk isn't even mounted. `password` is for someone who has
 * just typed theirs; anyone else is asked.
 */
export function DiskQuestion({
  disk,
  password,
  onDone,
}: {
  disk: DiskStatus
  password?: string
  onDone: () => void | Promise<void>
}) {
  const setUpOnly = disk.state === "foreign"
  const [answer, setAnswer] = useState(setUpOnly ? "erase" : "keep")
  const { job, busy, error, start } = useJob((finished) => {
    if (finished.state === "succeeded") void onDone()
  })
  const form = useSubmit(async (values) => {
    await start(`/api/v1/disk/${answer}`, { current_password: password ?? field(values, "current_password") })
  })
  const failure = job?.state === "failed" ? `That didn't work: ${job.result}` : null
  const erasing = answer === "erase"

  return (
    <form onSubmit={form.onSubmit} className="grid gap-4">
      {setUpOnly ? (
        <Muted>It may be new, from another machine, or damaged. Setting it up erases anything on it.</Muted>
      ) : (
        <>
          <Muted>{describe(disk)} Nothing on it can run or be read until you choose.</Muted>
          <Choice name="answer" label="What should happen to it?" options={ANSWERS} defaultValue="keep" onChange={setAnswer} />
        </>
      )}
      {password === undefined && <CurrentPassword />}
      <ErrorNote>{form.error ?? error ?? failure}</ErrorNote>
      <Button type="submit" size="lg" variant={erasing ? "destructive" : "default"} disabled={busy || form.busy}>
        {busy || form.busy ? "Working…" : setUpOnly ? "Erase and set it up" : erasing ? "Erase the disk" : "Keep them"}
      </Button>
    </form>
  )
}

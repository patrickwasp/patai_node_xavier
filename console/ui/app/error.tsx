"use client"

import { ErrorNote } from "@/components/common"
import { Button } from "@/components/ui/button"

export default function ErrorPage({ reset }: { reset: () => void }) {
  return (
    <main className="mx-auto grid min-h-svh max-w-sm content-center gap-3 px-4">
      <ErrorNote>This page couldn&apos;t be shown.</ErrorNote>
      <Button variant="outline" onClick={reset}>
        Try again
      </Button>
    </main>
  )
}

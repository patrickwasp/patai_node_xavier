"use client"

import { type ComponentProps, type ReactNode, useId, useState } from "react"
import { Check, CircleAlert, CircleCheck, CircleHelp, Copy, LoaderCircle, OctagonX, TriangleAlert } from "lucide-react"

import { Button } from "@/components/ui/button"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { copyText } from "@/lib/browser"
import type { Level } from "@/lib/health"
import { cn } from "@/lib/utils"

export const tones: Record<Level, { icon: typeof CircleCheck; box: string; solid: string; text: string }> = {
  good: {
    icon: CircleCheck,
    box: "border-good/40 bg-good/10",
    solid: "bg-primary text-primary-foreground",
    text: "text-good",
  },
  warn: {
    icon: TriangleAlert,
    box: "border-warn/50 bg-warn/10",
    solid: "bg-warn text-background",
    text: "text-warn",
  },
  bad: {
    icon: OctagonX,
    box: "border-destructive/50 bg-destructive/10",
    solid: "bg-destructive text-background",
    text: "text-destructive",
  },
  unknown: {
    icon: CircleHelp,
    box: "border-border bg-card",
    solid: "bg-muted-foreground text-background",
    text: "text-muted-foreground",
  },
}

export function Spinner({ className }: { className?: string }) {
  return <LoaderCircle aria-hidden className={cn("size-5 animate-spin motion-reduce:animate-none", className)} />
}

/** A sentence that needs to stand out, in the colour of its level. */
export function Notice({
  level,
  title,
  children,
  className,
}: {
  level: Level
  title?: ReactNode
  children?: ReactNode
  className?: string
}) {
  const tone = tones[level]
  const Icon = tone.icon
  return (
    <div className={cn("flex gap-3 rounded-lg border p-3 text-sm", tone.box, className)}>
      <Icon aria-hidden className={cn("mt-0.5 size-4 shrink-0", tone.text)} />
      <div className="grid min-w-0 gap-1">
        {title && <p className="font-medium">{title}</p>}
        {children && <div className="text-foreground/90">{children}</div>}
      </div>
    </div>
  )
}

export function ErrorNote({ children, className }: { children: ReactNode; className?: string }) {
  if (!children) return null
  return (
    <div
      role="alert"
      className={cn("flex gap-2 rounded-lg border border-destructive/50 bg-destructive/10 p-3 text-sm", className)}
    >
      <CircleAlert aria-hidden className="mt-0.5 size-4 shrink-0 text-destructive" />
      <p className="min-w-0">{children}</p>
    </div>
  )
}

export function Field({
  label,
  hint,
  id,
  ...input
}: { label: string; hint?: string } & ComponentProps<typeof Input>) {
  const generated = useId()
  const inputId = id ?? generated
  return (
    <div className="grid gap-1.5">
      <Label htmlFor={inputId}>{label}</Label>
      <Input id={inputId} aria-describedby={hint ? `${inputId}-hint` : undefined} {...input} />
      {hint && (
        <p id={`${inputId}-hint`} className="text-xs text-muted-foreground">
          {hint}
        </p>
      )}
    </div>
  )
}

export function CopyButton({ text, label = "Copy" }: { text: string; label?: string }) {
  const [result, setResult] = useState<"idle" | "copied" | "failed">("idle")
  async function copy() {
    setResult((await copyText(text)) ? "copied" : "failed")
    setTimeout(() => setResult("idle"), 2500)
  }
  return (
    <Button type="button" variant="outline" onClick={copy}>
      {result === "copied" ? <Check aria-hidden /> : <Copy aria-hidden />}
      {result === "copied" ? "Copied" : result === "failed" ? "Couldn't copy: select it yourself" : label}
    </Button>
  )
}

/** A card with a title. Most of the console is these. */
export function Panel({
  title,
  action,
  children,
  className,
  id,
}: {
  title: ReactNode
  action?: ReactNode
  children: ReactNode
  className?: string
  id?: string
}) {
  return (
    <Card id={id} className={cn("scroll-mt-20", className)}>
      <CardHeader className="flex items-center justify-between gap-3">
        <CardTitle>
          <h2>{title}</h2>
        </CardTitle>
        {action}
      </CardHeader>
      <CardContent className="grid gap-3">{children}</CardContent>
    </Card>
  )
}

/** One fact: a label on the left, its value on the right. */
export function Row({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="flex items-baseline justify-between gap-4">
      <dt className="shrink-0 text-muted-foreground">{label}</dt>
      <dd className="min-w-0 text-right">{children}</dd>
    </div>
  )
}

export function Muted({ children }: { children: ReactNode }) {
  return <p className="text-sm text-muted-foreground">{children}</p>
}

export function ProgressBar({ percent, label }: { percent: number; label: string }) {
  const value = Math.min(100, Math.max(0, percent))
  return (
    <div
      role="progressbar"
      aria-label={label}
      aria-valuemin={0}
      aria-valuemax={100}
      aria-valuenow={value}
      className="h-2 overflow-hidden rounded-full bg-muted"
    >
      <div className="h-full rounded-full bg-primary transition-[width]" style={{ width: `${value}%` }} />
    </div>
  )
}

export function PageTitle({ children, hint }: { children: ReactNode; hint?: ReactNode }) {
  return (
    <div className="grid gap-1">
      <h1 className="text-xl font-semibold">{children}</h1>
      {hint && <p className="text-sm text-muted-foreground">{hint}</p>}
    </div>
  )
}

/** The page shape for everything before a login: the unit's name over one card. */
export function SoloPage({
  name,
  title,
  children,
}: {
  name?: string
  title: string
  children: ReactNode
}) {
  return (
    <main className="mx-auto grid min-h-svh w-full max-w-md content-center gap-4 px-4 py-8">
      <div className="grid gap-1">
        {name && <p className="font-mono text-sm text-muted-foreground">{name}</p>}
        <h1 className="text-2xl font-semibold">{title}</h1>
      </div>
      {children}
    </main>
  )
}

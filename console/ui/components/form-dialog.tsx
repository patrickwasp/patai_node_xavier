"use client"

import type { ReactNode } from "react"

import { ErrorNote, Field } from "@/components/common"
import { Button } from "@/components/ui/button"
import {
  Dialog,
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import { useSubmit } from "@/lib/hooks"

type Props = {
  open: boolean
  onOpenChange: (open: boolean) => void
  title: string
  description?: ReactNode
  submit: string
  destructive?: boolean
  action: (form: FormData) => Promise<void>
  children?: ReactNode
}

/** A dialog that is one form. A failure stays in the dialog, and closing it forgets everything typed. */
export function FormDialog({ open, onOpenChange, ...rest }: Props) {
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent>
        <Body {...rest} />
      </DialogContent>
    </Dialog>
  )
}

function Body({ title, description, submit, destructive, action, children }: Omit<Props, "open" | "onOpenChange">) {
  const { busy, error, onSubmit } = useSubmit(action)
  return (
    <form onSubmit={onSubmit} className="grid gap-4">
      <DialogHeader>
        <DialogTitle>{title}</DialogTitle>
        {description && <DialogDescription>{description}</DialogDescription>}
      </DialogHeader>
      {children}
      <ErrorNote>{error}</ErrorNote>
      <DialogFooter>
        <DialogClose render={<Button type="button" variant="outline" />}>Cancel</DialogClose>
        <Button type="submit" variant={destructive ? "destructive" : "default"} disabled={busy}>
          {busy ? "Working…" : submit}
        </Button>
      </DialogFooter>
    </form>
  )
}

/** The fresh password the API wants for sensitive changes. */
export function CurrentPassword() {
  return (
    <Field
      label="Your password"
      name="current_password"
      type="password"
      autoComplete="current-password"
      hint="To confirm it's you."
      required
    />
  )
}

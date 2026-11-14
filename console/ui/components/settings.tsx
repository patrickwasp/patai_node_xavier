"use client"

import { useEffect, useId, useState } from "react"

import { ErrorNote, Muted } from "@/components/common"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { api, plainError } from "@/lib/api"
import type { SettingValue, WorkloadSetting, WorkloadStack } from "@/lib/types"

function shown(setting: WorkloadSetting, value: SettingValue): string {
  if (value === null) return "Model's"
  const label = setting.labels?.[String(value)]
  if (label) return label
  if (typeof value === "boolean") return value ? "On" : "Off"
  if (typeof value === "number") return `${value.toLocaleString()}${setting.unit ? ` ${setting.unit}` : ""}`
  return value.charAt(0).toUpperCase() + value.slice(1)
}

/** The choices a setting has for this model: its own, or none, up to the model's limit. */
function choicesOf(setting: WorkloadSetting, limit?: number): (number | string)[] | null {
  if (!setting.choices) return null
  return setting.choices.filter((choice) => typeof choice !== "number" || limit === undefined || choice <= limit)
}

function Control({
  setting,
  value,
  limit,
  disabled,
  onChange,
}: {
  setting: WorkloadSetting
  value: SettingValue
  limit?: number
  disabled: boolean
  onChange: (value: SettingValue) => void
}) {
  const id = useId()
  const choices = choicesOf(setting, limit)
  const label = (
    <Label htmlFor={id} className="font-medium">
      {setting.title}
    </Label>
  )
  if (setting.type === "bool") {
    return (
      <Label className="font-medium">
        <Checkbox checked={value === true} disabled={disabled} onCheckedChange={(checked) => onChange(checked === true)} />
        {setting.title}
      </Label>
    )
  }
  if (choices) {
    const options = value === null ? [null, ...choices] : choices
    return (
      <div className="grid gap-1.5">
        {label}
        <select
          id={id}
          value={String(value)}
          disabled={disabled}
          onChange={(event) => {
            const picked = options.find((option) => String(option) === event.target.value)
            onChange(picked === undefined ? null : picked)
          }}
          className="h-8 w-full min-w-0 rounded-lg border border-input bg-transparent px-2 text-sm outline-none focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50 disabled:opacity-50 dark:bg-input/30"
        >
          {options.map((option) => (
            <option key={String(option)} value={String(option)}>
              {shown(setting, option)}
            </option>
          ))}
        </select>
      </div>
    )
  }
  const high = limit ?? setting.max
  return (
    <div className="grid gap-1.5">
      {label}
      <Input
        id={id}
        type="number"
        inputMode="decimal"
        min={setting.min}
        max={high}
        step={setting.type === "int" ? 1 : 0.01}
        value={value === null ? "" : String(value)}
        placeholder="Model's"
        disabled={disabled}
        onChange={(event) => {
          const text = event.target.value.trim()
          onChange(text === "" ? null : Number(text))
        }}
      />
    </div>
  )
}

/** A service's settings for the model it runs. Saving a change the service reads restarts it; one only the console
 * uses doesn't. */
export function Settings({ stack, operate, onSaved }: { stack: WorkloadStack; operate: boolean; onSaved: () => void }) {
  const values = stack.values
  const [draft, setDraft] = useState<Record<string, SettingValue>>({})
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  // Another model has its own.
  useEffect(() => setDraft({}), [values?.model])
  if (!values) return <Muted>Not known yet.</Muted>

  const current = (name: string) => (name in draft ? draft[name] : values.values[name])
  const restarts = stack.settings.some((setting) => setting.name in draft && setting.for !== "gateway")
  const save = async (changes: Record<string, SettingValue>) => {
    setBusy(true)
    setError(null)
    try {
      await api(`/api/v1/workloads/${encodeURIComponent(stack.name)}/settings`, { method: "PUT", body: changes })
      setDraft({})
      onSaved()
    } catch (failure) {
      setError(plainError(failure))
    } finally {
      setBusy(false)
    }
  }
  // Null puts back a setting's default, the model's own included.
  const asked = Object.fromEntries(
    Object.entries(draft).map(([name, value]) => [name, value === values.defaults[name] ? null : value]),
  )

  return (
    <div className="grid gap-4">
      {values.model && <Muted>For {stack.models.find((model) => model.name === values.model)?.title || values.model}.</Muted>}
      <ul className="grid gap-4">
        {stack.settings.map((setting) => {
          const value = current(setting.name)
          const fallback = values.defaults[setting.name]
          return (
            <li key={setting.name} className="grid gap-1">
              <Control
                setting={setting}
                value={value}
                limit={values.limits[setting.name]}
                disabled={!operate || busy}
                onChange={(next) => setDraft((was) => ({ ...was, [setting.name]: next }))}
              />
              <p className="text-xs text-muted-foreground">
                {[setting.about, value !== fallback && `Default: ${shown(setting, fallback)}`].filter(Boolean).join(" ")}
              </p>
            </li>
          )
        })}
      </ul>
      <ErrorNote>{error}</ErrorNote>
      {operate && (
        <div className="flex flex-wrap items-center gap-2">
          <Button size="sm" disabled={busy || Object.keys(draft).length === 0} onClick={() => void save(asked)}>
            {busy ? "Saving…" : "Save"}
          </Button>
          {Object.keys(draft).length > 0 && (
            <Button size="sm" variant="ghost" disabled={busy} onClick={() => setDraft({})}>
              Undo
            </Button>
          )}
          {values.changed.length > 0 && Object.keys(draft).length === 0 && (
            <Button
              size="sm"
              variant="outline"
              disabled={busy}
              onClick={() => void save(Object.fromEntries(values.changed.map((name) => [name, null])))}
            >
              Reset to defaults
            </Button>
          )}
          {restarts && <span className="text-xs text-muted-foreground">Saving restarts it.</span>}
        </div>
      )}
    </div>
  )
}

/** An owner's secrets for a service, such as a search provider's key. Each is set or replaced, never shown. */
export function Secrets({ stack, owner, onSaved }: { stack: WorkloadStack; owner: boolean; onSaved: () => void }) {
  const [editing, setEditing] = useState<string | null>(null)
  const [value, setValue] = useState("")
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const id = useId()
  const put = async (name: string, secret: string) => {
    setBusy(true)
    setError(null)
    try {
      await api(`/api/v1/workloads/${encodeURIComponent(stack.name)}/secrets/${encodeURIComponent(name)}`, {
        method: "PUT",
        body: { value: secret },
      })
      setEditing(null)
      setValue("")
      onSaved()
    } catch (failure) {
      setError(plainError(failure))
    } finally {
      setBusy(false)
    }
  }
  // One given once, such as an account's first password, is the app's own to change.
  const secrets = stack.secrets.filter((secret) => !secret.once)
  if (secrets.length === 0) return null
  return (
    <div className="grid gap-3">
      <ul className="grid divide-y">
        {secrets.map((secret) => (
          <li key={secret.name} className="grid gap-2 py-2 first:pt-0 last:pb-0">
            <div className="flex items-center justify-between gap-2">
              <span className="text-sm">
                {secret.title}: <span className="text-muted-foreground">{secret.set ? "Set" : "Not set"}</span>
              </span>
              {owner && editing !== secret.name && (
                <div className="flex gap-1.5">
                  <Button size="sm" variant="outline" disabled={busy} onClick={() => setEditing(secret.name)}>
                    {secret.set ? "Replace" : "Set"}
                  </Button>
                  {secret.set && (
                    <Button size="sm" variant="ghost" disabled={busy} onClick={() => void put(secret.name, "")}>
                      Remove
                    </Button>
                  )}
                </div>
              )}
            </div>
            {editing === secret.name && (
              <form
                className="flex gap-1.5"
                onSubmit={(event) => {
                  event.preventDefault()
                  void put(secret.name, value)
                }}
              >
                <Label htmlFor={id} className="sr-only">
                  {secret.title}
                </Label>
                <Input id={id} type="password" autoComplete="off" value={value} onChange={(event) => setValue(event.target.value)} />
                <Button size="sm" type="submit" disabled={busy || value.trim() === ""}>
                  Save
                </Button>
                <Button size="sm" type="button" variant="ghost" disabled={busy} onClick={() => setEditing(null)}>
                  Cancel
                </Button>
              </form>
            )}
          </li>
        ))}
      </ul>
      <ErrorNote>{error}</ErrorNote>
    </div>
  )
}

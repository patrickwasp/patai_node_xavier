"use client"

import { useCallback, useEffect, useMemo, useState } from "react"

import { Choice } from "@/components/choice"
import { ErrorNote, Field, Muted, Panel, tones } from "@/components/common"
import { columnsOf, DataTable } from "@/components/data-table"
import { FormDialog } from "@/components/form-dialog"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip"
import {
  Dialog,
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import { api, plainError } from "@/lib/api"
import type { App } from "@/lib/types"

/** Models from services (docs/apps.md): chat models of outside services an owner adds with a key, for the apps they
 * choose. The key goes to the unit and is never shown again. */
type Fit = "good" | "unknown" | "no-tools" | "not-chat"
const FITS: Fit[] = ["good", "unknown", "no-tools", "not-chat"]
/** A service's model as the unit describes it: what it takes and gives, its effort levels, its price a million tokens,
 * and whether it suits patai. */
type Model = {
  id: string
  name: string
  maker?: string
  context?: number
  max_output?: number
  input: string[]
  output: string[]
  tools: boolean | null
  reasoning: boolean | null
  efforts?: string[]
  default_effort?: string
  intelligence?: number
  deprecated?: boolean
  price_in?: number
  price_out?: number
  created?: number
  fit: Fit
}
type Service = { id: string; kind: string; title: string; base: string; has_key: boolean; models: Model[] }
type Outside = { providers: Service[]; apps: string[] }

const KINDS = [
  { value: "openrouter", label: "OpenRouter", hint: "Many services' models with one key." },
  { value: "openai", label: "OpenAI", hint: "" },
  { value: "anthropic", label: "Anthropic", hint: "" },
  { value: "custom", label: "Other", hint: "Any service that answers as OpenAI's does." },
]

const slug = (text: string) =>
  text
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "")
    .slice(0, 30) || "service"

export function OutsideModels({ apps }: { apps: App[] }) {
  const [outside, setOutside] = useState<Outside | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [adding, setAdding] = useState(false)
  const [kind, setKind] = useState("openrouter")
  const [choosing, setChoosing] = useState<Service | null>(null)
  const [keying, setKeying] = useState<Service | null>(null)

  const load = useCallback(() => {
    api<Outside>("/api/v1/outside")
      .then(setOutside)
      .catch((failure) => setError(plainError(failure)))
  }, [])
  useEffect(load, [load])

  async function change(path: string, method: "PUT" | "DELETE", body?: unknown) {
    setError(null)
    try {
      setOutside(await api<Outside>(path, { method, body }))
    } catch (failure) {
      setError(plainError(failure))
    }
  }

  // The apps that can use a model of their own choosing: those with a key of their own, and owners' keys.
  const users = apps.filter((app) => app.key)
  return (
    <Panel title="Models from services">
      <Muted>Chat models of services such as OpenRouter. What an app sends them leaves the unit.</Muted>
      <ErrorNote>{error}</ErrorNote>
      {outside?.providers.map((service) => (
        <div key={service.id} className="grid gap-2 rounded-md border p-3">
          <div className="flex items-baseline justify-between gap-3">
            <span className="font-medium">{service.title}</span>
            <span className="text-sm text-muted-foreground">
              {service.models.length === 1 ? "1 model" : `${service.models.length} models`}
              {service.has_key ? "" : " · No key"}
            </span>
          </div>
          {service.kind === "custom" && <code className="font-mono text-xs break-all">{service.base}</code>}
          <div className="flex flex-wrap gap-2">
            <Button size="sm" variant="outline" onClick={() => setChoosing(service)}>
              Choose models
            </Button>
            <Button size="sm" variant="outline" onClick={() => setKeying(service)}>
              Change key
            </Button>
            <Button size="sm" variant="ghost" onClick={() => change(`/api/v1/outside/services/${service.id}`, "DELETE")}>
              Remove
            </Button>
          </div>
        </div>
      ))}
      <Button variant="outline" onClick={() => setAdding(true)}>
        Add a service
      </Button>
      {outside && outside.providers.length > 0 && (
        <fieldset className="grid gap-2">
          <legend className="mb-1 text-sm font-medium">Who can use them</legend>
          {[...users.map((app) => ({ name: app.name, title: app.title })), { name: "ai-api", title: "Owners' API keys" }].map(
            (user) => (
              <label key={user.name} className="flex items-center gap-2 text-sm">
                <Checkbox
                  checked={outside.apps.includes(user.name)}
                  onCheckedChange={(checked) =>
                    change("/api/v1/outside/apps", "PUT", {
                      apps: checked
                        ? [...outside.apps, user.name]
                        : outside.apps.filter((name) => name !== user.name),
                    })
                  }
                />
                {user.title}
              </label>
            ),
          )}
        </fieldset>
      )}
      <FormDialog
        open={adding}
        onOpenChange={setAdding}
        title="Add a service"
        description="Its key stays on the unit."
        submit="Add"
        action={async (form) => {
          const title = String(form.get("title") ?? "").trim()
          const id = kind === "custom" ? slug(title) : kind
          setOutside(
            await api<Outside>(`/api/v1/outside/services/${id}`, {
              method: "PUT",
              body: { kind, title, base: form.get("base") ?? "", key: form.get("key") ?? "" },
            }),
          )
          setAdding(false)
        }}
      >
        <Choice name="kind" label="Service" options={KINDS} defaultValue={kind} onChange={setKind} />
        {kind === "custom" && (
          <>
            <Field label="Name" name="title" required maxLength={60} />
            <Field label="Address" name="base" placeholder="https://example.com/v1" required />
          </>
        )}
        <Field label="API key" name="key" type="password" autoComplete="off" required={kind !== "custom"} />
      </FormDialog>
      <FormDialog
        open={keying !== null}
        onOpenChange={(open) => !open && setKeying(null)}
        title={`${keying?.title ?? ""} key`}
        submit="Save"
        action={async (form) => {
          if (!keying) return
          setOutside(
            await api<Outside>(`/api/v1/outside/services/${keying.id}`, {
              method: "PUT",
              body: { kind: keying.kind, title: keying.title, base: keying.base, key: form.get("key") ?? "" },
            }),
          )
          setKeying(null)
        }}
      >
        <Field label="API key" name="key" type="password" autoComplete="off" required />
      </FormDialog>
      {choosing && (
        <ChooseModels
          service={choosing}
          onClose={() => setChoosing(null)}
          onSaved={(saved) => {
            setOutside(saved)
            setChoosing(null)
          }}
        />
      )}
    </Panel>
  )
}

const FIT: Record<Fit, string> = {
  good: "Fits",
  unknown: "Not known",
  "no-tools": "No tools",
  "not-chat": "Not chat",
}

const INPUTS: Record<string, string> = { image: "Images", file: "Files", audio: "Audio", video: "Video" }

const tokens = (n: number) =>
  n >= 1_000_000 ? `${Math.round(n / 100_000) / 10}M` : n >= 1000 ? `${Math.round(n / 1000)}K` : String(n)
const dollars = (n: number | undefined) =>
  n === undefined ? "" : n === 0 ? "Free" : `$${n < 10 ? n.toFixed(2) : n.toFixed(0)}`

const modelColumns = columnsOf<Model>()

/** A service's models, described, to offer some of: those that fit patai first. */
function ChooseModels({
  service,
  onClose,
  onSaved,
}: {
  service: Service
  onClose: () => void
  onSaved: (outside: Outside) => void
}) {
  const [catalogue, setCatalogue] = useState<Model[] | null>(null)
  const [chosen, setChosen] = useState<string[]>(service.models.map((m) => m.id).filter(Boolean))
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  useEffect(() => {
    api<{ models: Model[] }>(`/api/v1/outside/services/${service.id}/catalogue`)
      .then((listed) => setCatalogue(listed.models))
      .catch((failure) => setError(plainError(failure)))
  }, [service.id])

  const columns = useMemo(
    () =>
      modelColumns.columns([
        // Filtered by its name and its id.
        modelColumns.accessor((m) => `${m.name} ${m.id}`, {
          id: "name",
          header: "Model",
          meta: { className: "whitespace-normal" },
          cell: (cell) => (
            <label className="flex items-start gap-2">
              <Checkbox
                className="mt-0.5"
                checked={chosen.includes(cell.row.original.id)}
                onCheckedChange={(checked) =>
                  setChosen((list) =>
                    checked ? [...list, cell.row.original.id] : list.filter((id) => id !== cell.row.original.id),
                  )
                }
              />
              <span className="grid min-w-0 gap-0.5">
                <span className="font-medium">
                  {cell.row.original.name}
                  {cell.row.original.deprecated && <span className="font-normal text-muted-foreground"> · Ending</span>}
                </span>
                <code className="font-mono text-xs break-all text-muted-foreground">{cell.row.original.id}</code>
              </span>
            </label>
          ),
        }),
        modelColumns.accessor((m) => FIT[m.fit], {
          id: "fit",
          header: "Fit",
          meta: { facet: {} },
          sortFn: (a, b) => FITS.indexOf(a.original.fit) - FITS.indexOf(b.original.fit),
          cell: (cell) => (
            <span className={cell.row.original.fit === "good" ? tones.good.text : "text-muted-foreground"}>
              {cell.getValue()}
            </span>
          ),
        }),
        modelColumns.accessor((m) => m.intelligence ?? -1, {
          id: "intelligence",
          // Credited where it's read, in the header's tooltip.
          header: () => (
            <Tooltip>
              <TooltipTrigger render={<span />}>Intelligence</TooltipTrigger>
              <TooltipContent>Artificial Analysis Intelligence Index, from OpenRouter</TooltipContent>
            </Tooltip>
          ),
          enableGlobalFilter: false,
          meta: { numeric: true, title: "Intelligence" },
          cell: (cell) => cell.row.original.intelligence?.toFixed(1) ?? "",
        }),
        modelColumns.accessor((m) => m.maker ?? "", { id: "maker", header: "Maker", meta: { facet: {} } }),
        modelColumns.accessor(
          (m) =>
            m.input
              .filter((kind) => INPUTS[kind])
              .map((kind) => INPUTS[kind])
              .join(", "),
          { id: "input", header: "Takes", meta: { className: "whitespace-normal text-xs" } },
        ),
        modelColumns.accessor(
          (m) =>
            m.efforts?.length
              ? m.efforts.map((level) => (level === m.default_effort ? `${level} (default)` : level)).join(", ")
              : m.reasoning
                ? "Yes"
                : "",
          { id: "reasoning", header: "Effort", meta: { className: "whitespace-normal text-xs" } },
        ),
        modelColumns.accessor((m) => m.context ?? 0, {
          id: "context",
          header: "Context",
          enableGlobalFilter: false,
          meta: { numeric: true },
          cell: (cell) => (cell.getValue() ? tokens(cell.getValue()) : ""),
        }),
        modelColumns.accessor((m) => m.price_in ?? -1, {
          id: "price_in",
          header: "In, $/M",
          enableGlobalFilter: false,
          meta: { numeric: true },
          cell: (cell) => dollars(cell.row.original.price_in),
        }),
        modelColumns.accessor((m) => m.price_out ?? -1, {
          id: "price_out",
          header: "Out, $/M",
          enableGlobalFilter: false,
          meta: { numeric: true },
          cell: (cell) => dollars(cell.row.original.price_out),
        }),
        modelColumns.accessor((m) => m.created ?? 0, {
          id: "created",
          header: "Released",
          enableGlobalFilter: false,
          cell: (cell) => (cell.getValue() ? new Date(cell.getValue() * 1000).toLocaleDateString() : ""),
        }),
      ]),
    [chosen],
  )

  return (
    <Dialog open onOpenChange={(open) => !open && onClose()}>
      <DialogContent className="sm:max-w-5xl">
        <DialogHeader>
          <DialogTitle>{service.title} models</DialogTitle>
          <DialogDescription>
            The apps offer those chosen. patai needs tool calling for search, memory and documents.
          </DialogDescription>
        </DialogHeader>
        <ErrorNote>{error}</ErrorNote>
        {catalogue === null && !error && <Muted>Loading…</Muted>}
        {catalogue && (
          <DataTable
            id={`models-${service.kind}`}
            label={`${service.title} models`}
            data={catalogue}
            columns={columns}
            rowId={(m) => m.id}
            empty="The service lists no models."
            hidden={service.kind === "openrouter" ? ["created"] : ["maker", "created"]}
            filters={catalogue.some((m) => m.fit === "good") ? [{ id: "fit", value: [FIT.good] }] : []}
            sort={[{ id: "fit", desc: false }]}
            pageSize={20}
          />
        )}
        <DialogFooter>
          <span className="mr-auto self-center text-sm text-muted-foreground">
            {chosen.length === 1 ? "1 chosen" : `${chosen.length} chosen`}
          </span>
          <DialogClose render={<Button type="button" variant="outline" />}>Cancel</DialogClose>
          <Button
            disabled={busy}
            onClick={async () => {
              setBusy(true)
              setError(null)
              try {
                onSaved(
                  await api<Outside>(`/api/v1/outside/services/${service.id}/models`, {
                    method: "PUT",
                    body: { models: chosen },
                  }),
                )
              } catch (failure) {
                setError(plainError(failure))
              } finally {
                setBusy(false)
              }
            }}
          >
            {busy ? "Saving…" : "Save"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

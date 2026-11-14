"use client"

import { useMemo, useState } from "react"
import { Combobox } from "@base-ui/react/combobox"
import { Check, ChevronsUpDown } from "lucide-react"

/** One option: what it's called, and a line of what it is. Search matches both. */
export type PickerItem = { value: string; label: string; detail: string }

/** A list for a form, in the order given, with a search box at its top. The form gets the chosen value under name. */
export function Picker({
  name,
  label,
  items,
  defaultValue,
  onChange,
}: {
  name: string
  label: string
  items: PickerItem[]
  defaultValue: string
  onChange?: (value: string) => void
}) {
  const [value, setValue] = useState(defaultValue)
  const chosen = items.find((item) => item.value === value)
  const collection = useMemo(
    () => Combobox.createItems(items, { getValue: (item) => item.value, getLabel: (item) => item.label }),
    [items]
  )
  return (
    <Combobox.Root
      items={collection}
      name={name}
      value={value}
      required
      autoHighlight
      filter={(item: PickerItem, query: string) =>
        `${item.label} ${item.detail}`.toLowerCase().includes(query.trim().toLowerCase())
      }
      onValueChange={(next) => {
        if (next === null) return
        setValue(String(next))
        onChange?.(String(next))
      }}
    >
      <div className="grid gap-1.5">
        <Combobox.Label className="text-sm font-medium">{label}</Combobox.Label>
        <Combobox.Trigger className="flex min-h-11 w-full items-center justify-between gap-3 rounded-lg border border-input px-3 py-2 text-left outline-none focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50">
          <span className="grid min-w-0 gap-0.5">
            <span className="text-sm font-medium">{chosen?.label ?? "Choose one"}</span>
            {chosen && <span className="text-xs text-muted-foreground">{chosen.detail}</span>}
          </span>
          <ChevronsUpDown aria-hidden className="size-4 shrink-0 text-muted-foreground" />
        </Combobox.Trigger>
      </div>
      <Combobox.Portal>
        <Combobox.Positioner className="isolate z-50 outline-none" align="start" sideOffset={4}>
          <Combobox.Popup
            aria-label={label}
            className="w-(--anchor-width) max-w-(--available-width) rounded-lg bg-popover p-1 text-popover-foreground shadow-md ring-1 ring-foreground/10"
          >
            <Combobox.Input
              placeholder="Search"
              className="mb-1 h-9 w-full rounded-md border border-input bg-transparent px-2 text-base outline-none placeholder:text-muted-foreground focus-visible:border-ring md:text-sm"
            />
            <Combobox.Empty>
              <p className="p-2 text-sm text-muted-foreground">Nothing matches.</p>
            </Combobox.Empty>
            <Combobox.List className="max-h-[min(20rem,calc(var(--available-height)-3rem))] overflow-y-auto overscroll-contain outline-none">
              {(item: PickerItem) => (
                <Combobox.Item
                  key={item.value}
                  value={item.value}
                  className="grid cursor-default grid-cols-[1rem_1fr] items-start gap-2 rounded-md p-2 text-sm outline-none select-none data-highlighted:bg-accent data-highlighted:text-accent-foreground"
                >
                  <Combobox.ItemIndicator className="mt-0.5">
                    <Check aria-hidden className="size-4" />
                  </Combobox.ItemIndicator>
                  <span className="col-start-2 grid gap-0.5">
                    <span className="font-medium">{item.label}</span>
                    <span className="text-xs text-muted-foreground">{item.detail}</span>
                  </span>
                </Combobox.Item>
              )}
            </Combobox.List>
          </Combobox.Popup>
        </Combobox.Positioner>
      </Combobox.Portal>
    </Combobox.Root>
  )
}

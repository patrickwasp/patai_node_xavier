"use client"

import { Label } from "@/components/ui/label"
import { RadioGroup, RadioGroupItem } from "@/components/ui/radio-group"

/** One of a few options, each with a line saying what it means. */
export function Choice({
  name,
  options,
  defaultValue,
  label,
  onChange,
}: {
  name: string
  options: { value: string; label: string; hint: string }[]
  defaultValue: string
  label: string
  onChange?: (value: string) => void
}) {
  return (
    <div className="grid gap-1.5">
      <p id={`${name}-label`} className="text-sm font-medium">
        {label}
      </p>
      <RadioGroup
        name={name}
        defaultValue={defaultValue}
        aria-labelledby={`${name}-label`}
        onValueChange={(value) => onChange?.(String(value))}
      >
        {options.map((option) => (
          <Label
            key={option.value}
            className="items-start gap-3 rounded-lg border p-3 font-normal has-data-checked:border-ring"
          >
            <RadioGroupItem value={option.value} className="mt-0.5" />
            <span className="grid gap-1">
              <span className="font-medium">{option.label}</span>
              <span className="leading-snug text-muted-foreground">{option.hint}</span>
            </span>
          </Label>
        ))}
      </RadioGroup>
    </div>
  )
}

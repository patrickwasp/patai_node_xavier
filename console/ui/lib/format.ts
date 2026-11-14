/** Bytes in the units disks are sold in. */
export function bytes(value: number): string {
  const units = ["B", "KB", "MB", "GB", "TB"]
  let amount = value
  let unit = 0
  while (amount >= 1000 && unit < units.length - 1) {
    amount /= 1000
    unit += 1
  }
  const digits = amount >= 100 || unit === 0 ? 0 : 1
  return `${amount.toFixed(digits)} ${units[unit]}`
}

function plural(count: number, word: string): string {
  return `${count} ${word}${count === 1 ? "" : "s"}`
}

export function duration(seconds: number): string {
  if (seconds < 90) return plural(Math.max(0, Math.round(seconds)), "second")
  const minutes = Math.round(seconds / 60)
  if (minutes < 60) return plural(minutes, "minute")
  const hours = Math.round(minutes / 60)
  if (hours < 48) return plural(hours, "hour")
  return plural(Math.round(hours / 24), "day")
}

/** A date from the unit, or null if there's none to show. It may be wrong: a unit may not know the date. */
export function when(value: string | null | undefined): string | null {
  if (!value) return null
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return value
  return new Intl.DateTimeFormat(undefined, { dateStyle: "medium", timeStyle: "short" }).format(date)
}

export function capitalise(value: string): string {
  return value.charAt(0).toUpperCase() + value.slice(1)
}

/**
 * Copies text. navigator.clipboard only exists in a secure context, which a self-signed certificate may
 * not get, so the old way is the fallback. False means it didn't work and the person must copy by hand.
 */
export async function copyText(text: string): Promise<boolean> {
  if (window.isSecureContext && navigator.clipboard) {
    try {
      await navigator.clipboard.writeText(text)
      return true
    } catch {
      // Refused, so try the old way.
    }
  }

  const area = document.createElement("textarea")
  area.value = text
  area.readOnly = true
  area.style.position = "fixed"
  area.style.opacity = "0"
  // An open dialog keeps focus inside itself, so the textarea has to be in there too.
  const host = document.activeElement?.closest("[role=dialog], [role=alertdialog]") ?? document.body
  const focused = document.activeElement as HTMLElement | null
  host.append(area)
  area.select()
  area.setSelectionRange(0, text.length)
  let copied = false
  try {
    copied = document.execCommand("copy")
  } catch {
    copied = false
  }
  area.remove()
  focused?.focus()
  return copied
}

/** Saves text as a file, made in the browser. */
export function downloadText(filename: string, text: string) {
  const url = URL.createObjectURL(new Blob([text], { type: "text/plain;charset=utf-8" }))
  const link = document.createElement("a")
  link.href = url
  link.download = filename
  document.body.append(link)
  link.click()
  link.remove()
  setTimeout(() => URL.revokeObjectURL(url), 60_000)
}

let claimCode = ""

/**
 * The claim code from a #claim=CODE fragment. The first call takes it out of the address, so it isn't
 * left in the browser's history or on the screen. Later calls return it from memory.
 */
export function takeClaimCode(): string {
  if (typeof window === "undefined") return ""
  const fragment = window.location.hash.slice(1)
  if (fragment.startsWith("claim=")) {
    claimCode = new URLSearchParams(fragment).get("claim")?.trim() ?? ""
    window.history.replaceState(window.history.state, "", window.location.pathname + window.location.search)
  }
  return claimCode
}

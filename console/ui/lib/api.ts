import type { Job } from "@/lib/types"

/** Sent when the API says the unit isn't in a usable state, so the gate looks again. */
export const RECHECK_EVENT = "patai:recheck"

const LOGIN_PAGE = "/login/"

export class ApiError extends Error {
  constructor(
    readonly status: number,
    readonly code: string,
    message: string
  ) {
    super(message)
  }
}

/** A problem with what someone typed, found before asking the unit. */
export class FormError extends Error {}

type Options = {
  method?: "GET" | "POST" | "PUT" | "DELETE"
  body?: unknown
  headers?: Record<string, string>
  timeoutMs?: number
  /** Hand a 401 back to the caller instead of going to the login page. */
  anonymous?: boolean
}

export async function api<T = void>(path: string, options: Options = {}): Promise<T> {
  const { method = "GET", body, headers = {}, timeoutMs = 30_000, anonymous = false } = options
  if (body !== undefined) headers["Content-Type"] = "application/json"

  // A unit that has gone away doesn't always refuse the connection: a phone can wait a long time.
  const controller = new AbortController()
  const timer = setTimeout(() => controller.abort(), timeoutMs)
  let response: Response
  try {
    response = await fetch(path, {
      method,
      headers,
      body: body === undefined ? undefined : JSON.stringify(body),
      credentials: "same-origin",
      cache: "no-store",
      signal: controller.signal,
    })
  } catch {
    throw new ApiError(0, "unreachable", "the unit didn't answer")
  } finally {
    clearTimeout(timer)
  }

  if (response.ok) {
    if (response.status === 204) return undefined as T
    return (await response.json()) as T
  }

  const failure = await response.json().catch(() => null)
  const error = refusal(response.status, failure)
  if (error.code === "login" && !anonymous) return leaveForLogin()
  throw error
}

function refusal(status: number, failure: { code?: unknown; error?: unknown } | null): ApiError {
  const code = typeof failure?.code === "string" ? failure.code : "unknown"
  const message = typeof failure?.error === "string" ? failure.error : "something went wrong"
  if (code === "recovery") window.dispatchEvent(new Event(RECHECK_EVENT))
  return new ApiError(status, code, message)
}

function leaveForLogin<T>(): Promise<T> {
  window.location.assign(LOGIN_PAGE)
  // The page is leaving. Nothing should render this as an error on the way out.
  return new Promise<T>(() => {})
}

/** Sends a file as the body of a POST. fetch can't say how far an upload has got, so this is XHR. */
export function sendFile<T>(
  path: string,
  file: File,
  onProgress: (fraction: number) => void,
  method: "POST" | "PUT" = "POST"
): Promise<T> {
  return new Promise<T>((resolve, reject) => {
    const request = new XMLHttpRequest()
    request.open(method, path)
    request.setRequestHeader("Content-Type", "application/octet-stream")
    request.upload.onprogress = (event) => {
      if (event.lengthComputable) onProgress(event.loaded / event.total)
    }
    request.onerror = () => reject(new ApiError(0, "unreachable", "the unit didn't answer"))
    request.onload = () => {
      let body = null
      try {
        body = JSON.parse(request.responseText)
      } catch {
        // A proxy's error page, or nothing. The status says enough.
      }
      if (request.status >= 200 && request.status < 300) {
        resolve(body as T)
        return
      }
      const error = refusal(request.status, body)
      if (error.code === "login") void leaveForLogin()
      else reject(error)
    }
    request.send(file)
  })
}

/** Asks for a file and saves it as name. It's held in memory on the way, so it's for small files. */
export async function download(path: string, name: string, timeoutMs = 120_000): Promise<void> {
  const controller = new AbortController()
  const timer = setTimeout(() => controller.abort(), timeoutMs)
  let blob: Blob
  try {
    const response = await fetch(path, {
      method: "POST",
      credentials: "same-origin",
      cache: "no-store",
      signal: controller.signal,
    })
    if (!response.ok) {
      const error = refusal(response.status, await response.json().catch(() => null))
      if (error.code === "login") return leaveForLogin()
      throw error
    }
    blob = await response.blob()
  } catch (error) {
    if (error instanceof ApiError) throw error
    throw new ApiError(0, "unreachable", "the unit didn't answer")
  } finally {
    clearTimeout(timer)
  }
  const url = URL.createObjectURL(blob)
  const link = document.createElement("a")
  link.href = url
  link.download = name
  document.body.append(link)
  link.click()
  link.remove()
  setTimeout(() => URL.revokeObjectURL(url), 60_000)
}

/** What to tell a person about any failure. */
export function plainError(error: unknown): string {
  if (error instanceof FormError) return error.message
  if (error instanceof ApiError) {
    switch (error.code) {
      case "unreachable":
        return "The unit didn't answer. Check that you're still on its network, then try again."
      case "starting":
        return "The unit is still starting. Try again in a minute."
      case "throttled":
        return "Too many attempts. Wait a while, then try again."
      case "storage":
        return "The unit can't record this change, so it wasn't made. Its storage may be full."
      case "recovery":
        return "This unit needs service."
    }
    return sentence(error.message)
  }
  return "Something went wrong. Try again."
}

/** The API's messages are lower case, to fit inside a sentence. */
function sentence(text: string): string {
  const first = text.charAt(0).toUpperCase() + text.slice(1)
  return /[.!?]$/.test(first) ? first : `${first}.`
}

/** A random string. crypto.randomUUID needs a secure context, which a self-signed certificate may not get. */
function randomKey(): string {
  const bytes = crypto.getRandomValues(new Uint8Array(16))
  return Array.from(bytes, (byte) => byte.toString(16).padStart(2, "0")).join("")
}

/** Asks the unit to do something slow. Call once per click, so a retry of the same click can't do it twice. */
export function startJob(path: string, body?: unknown): Promise<Job> {
  return api<Job>(path, { method: "POST", body, headers: { "Idempotency-Key": randomKey() } })
}

export function jobFinished(job: Job): boolean {
  return job.state === "succeeded" || job.state === "failed"
}

// What the API sends, from console/internal/api and console/internal/store.

export type Identity = {
  product?: string
  name: string
  release: string
  api_versions?: string[]
  state: "unclaimed" | "claimed" | "unclaimable" | "recovery"
  fault?: string
  fingerprint?: string
}

export type Session = {
  name: string
  role: string
  kind: string
}

export type Job = {
  id: number
  kind: string
  target: string
  actor: string
  state: "queued" | "running" | "succeeded" | "failed"
  result: string
  boot_id: string
  created_at: string
  updated_at: string
}

export type User = {
  id: number
  name: string
  role: string
  created_at: string
}

export type Token = {
  id: number
  name: string
  scope: string
  created_by: string
  created_at: string
  expires_at: string | null
}

export type DiskStatus = {
  state: "ready" | "undecided" | "foreign" | "absent" | "unmounted"
  size: number
  used: number
}

export type SshKey = {
  id: string
  type: string
  fingerprint: string
  comment: string
}

export type UpdateSlot = {
  name: string
  state: string
  version: string
  booted: boolean
  primary: boolean
}

export type UpdateCheck = {
  time: string
  ok: boolean
  detail: string
  offered?: string
  last_success?: string
  /** From a file the fleet signed; missing when the check found none. */
  channel?: string
  /** Unix seconds when the channel's file was signed. */
  sequence?: number
}

export type UpdateInstall = {
  time: string
  version: string
  source: string
  state: string
  detail: string
}

export type UploadedBundle = {
  version: string
  size: number
}

export type Updates = {
  channel: string
  channel_name: string
  auto_install: boolean
  auto_restart: boolean
  slots: UpdateSlot[]
  check: UpdateCheck | null
  tried: string
  busy: boolean
  installing: boolean
  percent: number
  step: string
  history: UpdateInstall[]
  upload: UploadedBundle | null
}

export type WorkloadPart = {
  name: string
  size: number
  here: boolean
  unchecked?: boolean
  partial?: number
}

/** A service of an app, as it runs now. Memory is its own; GPU memory is nvmap's count for its processes. */
export type WorkloadService = {
  name: string
  title: string
  image: string
  state: string
  healthy: boolean | null
  started_at?: string
  restarts: number
  memory: number | null
  gpu_memory: number | null
}

export type WorkloadStack = {
  name: string
  title: string
  version: string
  services: WorkloadService[]
  image: WorkloadPart
  models: WorkloadModel[]
  /** It runs one model at a time, chosen with Use. Without, it sees every model it has. */
  one_model: boolean
  /** What an owner's own model can be: gguf, safetensors. */
  formats: string[]
  /** What an owner can change, and their values for the model it runs. */
  settings: WorkloadSetting[]
  values?: WorkloadSettings
  /** An owner's secrets for it, such as a provider's key: only whether each is set. */
  /** Once: passed until the service has answered, such as the password of an account it makes at its first start. */
  secrets: { name: string; title: string; set: boolean; once?: boolean; used?: boolean }[]
  /** What it last said of its data, such as a migration that failed, and its Postgres clusters (docs/updates.md, Data
   * across slots). */
  data?: { record?: DataRecord; clusters?: { here: { Major: number; Gen: number }[] | null } }
}

/** A service's format.json: the version its data is at, and how its last start went. */
export type DataRecord = {
  version: number
  floor: number
  state: "ready" | "migrating" | "failed"
  error?: string
  detail?: string
  release?: string
}

/** A setting's value. Null is the model's own, which the service isn't told. */
export type SettingValue = number | string | boolean | null

export type WorkloadSetting = {
  name: string
  title: string
  about?: string
  type: "int" | "number" | "choice" | "bool"
  min?: number
  max?: number
  choices?: (number | string)[]
  /** What a choice is called, where its value won't do. */
  labels?: Record<string, string>
  unit?: string
  default: SettingValue
  /** "gateway" for one the console uses, which needs no restart. */
  for?: string
}

export type WorkloadSettings = {
  model: string
  values: Record<string, SettingValue>
  defaults: Record<string, SettingValue>
  changed: string[]
  limits: Record<string, number>
}

export type WorkloadModel = WorkloadPart & {
  title?: string
  kept?: boolean
  in_use?: boolean
  own?: boolean
  source?: string
}

export type WorkloadFetch = {
  stack: string
  step: "image" | "model" | "checking" | "loading" | "ready" | "failed" | "stopped"
  total: number
  done: number
  source: string
  detail: string
  /** An owner's model, if that's what it's getting. */
  adding?: string
  time: string
}

export type AppAPI = {
  name: string
  title: string
  about: string
  /** The service behind it. */
  service: string
  always?: boolean
  default?: boolean
  /** Run for each person who uses it, rather than with the app. */
  person?: boolean
  routes: string[]
}

/** An app: what an owner turns on, made of services (docs/apps.md). */
export type App = {
  name: string
  title: string
  about: string
  apis: AppAPI[]
  services: string[] | null
  /** Set for an app that reaches its APIs with a key of its own, which owners' keys can't use. */
  key?: string
  /** Its own web pages, on a port of their own, where people sign in with its own accounts. */
  ui?: { port: number; service: string }
  on: boolean
  apis_on: string[]
  /** The services it runs with its APIs as they are. */
  running: string[]
  /** Services it names that this release doesn't have. */
  missing?: string[]
}

export type WorkloadStatus = {
  /** The apps that are on. */
  chosen: string[]
  /** The services they need. */
  running: string[]
  apps: App[]
  disk: string
  busy: boolean
  fetch: WorkloadFetch | null
  /** Every service this release has. */
  stacks: WorkloadStack[]
  /** What the apps that are on need, by service, against the budget for apps, in MiB. */
  memory: { budget: number; services: Record<string, number> } | null
  /** Each service run for each person: the people whose runs. */
  people?: Record<string, string[]> | null
}

/** An API key's limits. Zero is none, and no APIs is all of them. */
export type KeyLimits = {
  apis: string[]
  max_concurrent: number
  max_body: number
  tokens_per_day: number
  images_per_day: number
}

export type Usage = {
  day: string
  key_id: number
  key: string
  requests: number
  errors: number
  prompt_tokens: number
  completion_tokens: number
  images: number
  millis: number
}

export type APIKey = Token & { limits: KeyLimits; today: Usage | null }

export type APIRequest = {
  id: number
  at: string
  key_id: number
  key: string
  api: string
  endpoint: string
  model: string
  status: number
  prompt_tokens: number
  completion_tokens: number
  images: number
  millis: number
}

export type LogEntry = {
  cursor?: string
  time_us: number
  priority: number
  unit?: string
  ident?: string
  pid?: string
  stream?: "stdout" | "stderr"
  message: string
}

export type LogPage = {
  entries: LogEntry[]
  cursor: string
  more: boolean
  /** Why there's nothing to read: no data disk in use, or the workload has no container. */
  absent?: "disk" | "container"
  /** Following couldn't reach back to where it was, so lines were missed. */
  gap?: boolean
}

export type LogBoot = {
  id: string
  /** 0 is this start, -1 the one before. */
  index: number
  first?: string
  last?: string
}

export type LogIndex = {
  sources: { id: string; bundle: boolean }[]
  boots: LogBoot[]
}

export type AuditEntry = {
  seq: number
  boot_id: string
  at: string
  actor: string
  origin: string
  source: string
  action: string
  target: string
  result: string
}

export type NetworkSetting = {
  mode: "dhcp" | "static"
  address?: string
  gateway?: string
  dns: string[]
}

export type NetworkPort = {
  id: string
  label: string
  connector: string
  interface: string | null
  carrier: boolean
  speed_mbps: number | null
  mac: string
  addresses: string[]
  link_local: string
  ipv6: string[]
  gateway: string
  dns: string[]
  saved: NetworkSetting
}

export type NetworkTrial = {
  id: string
  port: string
  setting: NetworkSetting
  applied: boolean
  /** Going back, and no longer to be kept. */
  reverting: boolean
  left_s: number
  urls: string[]
}

export type NetworkLast = {
  id: string
  port: string
  setting: NetworkSetting
  outcome: "kept" | "undone" | "went back"
  time: string
}

export type NetworkStatus = {
  ports: NetworkPort[]
  trial: NetworkTrial | null
  last: NetworkLast | null
  problem: string
  trial_s: number
  /** The unit's address this request came in on. */
  via: string
}

/** GET /api/v1/clock, from patai-clock. */
export type ClockStatus = {
  now_ms: number
  synced: boolean
  ntp: boolean
  server: string | null
  /** The backup RTC had lost the time when the unit started: it has no battery, or a flat one. */
  rtc_lost: boolean
  /** The last time an owner set it, this boot. */
  set: { from_ms: number; to_ms: number; ago_s: number } | null
  earliest_ms: number
  latest_ms: number
  /** Only after a setting: whether the battery RTC and timesyncd's saved time took it. */
  rtc_written?: boolean
  floor_saved?: boolean
}

/** One nvpmodel mode. A null limit is no limit. */
export type PowerModeInfo = { id: number; name: string; cores: number; cpu_mhz: number | null; gpu_mhz: number | null }

/** GET /api/v1/power-mode, from patai-power-mode. */
export type PowerStatus = {
  modes: PowerModeInfo[]
  default: number | null
  current: number | null
  saved: number | null
}

/** An owner's certificate, as the unit checked it. The key never leaves the unit. */
export type OwnerCertificate = {
  /** Where it's served. */
  names: string[]
  /** In it, but the unit's own certificate answers there. */
  unused: string[]
  subject: string
  issuer: string
  not_before: string
  not_after: string
  fingerprint: string
  chain: number
  /** The unit's clock may be wrong, so its dates weren't checked. */
  dates_unsure: boolean
}

/** GET /api/v1/certificate. */
export type CertificateStatus = {
  unit_fingerprint: string
  owner: OwnerCertificate | null
  /** Why a kept certificate isn't in use. */
  problem: string
  /** False in recovery mode, which has nowhere to keep one. */
  available: boolean
}

/** GET /api/v1/resources: how busy the unit is now. Bytes, and percent of all cores or of the GPU. */
export type Resources = {
  cpu_percent: number | null
  gpu_percent: number | null
  memory_total: number
  memory_used: number
  gpu_memory: number | null
  swap_total: number
  swap_used: number
}

/** Remote access with Tailscale: tailscaled's state ("off", "starting", "NeedsLogin", "NeedsMachineAuth", "Running"…),
 *  the login URL while it waits for one, and the unit as its tailnet knows it. Owners only. */
export type TailscaleStatus = {
  available: boolean
  on: boolean
  state: string
  login_url?: string
  tailnet?: string
  name?: string
  addresses: string[]
  key_expiry?: string
  expired?: boolean
  health: string[]
  /** The tailnet gives this unit's name a certificate: MagicDNS and HTTPS are on for it. */
  https: boolean
  certificate: { on: boolean; in_use: boolean; not_after?: string; problem?: string }
  logs: boolean
  version?: string
}

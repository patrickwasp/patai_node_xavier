import type { Job, NetworkSetting } from "@/lib/types"

export const ROLES = [
  { value: "viewer", label: "Viewer", hint: "Can see how the unit is doing." },
  { value: "operator", label: "Operator", hint: "Can also start and stop workloads and restart the unit." },
  { value: "owner", label: "Owner", hint: "Can do everything, including managing people and tokens." },
]

export const SCOPES = [
  { value: "viewer", label: "Viewer", hint: "Can read the unit's status." },
  { value: "operator", label: "Operator", hint: "Can also start and stop workloads and restart the unit." },
]

export function roleLabel(role: string): string {
  if (role === "key") return "API key"
  return ROLES.find((item) => item.value === role)?.label ?? SCOPES.find((item) => item.value === role)?.label ?? role
}

// Apps by their titles, for jobs and the report, which name them. The Apps page has them from the manifests.
const APP_TITLES: Record<string, string> = {
  "ai-api": "AI API",
  chat: "Chat",
}

export function appTitle(app: string): string {
  return APP_TITLES[app] ?? app
}

/** The model in a job's target, app/model. */
function modelOf(target: string): string {
  return target.slice(target.indexOf("/") + 1)
}

export function jobTitle(job: Pick<Job, "kind" | "target">): string {
  switch (job.kind) {
    case "reboot":
      return "Restart the unit"
    case "workload-start":
      return `Turn on ${appTitle(job.target)}`
    case "workload-stop":
      return `Turn off ${appTitle(job.target)}`
    case "workload-fetch":
      return `Download ${appTitle(job.target)}`
    case "workload-fetch-cancel":
      return `Stop downloading ${appTitle(job.target)}`
    case "app-api-on":
      return `Turn on ${modelOf(job.target)}`
    case "app-api-off":
      return `Turn off ${modelOf(job.target)}`
    case "model-use":
      return `Use ${modelOf(job.target)}`
    case "model-fetch":
      return `Download ${modelOf(job.target)}`
    case "model-remove":
      return `Remove ${modelOf(job.target)}`
    case "disk-keep":
      return "Keep the disk from before"
    case "disk-erase":
      return "Erase the disk"
    case "update-check":
      return "Check for updates"
    case "update-install":
      return `Install ${job.target}`
    case "update-upload":
      return `Install ${job.target} from a file`
    case "rename":
      return `Rename the unit ${job.target}`
    case "shutdown":
      return "Shut the unit down"
    case "backup":
      return job.target === "scheduled" ? "Scheduled backup" : "Back up"
    case "backup-check":
      return "Check a backup"
    case "restore":
      return "Restore a backup"
  }
  return job.target ? `${job.kind} ${job.target}` : job.kind
}

export function jobState(state: Job["state"]): string {
  switch (state) {
    case "queued":
      return "Waiting"
    case "running":
      return "In progress"
    case "succeeded":
      return "Done"
    case "failed":
      return "Failed"
  }
  return state
}

export function installState(state: string): string {
  switch (state) {
    case "ok":
      return "Installed"
    case "failed":
      return "Failed"
    case "running":
      return "In progress"
  }
  return state
}

export function installSource(source: string): string {
  switch (source) {
    case "automatic":
      return "By itself"
    case "channel":
      return "From the channel"
    case "file":
      return "From a file"
  }
  return source
}

const AUDIT_ACTIONS: Record<string, string> = {
  claim: "Claimed the unit",
  login: "Logged in",
  recover: "Used a recovery code",
  "change-password": "Changed their password",
  "create-user": "Added a person",
  "delete-user": "Removed a person",
  "reset-password": "Reset a password",
  "reset-owner": "Reset an owner's password on the unit",
  "new-claim": "Made a new claim code on the unit",
  "create-token": "Created an API token",
  "delete-token": "Deleted an API token or key",
  "create-key": "Made an API key",
  "key-limits": "Changed an API key's limits",
  reboot: "Restarted the unit",
  "workload-start": "Turned an app on",
  "workload-stop": "Turned an app off",
  "workload-fetch": "Downloaded an app",
  "workload-fetch-cancel": "Stopped a download",
  "app-api-on": "Turned an API on",
  "app-api-off": "Turned an API off",
  "model-use": "Chose a model",
  "model-fetch": "Downloaded a model",
  "model-remove": "Removed a model",
  "model-add": "Added a model from an address",
  "model-upload": "Uploaded a model",
  "disk-keep": "Kept the disk from before",
  "disk-erase": "Erased the disk",
  "factory-reset": "Started a factory reset",
  "add-ssh-key": "Added an SSH key",
  "remove-ssh-key": "Removed an SSH key",
  "update-check": "Checked for updates",
  "update-install": "Installed an update",
  "update-upload": "Installed an update from a file",
  "update-file": "Sent an update file",
  "update-settings": "Changed the update channel",
  "network-try": "Tried a network change",
  "network-keep": "Kept a network change",
  "network-undo": "Undid a network change",
  "clock-set": "Set the clock",
  "power-mode": "Changed the power mode",
  rename: "Renamed the unit",
  shutdown: "Shut the unit down",
  "certificate-set": "Put a certificate in use",
  "certificate-remove": "Removed the certificate",
  "support-bundle": "Made a support bundle",
}

const AUDIT_REFUSED: Record<string, string> = {
  claim: "Tried to claim the unit",
  login: "Tried to log in",
  recover: "Tried a recovery code",
  "update-file": "Sent a file that wasn't a good update",
}

export function auditAction(action: string, refused: boolean): string {
  return (refused ? AUDIT_REFUSED[action] : undefined) ?? AUDIT_ACTIONS[action] ?? action
}

const POWER_MODES: Record<string, string> = {
  MAXN: "Maximum",
  MODE_10W: "10 W",
  MODE_15W: "15 W",
  MODE_30W_ALL: "30 W, all cores",
  MODE_30W_6CORE: "30 W, 6 cores",
  MODE_30W_4CORE: "30 W, 4 cores",
  MODE_30W_2CORE: "30 W, 2 cores",
  MODE_15W_DESKTOP: "15 W desktop",
}

export function powerLabel(name: string): string {
  return POWER_MODES[name] ?? name
}

/** A port's setting in a line: "DHCP", or "Static 192.168.1.20/24 via 192.168.1.1". */
export function networkSummary(setting: NetworkSetting): string {
  let words = setting.mode === "static" ? `Static ${setting.address}` : "DHCP"
  if (setting.mode === "static" && setting.gateway) words += ` via ${setting.gateway}`
  if (setting.dns.length > 0) words += `, DNS ${setting.dns.join(" ")}`
  return words
}

const LOG_SOURCES: Record<string, { label: string; hint: string }> = {
  all: { label: "Everything", hint: "The whole journal, less sign-ins." },
  kernel: { label: "Kernel", hint: "Hardware, drivers, memory and storage." },
  console: { label: "Console", hint: "The console, its helper, the report and the screen." },
  updates: { label: "Updates", hint: "Checking for and installing updates, and why one went back." },
  workloads: { label: "Apps", hint: "Turning apps on and off, downloads, the data disk and Docker." },
  network: { label: "Network and clock", hint: "Addresses, the firewall, DNS and time servers." },
  power: { label: "Power and temperature", hint: "The power mode, the fan and the thermal log." },
  "sign-ins": {
    label: "Sign-ins",
    hint: "Who logged in over SSH, and what they ran with sudo. Only owners see these, and they're never in a support bundle.",
  },
  output: {
    label: "App output",
    hint: "What the app printed since it was started. It can include what people sent it, so it's never in a support bundle.",
  },
}

export function logSource(id: string): { label: string; hint: string } {
  return LOG_SOURCES[id] ?? { label: id, hint: "" }
}

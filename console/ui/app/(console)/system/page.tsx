"use client"

import { useState } from "react"

import { CertificatePanel } from "@/components/certificate"
import { Choice } from "@/components/choice"
import { CopyButton, ErrorNote, Field, Muted, Notice, PageTitle, Panel, Row, tones } from "@/components/common"
import { DiskQuestion, diskTitle, needsAnswer } from "@/components/disk-question"
import { CurrentPassword, FormDialog } from "@/components/form-dialog"
import { useIdentity } from "@/components/gate"
import { NetworkPorts } from "@/components/network"
import { Restarting, useRestart } from "@/components/restart"
import { isOwner, useSession } from "@/components/session"
import { ClockPanel, PowerPanel, RenameButton, Renaming, ShuttingDown, useShutdown } from "@/components/system-settings"
import { Button } from "@/components/ui/button"
import { api, plainError } from "@/lib/api"
import { when } from "@/lib/format"
import { field, useLoad, usePoll } from "@/lib/hooks"
import { jobState, jobTitle } from "@/lib/labels"
import type { DiskStatus, Job } from "@/lib/types"

export default function SystemPage() {
  const session = useSession()
  const { identity } = useIdentity()
  const restart = useRestart()
  const shutdown = useShutdown()
  const [claimCode, setClaimCode] = useState<string | null>(null)
  const [renamingTo, setRenamingTo] = useState<string | null>(null)

  return (
    <>
      <PageTitle>System</PageTitle>
      {restart.restarting && (
        <Restarting slow={restart.slow}>
          {renamingTo ? (
            <Renaming name={renamingTo} />
          ) : (
            <>About a minute. This page carries on by itself.</>
          )}
        </Restarting>
      )}
      {shutdown.stage && <ShuttingDown stage={shutdown.stage} since={shutdown.since} />}
      {claimCode && <Resetting code={claimCode} />}
      <div className="grid items-start gap-4 md:grid-cols-2">
        <Panel
          title="This unit"
          action={
            isOwner(session) && (
              <RenameButton
                onRenamed={(name, job) => {
                  setRenamingTo(name)
                  void restart.follow(job)
                }}
              />
            )
          }
        >
          <dl className="grid gap-1.5">
            <Row label="Name">{identity.name}</Row>
            <Row label="Release">{identity.release}</Row>
          </dl>
          {identity.fingerprint && (
            <div className="grid gap-1">
              <p className="text-muted-foreground">Certificate fingerprint</p>
              <code className="font-mono text-xs select-all">{identity.fingerprint}</code>
            </div>
          )}
        </Panel>

        <PowerPanel
          onRestart={() => void restart.restart()}
          restartError={restart.error}
          onShutdown={() => void shutdown.shutdown()}
          shutdownError={shutdown.error}
        />
        <ClockPanel />
        <CertificatePanel />
        <NetworkPorts />
        <DataDisk />
        {isOwner(session) && <FactoryReset onStarted={setClaimCode} />}
      </div>
      <RecentJobs />
    </>
  )
}

const DISK_NOTES: Partial<Record<DiskStatus["state"], string>> = {
  ready: "In use for models, images and outputs.",
  absent: "No NVMe found. The console and updates still work, but apps can't run.",
  unmounted: "The disk is there but didn't mount, so apps can't run. Restart the unit. If that doesn't help, it needs service.",
}

function DataDisk() {
  const session = useSession()
  const disk = useLoad<DiskStatus>("/api/v1/disk")
  const status = disk.data
  return (
    <Panel id="disk" title={status && needsAnswer(status) ? diskTitle(status) : "Data disk"}>
      <ErrorNote>{disk.error}</ErrorNote>
      {status && !needsAnswer(status) && <Muted>{DISK_NOTES[status.state]}</Muted>}
      {status && needsAnswer(status) && isOwner(session) && <DiskQuestion disk={status} onDone={disk.reload} />}
      {status && needsAnswer(status) && !isOwner(session) && <Muted>Apps can&apos;t run until an owner answers.</Muted>}
    </Panel>
  )
}

const DISK_ON_RESET = [
  { value: "erase", label: "Erase it", hint: "Models, images and outputs are gone for good." },
  { value: "keep", label: "Leave it for the next owner", hint: "Nothing on it runs until they choose to keep or erase it." },
]

function FactoryReset({ onStarted }: { onStarted: (code: string) => void }) {
  const { identity } = useIdentity()
  const [asking, setAsking] = useState(false)
  return (
    <Panel title="Factory reset">
      <Muted>
        For a change of owner. Removes every person, token, SSH key and setting, gives the unit a new identity and
        restarts it. This can&apos;t be undone.
      </Muted>
      <Button variant="outline" onClick={() => setAsking(true)}>
        Reset this unit
      </Button>
      <FormDialog
        open={asking}
        onOpenChange={setAsking}
        title="Reset this unit?"
        description="Everyone loses access at once, including you, and the unit will need claiming again."
        submit="Reset this unit"
        destructive
        action={async (form) => {
          const result = await api<{ claim_code: string }>("/api/v1/factory-reset", {
            method: "POST",
            body: {
              name: field(form, "name"),
              keep_data: field(form, "disk") === "keep",
              current_password: field(form, "current_password"),
            },
          })
          setAsking(false)
          onStarted(result.claim_code)
        }}
      >
        <Choice name="disk" label="What happens to the NVMe" options={DISK_ON_RESET} defaultValue="erase" />
        <Field
          label={`Type this unit's name: ${identity.name}`}
          name="name"
          autoComplete="off"
          autoCapitalize="none"
          spellCheck={false}
          className="font-mono"
          required
        />
        <CurrentPassword />
      </FormDialog>
    </Panel>
  )
}

function Resetting({ code }: { code: string }) {
  return (
    <div role="status" className="fixed inset-0 z-50 grid place-items-center overflow-y-auto bg-background p-6">
      <div className="grid w-full max-w-md gap-4">
        <div className="grid gap-1">
          <p className="text-lg font-semibold">The unit is resetting</p>
          <Muted>
            It comes back in a couple of minutes with no owner, its factory name and a new certificate, so this page won&apos;t
            reconnect. Its screen shows its address and fingerprint.
          </Muted>
        </div>
        <Notice level="warn" title="Claim code for the next owner">
          <p>The unit&apos;s screen shows it too. Nothing else will show it to you again.</p>
          <code className="mt-2 block font-mono text-sm break-all select-all">{code}</code>
        </Notice>
        <CopyButton text={code} label="Copy code" />
      </div>
    </div>
  )
}

function RecentJobs() {
  const [jobs, setJobs] = useState<Job[] | null>(null)
  const [error, setError] = useState<string | null>(null)
  usePoll(async () => {
    try {
      setJobs(await api<Job[]>("/api/v1/jobs"))
      setError(null)
    } catch (failure) {
      setError(plainError(failure))
    }
  }, 10_000)

  return (
    <Panel title="Recent jobs">
      <ErrorNote>{error}</ErrorNote>
      {jobs?.length === 0 && <Muted>Nothing yet.</Muted>}
      <ul className="grid divide-y">
        {jobs?.map((job) => {
          const tone = job.state === "failed" ? tones.bad.text : job.state === "succeeded" ? "" : tones.warn.text
          return (
            <li key={job.id} className="grid gap-0.5 py-2 first:pt-0 last:pb-0">
              <div className="flex items-baseline justify-between gap-3">
                <span className="font-medium">{jobTitle(job)}</span>
                <span className={`shrink-0 text-sm ${tone}`}>{jobState(job.state)}</span>
              </div>
              <p className="text-xs text-muted-foreground">
                {job.actor}
                {when(job.created_at) && ` · ${when(job.created_at)}`}
              </p>
              {job.state === "failed" && job.result && <p className="font-mono text-xs">{job.result}</p>}
            </li>
          )
        })}
      </ul>
    </Panel>
  )
}

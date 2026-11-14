"use client"

import { useEffect, useState } from "react"
import Link from "next/link"

import { AddModel, AppControls, APIs, appState, JobResult, Models, partState, Reach, serviceTitle, Services, stacksOf, useApps } from "@/components/apps"
import { ErrorNote, Muted, PageTitle, Panel, Row, Spinner, tones } from "@/components/common"
import { Keys } from "@/components/keys"
import { Secrets, Settings } from "@/components/settings"
import { RequestLog, UsageTable } from "@/components/usage"
import type { App, WorkloadStatus } from "@/lib/types"
import { cn } from "@/lib/utils"

const GIB = 1024

/** What its services need, by their measured peaks, against the memory kept for apps, in the app's order. */
function MemoryRow({ app, info }: { app: App; info: WorkloadStatus }) {
  const memory = info.memory
  if (!memory) return null
  const stacks = stacksOf(app, info)
  const parts = Object.entries(memory.services).sort(
    ([a], [b]) => stacks.findIndex((stack) => stack.name === a) - stacks.findIndex((stack) => stack.name === b),
  )
  const total = parts.reduce((sum, [, mib]) => sum + mib, 0)
  const gib = (mib: number) => (mib / GIB).toFixed(1)
  return (
    <Row label="Memory">
      {`${gib(total)} of ${gib(memory.budget)} GiB: `}
      {parts
        .map(([service, mib]) => `${serviceTitle(app, service, stacks.find((stack) => stack.name === service))} ${gib(mib)}`)
        .join(", ")}
    </Row>
  )
}

/** One app: its state, how to reach it, its APIs and their keys and use, its services and their models. It's
 * /apps/app/?name=<app>. */
export default function AppPage() {
  const apps = useApps()
  const { info, infoError, error, job, busy, start, operate, owner, reload } = apps
  // From the address once the page is in the browser: it's built ahead, for every app.
  const [name, setName] = useState<string | null>(null)
  useEffect(() => setName(new URLSearchParams(window.location.search).get("name") ?? ""), [])
  const app = info?.apps.find((item) => item.name === name)
  const state = app && info ? appState(app, info) : null
  const stacks = app && info ? stacksOf(app, info) : []

  return (
    <>
      <Link href="/apps/" prefetch={false} className="text-sm text-muted-foreground underline underline-offset-4">
        Apps
      </Link>
      <PageTitle>{app?.title ?? ""}</PageTitle>
      <ErrorNote>{infoError}</ErrorNote>
      <ErrorNote>{error}</ErrorNote>
      {(info === null || name === null) && !infoError && (
        <div role="status" className="flex justify-center py-12">
          <Spinner />
          <span className="sr-only">Loading</span>
        </div>
      )}
      {info && name !== null && !app && <Muted>This unit has no app called {name || "that"}.</Muted>}
      {app && state && info && (
        <div className="grid items-start gap-4 md:grid-cols-2">
          <Panel title="State" action={<span className={cn("text-sm", tones[state.level].text)}>{state.text}</span>}>
            <Muted>{app.about}</Muted>
            <dl className="grid gap-1">
              {stacks.map((stack) => (
                <Row key={stack.name} label={`${serviceTitle(app, stack.name, stack)} software`}>
                  {partState(stack.image)}
                </Row>
              ))}
              {app.on && <MemoryRow app={app} info={info} />}
            </dl>
            <AppControls app={app} apps={apps} />
          </Panel>
          {app.on && (
            <Panel title="How to reach it">
              <Reach app={app} />
            </Panel>
          )}
          {app.apis.length > 0 && (
            <Panel title="APIs">
              <APIs app={app} apps={apps} />
            </Panel>
          )}
          {app.apis.length > 0 && owner && (
            <Panel title="Keys">
              <Keys app={app} />
            </Panel>
          )}
          <Panel title="Services">
            <Services app={app} info={info} />
          </Panel>
          {stacks
            .filter((stack) => stack.models.length > 0 || stack.formats.length > 0)
            .map((stack) => (
              <Panel
                key={stack.name}
                title={`Models: ${serviceTitle(app, stack.name, stack)}`}
                action={owner && stack.formats.length > 0 && <AddModel stack={stack} onAdded={reload} />}
              >
                <Models stack={stack} operate={operate} busy={busy} start={start} />
                {job?.kind.startsWith("model-") && job.target.startsWith(`${stack.name}/`) && job.state === "failed" && (
                  <JobResult job={job} />
                )}
              </Panel>
            ))}
          {stacks
            .filter((stack) => stack.settings.length > 0 || stack.secrets.length > 0)
            .map((stack) => (
              <Panel key={`settings-${stack.name}`} title={`Advanced: ${serviceTitle(app, stack.name, stack)}`}>
                {stack.settings.length > 0 && <Settings stack={stack} operate={operate} onSaved={reload} />}
                <Secrets stack={stack} owner={owner} onSaved={reload} />
              </Panel>
            ))}
          {app.apis.length > 0 && operate && (
            <Panel title="Usage" className="md:col-span-2">
              <UsageTable />
            </Panel>
          )}
          {app.apis.length > 0 && operate && (
            <Panel title="Requests" className="md:col-span-2">
              <RequestLog app={app} />
            </Panel>
          )}
        </div>
      )}
    </>
  )
}

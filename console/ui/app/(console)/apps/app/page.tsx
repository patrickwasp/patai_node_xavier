"use client"

import { useEffect, useState } from "react"
import Link from "next/link"

import { AddModel, AppControls, APIs, appState, JobResult, Models, partState, Reach, serviceTitle, Services, stacksOf, useApps } from "@/components/apps"
import { ErrorNote, Muted, PageTitle, Panel, Row, Spinner, tones } from "@/components/common"
import { Keys } from "@/components/keys"
import { Secrets, Settings } from "@/components/settings"
import { Tabs, TabsList, TabsPanel, TabsTab } from "@/components/ui/tabs"
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

/** The tab in the address, so a reload or a link opens it again. */
function keepTab(tab: string) {
  const query = new URLSearchParams(window.location.search)
  if (tab === "overview") query.delete("tab")
  else query.set("tab", tab)
  window.history.replaceState(null, "", `?${query}`)
}

/** One app, in tabs: its state, how to reach it, its APIs and services; its models; its APIs' keys; their use; and
 * its services' settings. It's /apps/app/?name=<app>&tab=<tab>. */
export default function AppPage() {
  const apps = useApps()
  const { info, infoError, error, job, busy, start, operate, owner, reload } = apps
  // From the address once the page is in the browser: it's built ahead, for every app.
  const [name, setName] = useState<string | null>(null)
  const [tab, setTab] = useState("overview")
  useEffect(() => {
    const query = new URLSearchParams(window.location.search)
    setName(query.get("name") ?? "")
    setTab(query.get("tab") ?? "overview")
  }, [])
  const app = info?.apps.find((item) => item.name === name)
  const state = app && info ? appState(app, info) : null
  const stacks = app && info ? stacksOf(app, info) : []
  const withModels = stacks.filter((stack) => stack.models.length > 0 || stack.formats.length > 0)
  const withSettings = stacks.filter((stack) => stack.settings.length > 0 || stack.secrets.some((secret) => !secret.once))
  const tabs = app
    ? [
        { value: "overview", title: "Overview" },
        ...(withModels.length > 0 ? [{ value: "models", title: "Models" }] : []),
        ...(app.apis.length > 0 && !app.key && owner ? [{ value: "keys", title: "Keys" }] : []),
        ...(app.apis.length > 0 && operate ? [{ value: "usage", title: "Usage" }] : []),
        ...(withSettings.length > 0 ? [{ value: "settings", title: "Settings" }] : []),
      ]
    : []
  const current = tabs.some((item) => item.value === tab) ? tab : "overview"

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
        <Tabs
          value={current}
          onValueChange={(value) => {
            setTab(String(value))
            keepTab(String(value))
          }}
        >
          <TabsList>
            {tabs.map((item) => (
              <TabsTab key={item.value} value={item.value}>
                {item.title}
              </TabsTab>
            ))}
          </TabsList>
          <TabsPanel value="overview" className="grid items-start gap-4 md:grid-cols-2">
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
              <Panel title={app.key ? "What it uses" : "APIs"}>
                <APIs app={app} apps={apps} />
              </Panel>
            )}
            <Panel title="Services">
              <Services app={app} info={info} />
            </Panel>
          </TabsPanel>
          <TabsPanel value="models" className="grid items-start gap-4 md:grid-cols-2">
            {withModels.map((stack) => (
              <Panel
                key={stack.name}
                title={serviceTitle(app, stack.name, stack)}
                action={owner && stack.formats.length > 0 && <AddModel stack={stack} onAdded={reload} />}
              >
                <Models stack={stack} operate={operate} busy={busy} start={start} />
                {job?.kind.startsWith("model-") && job.target.startsWith(`${stack.name}/`) && job.state === "failed" && (
                  <JobResult job={job} />
                )}
              </Panel>
            ))}
          </TabsPanel>
          <TabsPanel value="keys">
            <Panel title="Keys">
              <Keys app={app} />
            </Panel>
          </TabsPanel>
          <TabsPanel value="usage" className="grid gap-4">
            <Panel title="Usage">
              <UsageTable />
            </Panel>
            <Panel title="Requests">
              <RequestLog app={app} />
            </Panel>
          </TabsPanel>
          <TabsPanel value="settings" className="grid items-start gap-4 md:grid-cols-2">
            {withSettings.map((stack) => (
              <Panel key={stack.name} title={serviceTitle(app, stack.name, stack)}>
                {stack.settings.length > 0 && <Settings stack={stack} operate={operate} onSaved={reload} />}
                <Secrets stack={stack} owner={owner} onSaved={reload} />
              </Panel>
            ))}
          </TabsPanel>
        </Tabs>
      )}
    </>
  )
}

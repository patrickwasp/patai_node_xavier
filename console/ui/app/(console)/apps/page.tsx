"use client"

import Link from "next/link"

import { AppControls, appState, Reach, useApps } from "@/components/apps"
import { ErrorNote, Muted, Notice, PageTitle, Panel, Spinner, tones } from "@/components/common"
import { TodayLine } from "@/components/usage"
import { cn } from "@/lib/utils"

export default function AppsPage() {
  const apps = useApps()
  const { info, infoError, error, operate } = apps
  return (
    <>
      <PageTitle hint="An app starts the services it needs. Two apps can share one.">Apps</PageTitle>
      <ErrorNote>{infoError}</ErrorNote>
      <ErrorNote>{error}</ErrorNote>
      {info && info.disk !== "ready" && (
        <Notice level="warn" title="The data disk isn't ready">
          Apps can&apos;t download or run until it is.{" "}
          <Link href="/system/#disk" prefetch={false} className="underline underline-offset-4">
            See the System page
          </Link>
          .
        </Notice>
      )}
      {info === null && !infoError && (
        <div role="status" className="flex justify-center py-12">
          <Spinner />
          <span className="sr-only">Loading</span>
        </div>
      )}
      {info?.apps.length === 0 && <Muted>This unit has no apps.</Muted>}
      <div className="grid items-start gap-4 md:grid-cols-2">
        {info?.apps.map((app) => {
          const state = appState(app, info)
          const details = `/apps/app/?name=${encodeURIComponent(app.name)}`
          return (
            <Panel
              key={app.name}
              title={
                <Link href={details} prefetch={false} className="hover:underline hover:underline-offset-4">
                  {app.title}
                </Link>
              }
              action={<span className={cn("text-sm", tones[state.level].text)}>{state.text}</span>}
            >
              <Muted>{app.about}</Muted>
              <AppControls app={app} apps={apps} />
              {app.on && <Reach app={app} />}
              {app.on && app.apis.length > 0 && operate && <TodayLine />}
              <Link href={details} prefetch={false} className="text-sm text-muted-foreground underline underline-offset-4">
                Details
              </Link>
            </Panel>
          )
        })}
      </div>
      {!operate && <Muted>Your role can look at apps but not turn them on or off.</Muted>}
    </>
  )
}

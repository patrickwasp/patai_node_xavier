"use client"

import Link from "next/link"

import { AppControls, appState, Reach, useApps } from "@/components/apps"
import { ErrorNote, Muted, Notice, PageTitle, Panel, Spinner, tones } from "@/components/common"
import { OutsideModels } from "@/components/outside"
import { TodayLine } from "@/components/usage"
import { cn } from "@/lib/utils"

export default function AppsPage() {
  const apps = useApps()
  const { info, infoError, error, operate, owner } = apps
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
              <div className="grid gap-1">
                <Muted>{app.about}</Muted>
                <p className="text-xs text-muted-foreground">Version {app.version}</p>
              </div>
              <AppControls app={app} apps={apps} />
              {app.on && <Reach app={app} />}
              {app.on && app.apis.length > 0 && operate && <TodayLine />}
              <div className="flex gap-4 text-sm text-muted-foreground">
                <Link href={details} prefetch={false} className="underline underline-offset-4">
                  Details
                </Link>
                {app.apis.length > 0 && !app.key && owner && (
                  <Link href={`${details}&tab=keys`} prefetch={false} className="underline underline-offset-4">
                    Keys
                  </Link>
                )}
                {app.apis.length > 0 && !app.key && operate && (
                  <Link href={`${details}&tab=usage`} prefetch={false} className="underline underline-offset-4">
                    Usage
                  </Link>
                )}
              </div>
            </Panel>
          )
        })}
      </div>
      {owner && info && <OutsideModels apps={info.apps} />}
      {!operate && <Muted>Your role can look at apps but not turn them on or off.</Muted>}
    </>
  )
}

"use client"

import type { ReactNode } from "react"
import Link from "next/link"
import { useRouter } from "next/navigation"
import { Activity, ArrowDownToLine, Boxes, CircleUser, KeyRound, LogOut, ScrollText, Server } from "lucide-react"

import { useIdentity } from "@/components/gate"
import { NetworkTrialBanner } from "@/components/network"
import { canOperate, isOwner, useSession } from "@/components/session"
import { Button } from "@/components/ui/button"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuGroup,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import { api } from "@/lib/api"
import { usePath } from "@/lib/hooks"
import { roleLabel } from "@/lib/labels"
import { cn } from "@/lib/utils"

const PAGES = [
  { href: "/", label: "Status", icon: Activity, need: "viewer" },
  { href: "/apps/", label: "Apps", icon: Boxes, need: "viewer" },
  { href: "/updates/", label: "Updates", icon: ArrowDownToLine, need: "viewer" },
  { href: "/system/", label: "System", icon: Server, need: "viewer" },
  { href: "/access/", label: "Access", icon: KeyRound, need: "owner" },
  // The audit log is a view of it, at /audit/ too.
  { href: "/logs/", label: "Logs", icon: ScrollText, need: "operator", also: "/audit" },
]

function AccountMenu() {
  const router = useRouter()
  const session = useSession()

  async function logOut() {
    await api("/api/v1/session", { method: "DELETE" }).catch(() => {})
    window.location.assign("/login/")
  }

  return (
    <DropdownMenu>
      <DropdownMenuTrigger render={<Button variant="ghost" size="sm" className="ml-auto max-w-40" />}>
        <CircleUser aria-hidden />
        <span className="truncate">{session.name}</span>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end" className="w-52">
        <DropdownMenuGroup>
          <DropdownMenuLabel>{roleLabel(session.role)}</DropdownMenuLabel>
          <DropdownMenuItem onClick={() => router.push("/access/#password")}>
            <KeyRound aria-hidden />
            Change my password
          </DropdownMenuItem>
        </DropdownMenuGroup>
        <DropdownMenuSeparator />
        <DropdownMenuItem onClick={logOut}>
          <LogOut aria-hidden />
          Log out
        </DropdownMenuItem>
      </DropdownMenuContent>
    </DropdownMenu>
  )
}

export function Shell({ children }: { children: ReactNode }) {
  const path = usePath()
  const session = useSession()
  const { identity } = useIdentity()
  const pages = PAGES.filter(
    (page) => page.need === "viewer" || (page.need === "operator" ? canOperate(session) : isOwner(session))
  )
  const isCurrent = (href: string) =>
    href === "/"
      ? path === "/"
      : path === href.slice(0, -1) || PAGES.some((page) => page.href === href && page.also === path)

  return (
    <div className="min-h-svh pb-[calc(4.5rem+env(safe-area-inset-bottom))] md:pb-8">
      <header className="sticky top-0 z-30 border-b bg-background pt-[env(safe-area-inset-top)]">
        <div className="mx-auto flex h-14 max-w-4xl items-center gap-6 px-4">
          <Link href="/" className="min-w-0 truncate font-mono text-sm font-medium">
            {identity.name}
          </Link>
          <nav aria-label="Pages" className="hidden items-center gap-1 md:flex">
            {pages.map((page) => (
              <Link
                key={page.href}
                href={page.href}
                prefetch={false}
                aria-current={isCurrent(page.href) ? "page" : undefined}
                className={cn(
                  "rounded-md px-3 py-2 text-sm text-muted-foreground hover:bg-muted hover:text-foreground",
                  isCurrent(page.href) && "bg-muted font-medium text-foreground"
                )}
              >
                {page.label}
              </Link>
            ))}
          </nav>
          <AccountMenu />
        </div>
      </header>

      <main className="mx-auto grid max-w-4xl gap-4 px-4 py-4 md:py-6">
        {isOwner(session) && <NetworkTrialBanner />}
        {children}
      </main>

      <nav
        aria-label="Pages"
        className="fixed inset-x-0 bottom-0 z-30 border-t bg-card pb-[env(safe-area-inset-bottom)] md:hidden"
      >
        <ul className="flex">
          {pages.map((page) => (
            <li key={page.href} className="min-w-0 flex-1">
              <Link
                href={page.href}
                prefetch={false}
                aria-current={isCurrent(page.href) ? "page" : undefined}
                className={cn(
                  "relative flex h-14 flex-col items-center justify-center gap-0.5 text-[0.6875rem] text-muted-foreground",
                  isCurrent(page.href) && "font-medium text-foreground"
                )}
              >
                {isCurrent(page.href) && (
                  <span aria-hidden className="absolute inset-x-3 top-0 h-0.5 rounded-b bg-primary" />
                )}
                <page.icon aria-hidden className="size-5" />
                {page.label}
              </Link>
            </li>
          ))}
        </ul>
      </nav>
    </div>
  )
}

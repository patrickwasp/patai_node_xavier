import type { ReactNode } from "react"

import { SessionProvider } from "@/components/session"
import { Shell } from "@/components/shell"

export default function ConsoleLayout({ children }: { children: ReactNode }) {
  return (
    <SessionProvider>
      <Shell>{children}</Shell>
    </SessionProvider>
  )
}

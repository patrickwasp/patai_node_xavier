import type { ReactNode } from "react"
import type { Metadata, Viewport } from "next"

import "./globals.css"
import { Gate } from "@/components/gate"

export const metadata: Metadata = {
  title: "Unit console",
  robots: { index: false, follow: false },
}

export const viewport: Viewport = {
  width: "device-width",
  initialScale: 1,
  viewportFit: "cover",
  themeColor: [
    { media: "(prefers-color-scheme: light)", color: "#f5f6f8" },
    { media: "(prefers-color-scheme: dark)", color: "#10141a" },
  ],
}

export default function RootLayout({ children }: { children: ReactNode }) {
  return (
    <html lang="en">
      <body>
        <Gate>{children}</Gate>
        <noscript>This console needs JavaScript.</noscript>
      </body>
    </html>
  )
}

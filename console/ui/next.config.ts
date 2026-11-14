import type { NextConfig } from "next"

// A static export embedded in patai-api and served from the same origin as the API. Nothing here may
// need a server, and nothing may load from outside the unit.
const config: NextConfig = {
  output: "export",
  images: { unoptimized: true },
  // Each page becomes <page>/index.html. Without this Next writes <page>.html beside a <page>/ folder of
  // route data, and the Go file server finds the folder first and lists it.
  trailingSlash: true,
  agentRules: false,
  devIndicators: false,
}

export default config

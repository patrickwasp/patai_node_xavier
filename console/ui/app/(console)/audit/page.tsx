"use client"

import { LogsView } from "@/components/logs"

/** The audit log is a view of the Logs page now. This address still opens it. */
export default function AuditPage() {
  return <LogsView initial="audit" />
}

"use client"

import { Tabs as TabsPrimitive } from "@base-ui/react/tabs"

import { cn } from "@/lib/utils"

function Tabs({ className, ...props }: TabsPrimitive.Root.Props) {
  return <TabsPrimitive.Root data-slot="tabs" className={cn("grid gap-4", className)} {...props} />
}

/** A row of tabs styled as the console's own navigation, scrolling sideways on a narrow screen rather than the page. */
function TabsList({ className, ...props }: TabsPrimitive.List.Props) {
  return (
    <TabsPrimitive.List
      data-slot="tabs-list"
      className={cn("-mx-1 flex gap-1 overflow-x-auto border-b px-1 pb-2 [scrollbar-width:none]", className)}
      {...props}
    />
  )
}

function TabsTab({ className, ...props }: TabsPrimitive.Tab.Props) {
  return (
    <TabsPrimitive.Tab
      data-slot="tabs-tab"
      className={cn(
        "shrink-0 rounded-md px-3 py-2 text-sm whitespace-nowrap text-muted-foreground outline-none hover:bg-muted hover:text-foreground focus-visible:ring-[3px] focus-visible:ring-ring/50 data-active:bg-muted data-active:font-medium data-active:text-foreground",
        className
      )}
      {...props}
    />
  )
}

function TabsPanel({ className, ...props }: TabsPrimitive.Panel.Props) {
  return <TabsPrimitive.Panel data-slot="tabs-panel" className={cn("outline-none", className)} {...props} />
}

export { Tabs, TabsList, TabsTab, TabsPanel }

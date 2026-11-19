"use client"

import { useEffect, useState, type ReactNode } from "react"
import {
  ArrowDown,
  ArrowUp,
  ChevronLeft,
  ChevronRight,
  ChevronsUpDown,
  Columns3,
  ListFilter,
  MoreHorizontal,
  Search,
  X,
} from "lucide-react"
import {
  columnFacetingFeature,
  columnFilteringFeature,
  columnVisibilityFeature,
  createColumnHelper,
  createFacetedRowModel,
  createFacetedUniqueValues,
  createFilteredRowModel,
  createPaginatedRowModel,
  createSortedRowModel,
  filterFn_includesString,
  functionalUpdate,
  globalFilteringFeature,
  metaHelper,
  rowPaginationFeature,
  rowSortingFeature,
  sortFn_alphanumeric,
  sortFn_basic,
  sortFn_datetime,
  sortFn_text,
  tableFeatures,
  useTable,
  type Column,
  type ColumnDef,
  type ColumnFiltersState,
  type ColumnVisibilityState,
  type FilterFn,
  type SortingState,
  type Updater,
} from "@tanstack/react-table"

import { Muted } from "@/components/common"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import {
  DropdownMenu,
  DropdownMenuCheckboxItem,
  DropdownMenuContent,
  DropdownMenuGroup,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import { Input } from "@/components/ui/input"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { cn } from "@/lib/utils"

/** What a column says about itself to the table around it. */
type Meta = {
  /** A number: right-aligned, in tabular figures. */
  numeric?: boolean
  /** Filtered from a menu of its values. label puts a value in words. */
  facet?: { label?: (value: string) => string }
  /** Classes for its cells, such as letting long text wrap. */
  className?: string
  /** Its name in words, for the Columns menu, when its header isn't text. */
  title?: string
}

const features = tableFeatures({
  columnFilteringFeature,
  globalFilteringFeature,
  filteredRowModel: createFilteredRowModel(),
  filterFns: { includesString: filterFn_includesString },
  columnFacetingFeature,
  facetedRowModel: createFacetedRowModel(),
  facetedUniqueValues: createFacetedUniqueValues(),
  rowSortingFeature,
  sortedRowModel: createSortedRowModel(),
  sortFns: { alphanumeric: sortFn_alphanumeric, text: sortFn_text, datetime: sortFn_datetime, basic: sortFn_basic },
  columnVisibilityFeature,
  rowPaginationFeature,
  paginatedRowModel: createPaginatedRowModel(),
  columnMeta: metaHelper<Meta>(),
})

type Features = typeof features

/** Builds a table's columns, typed by its rows. */
export function columnsOf<T extends object>() {
  return createColumnHelper<Features, T>()
}

/** A faceted column's filter: the row's value is one of those chosen. */
const oneOf: FilterFn<Features, any> = (row, id, chosen: string[]) => chosen.includes(String(row.getValue(id)))
oneOf.autoRemove = (value) => !Array.isArray(value) || value.length === 0

function title<T extends object>(column: Column<Features, T, unknown>): string {
  const header = column.columnDef.header
  return typeof header === "string" ? header : (column.columnDef.meta?.title ?? column.id)
}

/** What a viewer has hidden of a table's columns, kept in this browser only. */
function storedColumns(key: string): ColumnVisibilityState {
  try {
    const value = JSON.parse(localStorage.getItem(key) ?? "{}")
    return value && typeof value === "object" ? value : {}
  } catch {
    return {}
  }
}

export type DataTableProps<T extends object> = {
  /** Names the table, to keep each viewer's choice of columns. */
  id: string
  /** The table's accessible name. */
  label: string
  data: T[]
  /** Each column has its own value type. */
  columns: ColumnDef<Features, T, any>[]
  rowId: (row: T) => string
  /** Shown instead of the table when it has no rows at all. */
  empty: ReactNode
  /** Columns hidden until a viewer shows them. */
  hidden?: string[]
  sort?: SortingState
  /** Filters set to begin with, such as a facet's value chosen. */
  filters?: ColumnFiltersState
  pageSize?: number
  /** Beside the row count, such as a button that loads older rows. */
  more?: ReactNode
}

/** Rows with a filter, menus of each faceted column's values, sorting, a choice of columns and pages. */
export function DataTable<T extends object>({
  id,
  label,
  data,
  columns,
  rowId,
  empty,
  hidden = [],
  sort = [],
  filters = [],
  pageSize = 25,
  more,
}: DataTableProps<T>) {
  const key = `patai.columns.${id}`
  const [visibility, setVisibility] = useState<ColumnVisibilityState>(() => ({
    ...Object.fromEntries(hidden.map((column) => [column, false])),
    ...storedColumns(key),
  }))
  const table = useTable({
    features,
    columns,
    data,
    getRowId: rowId,
    defaultColumn: { filterFn: oneOf },
    globalFilterFn: "includesString",
    // A sorted column turns round on a click, rather than going back to the order rows came in.
    enableSortingRemoval: false,
    // Loading more rows, or a poll's fresh ones, keeps the page in view.
    autoResetPageIndex: false,
    initialState: { sorting: sort, columnFilters: filters, pagination: { pageIndex: 0, pageSize } },
    state: { columnVisibility: visibility },
    onColumnVisibilityChange: (updater: Updater<ColumnVisibilityState>) =>
      setVisibility((old) => {
        const next = functionalUpdate(updater, old)
        try {
          localStorage.setItem(key, JSON.stringify(next))
        } catch {}
        return next
      }),
  })

  const pages = table.getPageCount()
  const page = table.state.pagination.pageIndex
  useEffect(() => {
    if (page > 0 && page >= pages) table.setPageIndex(Math.max(0, pages - 1))
  }, [page, pages, table])

  if (data.length === 0)
    return (
      <div className="flex flex-wrap items-center gap-3">
        <Muted>{empty}</Muted>
        {more}
      </div>
    )

  const first = () => table.setPageIndex(0)
  const filtered = table.state.columnFilters.length > 0 || Boolean(table.state.globalFilter)
  const shown = table.getPrePaginatedRowModel().rows.length
  const columnsShown = table.getVisibleLeafColumns().length
  const leaves = table.getAllLeafColumns()
  const facets = leaves.filter((column) => column.columnDef.meta?.facet && column.getIsVisible())
  // A row's first column names it, and its actions are how it's changed: neither can be hidden.
  const hideable = leaves.filter((column, index) => index > 0 && column.id !== "actions" && column.getCanHide())

  return (
    <div className="grid min-w-0 gap-3">
      <div className="flex flex-wrap items-center gap-2">
        <div className="relative w-full sm:w-56">
          <Search
            aria-hidden
            className="pointer-events-none absolute top-1/2 left-2.5 size-4 -translate-y-1/2 text-muted-foreground"
          />
          <Input
            type="search"
            aria-label={`Filter ${label.toLowerCase()}`}
            placeholder="Filter"
            value={String(table.state.globalFilter ?? "")}
            onChange={(event) => {
              table.setGlobalFilter(event.target.value)
              first()
            }}
            className="h-9 pl-8"
          />
        </div>
        {facets.map((column) => (
          <Facet key={column.id} column={column} onChange={first} />
        ))}
        {filtered && (
          <Button
            size="sm"
            variant="ghost"
            onClick={() => {
              // Every filter off, those it began with too.
              table.setColumnFilters([])
              table.setGlobalFilter("")
              first()
            }}
          >
            <X aria-hidden />
            Reset
          </Button>
        )}
        {hideable.length > 0 && (
          <DropdownMenu>
            <DropdownMenuTrigger render={<Button size="sm" variant="outline" className="ml-auto" />}>
              <Columns3 aria-hidden />
              Columns
            </DropdownMenuTrigger>
            <DropdownMenuContent align="end" className="w-auto min-w-44">
              <DropdownMenuGroup>
                <DropdownMenuLabel>Show</DropdownMenuLabel>
                {hideable.map((column) => (
                  <DropdownMenuCheckboxItem
                    key={column.id}
                    checked={column.getIsVisible()}
                    onCheckedChange={(on) => column.toggleVisibility(on)}
                  >
                    {title(column)}
                  </DropdownMenuCheckboxItem>
                ))}
              </DropdownMenuGroup>
            </DropdownMenuContent>
          </DropdownMenu>
        )}
      </div>

      <Table aria-label={label}>
        <TableHeader>
          {table.getHeaderGroups().map((group) => (
            <TableRow key={group.id} className="hover:bg-transparent">
              {group.headers.map((header) => {
                const column = header.column
                const meta = column.columnDef.meta
                const sorted = column.getIsSorted()
                return (
                  <TableHead
                    key={header.id}
                    aria-sort={sorted === "asc" ? "ascending" : sorted === "desc" ? "descending" : undefined}
                    className={cn(meta?.numeric && "text-right")}
                  >
                    {header.isPlaceholder ? null : column.getCanSort() ? (
                      <button
                        type="button"
                        onClick={(event) => {
                          column.getToggleSortingHandler()?.(event)
                          first()
                        }}
                        className={cn(
                          "-mx-2 inline-flex h-8 items-center gap-1 rounded-md px-2 outline-none hover:bg-muted hover:text-foreground focus-visible:ring-3 focus-visible:ring-ring/50",
                          meta?.numeric && "flex-row-reverse",
                          sorted && "text-foreground"
                        )}
                      >
                        <table.FlexRender header={header} />
                        {sorted === "asc" ? (
                          <ArrowUp aria-hidden className="size-3.5" />
                        ) : sorted === "desc" ? (
                          <ArrowDown aria-hidden className="size-3.5" />
                        ) : (
                          <ChevronsUpDown aria-hidden className="size-3.5 opacity-40" />
                        )}
                      </button>
                    ) : (
                      <table.FlexRender header={header} />
                    )}
                  </TableHead>
                )
              })}
            </TableRow>
          ))}
        </TableHeader>
        <TableBody>
          {table.getRowModel().rows.map((row) => (
            <TableRow key={row.id}>
              {row.getVisibleCells().map((cell) => {
                const meta = cell.column.columnDef.meta
                return (
                  <TableCell key={cell.id} className={cn(meta?.numeric && "text-right tabular-nums", meta?.className)}>
                    <table.FlexRender cell={cell} />
                  </TableCell>
                )
              })}
            </TableRow>
          ))}
          {shown === 0 && (
            <TableRow className="hover:bg-transparent">
              <TableCell colSpan={columnsShown} className="h-16 text-center text-muted-foreground">
                No matches.
              </TableCell>
            </TableRow>
          )}
        </TableBody>
      </Table>

      <div className="flex flex-wrap items-center gap-x-3 gap-y-2 text-sm text-muted-foreground">
        <span className="tabular-nums">
          {filtered ? `${shown.toLocaleString()} of ${data.length.toLocaleString()}` : data.length.toLocaleString()}{" "}
          {(filtered ? data.length : shown) === 1 ? "row" : "rows"}
        </span>
        {more}
        {pages > 1 && (
          <div className="ml-auto flex items-center gap-1">
            <span className="mr-2 tabular-nums">
              Page {page + 1} of {pages}
            </span>
            <Button
              size="icon-sm"
              variant="outline"
              aria-label="Previous page"
              disabled={!table.getCanPreviousPage()}
              onClick={() => table.previousPage()}
            >
              <ChevronLeft aria-hidden />
            </Button>
            <Button
              size="icon-sm"
              variant="outline"
              aria-label="Next page"
              disabled={!table.getCanNextPage()}
              onClick={() => table.nextPage()}
            >
              <ChevronRight aria-hidden />
            </Button>
          </div>
        )}
      </div>
    </div>
  )
}

/** A menu of a column's values, each with how many rows have it, to show only those chosen. */
function Facet<T extends object>({ column, onChange }: { column: Column<Features, T, unknown>; onChange: () => void }) {
  const label = column.columnDef.meta?.facet?.label ?? ((value: string) => value)
  const chosen = (column.getFilterValue() as string[] | undefined) ?? []
  const counts = [...column.getFacetedUniqueValues().entries()]
    .map(([value, count]) => [String(value), count] as const)
    .sort(([a], [b]) => label(a).localeCompare(label(b)))
  const choose = (next: string[]) => {
    column.setFilterValue(next.length > 0 ? next : undefined)
    onChange()
  }
  return (
    <DropdownMenu>
      <DropdownMenuTrigger render={<Button size="sm" variant="outline" className="border-dashed" />}>
        <ListFilter aria-hidden />
        {title(column)}
        {chosen.length > 0 && (
          <Badge variant="secondary" className="rounded-sm px-1 font-normal">
            {chosen.length === 1 ? label(chosen[0]) : `${chosen.length} chosen`}
          </Badge>
        )}
      </DropdownMenuTrigger>
      <DropdownMenuContent className="w-auto min-w-48">
        <DropdownMenuGroup>
          <DropdownMenuLabel>{title(column)}</DropdownMenuLabel>
          {counts.map(([value, count]) => (
            <DropdownMenuCheckboxItem
              key={value}
              checked={chosen.includes(value)}
              onCheckedChange={(on) => choose(on ? [...chosen, value] : chosen.filter((other) => other !== value))}
            >
              <span className="flex-1">{label(value)}</span>
              <span className="ml-6 font-mono text-xs text-muted-foreground tabular-nums">{count}</span>
            </DropdownMenuCheckboxItem>
          ))}
        </DropdownMenuGroup>
        {chosen.length > 0 && (
          <>
            <DropdownMenuSeparator />
            <DropdownMenuItem onClick={() => choose([])}>Clear</DropdownMenuItem>
          </>
        )}
      </DropdownMenuContent>
    </DropdownMenu>
  )
}

/** A row's actions, in a menu behind ⋯. */
export function RowActions({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="flex justify-end">
      <DropdownMenu>
        <DropdownMenuTrigger render={<Button size="icon-sm" variant="ghost" aria-label={label} />}>
          <MoreHorizontal aria-hidden />
        </DropdownMenuTrigger>
        <DropdownMenuContent align="end" className="w-auto min-w-40">
          {children}
        </DropdownMenuContent>
      </DropdownMenu>
    </div>
  )
}

/** The header of a column of row actions, for screen readers. */
export function ActionsHeader() {
  return <span className="sr-only">Actions</span>
}

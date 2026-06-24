import { useEffect, useRef } from "react"
import { ChevronLeft, ChevronRight, RotateCcw, ScrollText } from "lucide-react"
import { useTranslation } from "react-i18next"

import { Button } from "@/components/ui/button"
import { Card } from "@/components/ui/card"
import { PageHeader } from "@/components/ui/page-header"
import { RequestLogTable } from "@/features/dashboard/components/request-log-table"
import { useDashboardData } from "@/features/dashboard/hooks/use-dashboard-data"
import type { RequestSortKey, SortDirection } from "@/features/dashboard/api"

// Fixed page size for the logs view: 50 per page, prev/next only.
const PAGE_SIZE = 50

export function LogsPage({
  onUnauthorized,
  onSelectDetail,
}: {
  onUnauthorized: () => void
  onSelectDetail: (requestId: string) => void
}) {
  const {
    loading,
    refreshing,
    refreshDashboard,
    requests,
    total,
    offset,
    sortBy,
    sortOrder,
  } = useDashboardData(onUnauthorized)

  const { t } = useTranslation()

  // Load page 1 on mount.
  const loadedRef = useRef(false)
  useEffect(() => {
    if (loadedRef.current) return
    loadedRef.current = true
    void refreshDashboard({ limit: PAGE_SIZE, offset: 0, filters: {} })
  }, [refreshDashboard])

  const handleSortChange = (newSortBy: RequestSortKey, newSortOrder: SortDirection) => {
    void refreshDashboard({ limit: PAGE_SIZE, offset: 0, filters: {}, sortBy: newSortBy, sortOrder: newSortOrder })
  }

  const currentOffset = offset ?? 0
  const canPrev = currentOffset > 0
  const canNext = currentOffset + PAGE_SIZE < total

  const goPrev = () => {
    if (!canPrev) return
    void refreshDashboard({ limit: PAGE_SIZE, offset: Math.max(0, currentOffset - PAGE_SIZE), filters: {} })
  }
  const goNext = () => {
    if (!canNext) return
    void refreshDashboard({ limit: PAGE_SIZE, offset: currentOffset + PAGE_SIZE, filters: {} })
  }

  const pageStart = total === 0 ? 0 : currentOffset + 1
  const pageEnd = Math.min(currentOffset + PAGE_SIZE, total)

  return (
    <div className="flex flex-col gap-4">
      {/* Header */}
      <PageHeader
        icon={ScrollText}
        title={t("logs.title")}
        description={t("logs.description")}
        actions={
          <Button
            type="button"
            size="sm"
            disabled={refreshing}
            onClick={() => {
              void refreshDashboard({ limit: PAGE_SIZE, offset: currentOffset, filters: {} })
            }}
          >
            <RotateCcw data-icon="inline-start" className={`h-4 w-4 ${refreshing ? "animate-spin" : ""}`} />
            {t("common.refreshData")}
          </Button>
        }
      />

      {/* Table Card */}
      <Card>
        <RequestLogTable
          loading={loading}
          refreshing={refreshing}
          requests={requests}
          selectedId={null}
          sortBy={sortBy}
          sortOrder={sortOrder}
          onSort={handleSortChange}
          onSelect={(requestId) => onSelectDetail(requestId)}
          onClearFilters={() => {}}
          onApplyRouteFilter={() => {}}
          onApplyModelFilter={() => {}}
          onApplySourceTypeFilter={() => {}}
        />

        {/* Prev / Next only */}
        <div className="flex items-center justify-between border-t border-border/60 px-4 py-3">
          <p className="text-xs text-muted-foreground">
            {pageStart}–{pageEnd} / {total}
          </p>
          <div className="flex items-center gap-1">
            <Button
              variant="ghost"
              size="sm"
              className="h-8 gap-1 px-2"
              disabled={!canPrev || refreshing}
              onClick={goPrev}
            >
              <ChevronLeft className="size-4" />
              <span className="hidden sm:inline">{t("logs.prevPage")}</span>
            </Button>
            <Button
              variant="ghost"
              size="sm"
              className="h-8 gap-1 px-2"
              disabled={!canNext || refreshing}
              onClick={goNext}
            >
              <span className="hidden sm:inline">{t("logs.nextPage")}</span>
              <ChevronRight className="size-4" />
            </Button>
          </div>
        </div>
      </Card>
    </div>
  )
}

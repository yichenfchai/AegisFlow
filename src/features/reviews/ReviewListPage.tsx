import { useQuery } from '@tanstack/react-query'
import { Link, useNavigate } from 'react-router'
import { api } from '@/api'
import { DataTable, PageHeader, Button } from '@/components/ui/DataTable'
import { Segmented } from '@/components/ui/Badge'
import { StatusBadge, StatCard } from '@/components/ui/Badge'
import { useState } from 'react'
import type { ReviewRequest } from '@/api/types'
import { fmtRel } from '@/api/format'

const STATUS_TONE: Record<ReviewRequest['status'], 'success' | 'error' | 'running' | 'pending' | 'neutral'> = {
  pending: 'pending',
  reviewing: 'running',
  awaiting_approval: 'pending',
  approved: 'success',
  merged: 'success',
  rejected: 'error',
}
const STATUS_TEXT: Record<ReviewRequest['status'], string> = {
  pending: '待审查',
  reviewing: '审查中',
  awaiting_approval: '待批准',
  approved: '已批准',
  merged: '已合并',
  rejected: '已驳回',
}

type Filter = 'all' | ReviewRequest['status']

export function ReviewListPage() {
  const nav = useNavigate()
  const { data: reviews = [] } = useQuery({ queryKey: ['reviews'], queryFn: api.reviews })
  const [filter, setFilter] = useState<Filter>('all')
  const filtered = filter === 'all' ? reviews : reviews.filter((r) => r.status === filter)

  return (
    <>
      <PageHeader
        title="审查中心"
        subtitle="AI + 人工联合审查 · 高置信规则辅助裁决"
        actions={
          <Link to="/reviews/new">
            <Button icon="add">提交审查</Button>
          </Link>
        }
      />
      <div className="grid grid-cols-4 gap-gutter mb-space-xl">
        <StatCard label="件需裁决" value={reviews.filter((r) => r.status === 'awaiting_approval' || r.status === 'pending').length} accent="warning" />
        <StatCard label="本周已合并" value={1} trend="33%" trendDir="up" />
        <StatCard label="平均审查耗时" value="12m 4s" sub="↑ 33% 环比提速" />
        <StatCard label="AI 意见采纳率" value="79%" accent="ai" sub="人工修改占 21%" />
      </div>

      <div className="mb-space-lg">
        <Segmented
          value={filter}
          onChange={setFilter}
          options={[
            { value: 'all' as const, label: '全部' },
            { value: 'pending' as const, label: '待审查' },
            { value: 'reviewing' as const, label: '审查中' },
            { value: 'awaiting_approval' as const, label: '待批准' },
            { value: 'merged' as const, label: '已合并' },
            { value: 'rejected' as const, label: '已驳回' },
          ]}
        />
      </div>

      <DataTable
        rows={filtered}
        onRowClick={(r) => nav(`/reviews/${r.id}`)}
        columns={[
          {
            key: 'number',
            header: '编号',
            render: (r) => <span className="font-mono text-primary">{r.number}</span>,
          },
          {
            key: 'title',
            header: '标题',
            render: (r) => (
              <div className="max-w-[280px]">
                <div className="truncate">{r.title}</div>
                <div className="text-label-sm text-outline">
                  {r.sourceBranch} → {r.baseBranch} · {r.author}
                </div>
              </div>
            ),
          },
          { key: 'project', header: '项目', render: (r) => r.projectName },
          {
            key: 'delta',
            header: '变更量',
            render: (r) => (
              <span className="font-mono text-body-sm">
                <span className="text-secondary">+{r.additions}</span>{' '}
                <span className="text-error">−{r.deletions}</span>
              </span>
            ),
          },
          {
            key: 'status',
            header: '状态',
            render: (r) => <StatusBadge tone={STATUS_TONE[r.status]}>{STATUS_TEXT[r.status]}</StatusBadge>,
          },
          { key: 'time', header: '更新时间', render: (r) => fmtRel(r.createdAt) },
        ]}
      />
    </>
  )
}

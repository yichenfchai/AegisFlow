import { useQuery } from '@tanstack/react-query'
import { Link, useNavigate } from 'react-router'
import { api } from '@/api'
import { DataTable, PageHeader } from '@/components/ui/DataTable'
import { StatusBadge, StatCard } from '@/components/ui/Badge'
import type { Build } from '@/api/types'
import { fmtRel } from '@/api/format'

const TRIGGER_TEXT: Record<Build['triggerSource'], string> = {
  push: 'Push',
  cron: 'Cron',
  manual: 'Manual',
  rr_merge: '审查 RR',
  retry: '重试',
  defense: '防御',
}

const STATUS_TONE: Record<Build['status'], 'success' | 'error' | 'running' | 'pending'> = {
  success: 'success',
  failed: 'error',
  running: 'running',
  queued: 'running',
  blocked: 'error',
  awaiting_approval: 'pending',
  cancelled: 'pending',
}
const STATUS_TEXT: Record<Build['status'], string> = {
  success: '成功',
  failed: '失败',
  running: '运行中',
  queued: '排队中',
  blocked: '阻断',
  awaiting_approval: '待审批',
  cancelled: '已取消',
}

export function BuildListPage() {
  const nav = useNavigate()
  const { data: builds = [] } = useQuery({ queryKey: ['builds'], queryFn: api.builds })
  const success = builds.filter((b) => b.status === 'success').length

  return (
    <>
      <PageHeader title="构建流水线" subtitle="全部项目的构建记录与统计" />
      <div className="grid grid-cols-4 gap-gutter mb-space-xl">
        <StatCard label="成功率 (30D)" value="94.6%" accent="success" trend="+1.4%" trendDir="up" />
        <StatCard label="本周构建频次" value={47} unit="次" sub="峰值 8/日" />
        <StatCard label="平均耗时" value="2m 27s" sub="上周 47s → 增量缓存命中" />
        <StatCard label="运行中" value={builds.filter((b) => b.status === 'running').length} accent="default" sub="100% 隔离" />
      </div>

      <DataTable
        rows={builds}
        onRowClick={(b) => nav(`/builds/${b.number}`)}
        columns={[
          {
            key: 'number',
            header: '构建号',
            render: (b) => (
              <Link to={`/builds/${b.number}`} className="font-mono text-primary hover:underline" onClick={(e) => e.stopPropagation()}>
                #{b.number}
              </Link>
            ),
          },
          { key: 'project', header: '项目', render: (b) => b.projectName },
          {
            key: 'trigger',
            header: '触发源',
            render: (b) => (
              <div>
                <span className="text-body-sm">{TRIGGER_TEXT[b.triggerSource]}</span>
                <div className="text-label-sm text-outline">{b.triggerDetail}</div>
              </div>
            ),
          },
          {
            key: 'branch',
            header: '目标分支 / 提交',
            render: (b) => (
              <div className="font-mono text-body-sm">
                <div>{b.branch}</div>
                <div className="text-outline">{b.commitSha}</div>
              </div>
            ),
          },
          {
            key: 'status',
            header: '状态',
            render: (b) => (
              <div>
                <StatusBadge tone={STATUS_TONE[b.status]}>{STATUS_TEXT[b.status]}</StatusBadge>
                {b.blockedStage && <div className="text-label-sm text-error mt-1">{b.blockedStage} 阻断</div>}
              </div>
            ),
          },
          {
            key: 'duration',
            header: '执行耗时',
            render: (b) => (b.durationMs ? <span className="font-mono">{(b.durationMs / 1000).toFixed(1)}s</span> : '—'),
          },
          { key: 'time', header: '触发时间', render: (b) => fmtRel(b.createdAt) },
        ]}
      />
      <p className="text-label-sm text-outline mt-space-md">共 128 条记录 · 显示最近 {builds.length} 条（mock）· 成功 {success}</p>
    </>
  )
}

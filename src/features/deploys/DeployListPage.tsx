import { useQuery } from '@tanstack/react-query'
import { Link } from 'react-router'
import { api } from '@/api'
import { fmtDur, fmtRel } from '@/api/format'
import { DataTable, Pagination, PageHeader, Button } from '@/components/ui/DataTable'
import { Segmented } from '@/components/ui/Badge'
import { StatusBadge, StatCard } from '@/components/ui/Badge'
import { Icon } from '@/components/ui/Icon'
import { useState } from 'react'
import type { Deploy } from '@/api/types'

const STATUS_TONE: Record<Deploy['status'], 'success' | 'error' | 'running' | 'pending'> = {
  success: 'success',
  failed: 'error',
  running: 'running',
  rolled_back: 'pending',
  pending: 'running',
  awaiting_approval: 'pending',
}
const STATUS_TEXT: Record<Deploy['status'], string> = {
  success: '成功',
  failed: '失败',
  running: '进行中',
  rolled_back: '已回滚',
  pending: '待执行',
  awaiting_approval: '待审批',
}

type Filter = 'all' | 'running' | 'success' | 'failed' | 'rolled_back'

export function DeployListPage() {
  const { data = [] } = useQuery({ queryKey: ['deploys'], queryFn: api.deploys })
  const [filter, setFilter] = useState<Filter>('all')
  const [page, setPage] = useState(1)
  const pageSize = 20

  const filtered = filter === 'all' ? data : data.filter((d) => d.status === filter)
  const paged = filtered.slice((page - 1) * pageSize, page * pageSize)

  return (
    <>
      <PageHeader
        title="部署记录"
        subtitle="全部项目的部署与回滚历史"
        actions={
          <Button icon="add" onClick={() => alert('新建部署（演示）')}>
            新建部署
          </Button>
        }
      />

      <div className="grid grid-cols-4 gap-gutter mb-space-xl">
        <StatCard label="本周部署" value={23} trend="3" trendDir="up" />
        <StatCard label="30 天成功率" value="96.4%" accent="success" />
        <StatCard label="平均耗时" value="2m 41s" sub="上周 2m 55s" />
        <StatCard label="回滚次数" value={2} accent="warning" sub="近 30 天 · 均已恢复" />
      </div>

      <div className="flex items-center justify-between mb-space-lg">
        <Segmented
          value={filter}
          onChange={(v) => {
            setFilter(v)
            setPage(1)
          }}
          options={[
            { value: 'all', label: '全部' },
            { value: 'running', label: '进行中' },
            { value: 'success', label: '成功' },
            { value: 'failed', label: '失败' },
            { value: 'rolled_back', label: '已回滚' },
          ]}
        />
        <button className="flex items-center gap-space-sm bg-surface-container rounded px-space-lg py-1 text-body-sm text-on-surface-variant border border-surface-container-highest">
          项目: 全部
          <Icon name="expand_more" size={14} />
        </button>
      </div>

      <DataTable
        rows={paged}
        columns={[
          {
            key: 'number',
            header: '部署单',
            render: (d) => <span className="font-mono text-primary">{d.number}</span>,
          },
          { key: 'project', header: '项目', render: (d) => d.projectName },
          {
            key: 'tag',
            header: '镜像 Tag',
            render: (d) => <span className="font-mono text-body-sm">{d.imageTag}</span>,
          },
          {
            key: 'target',
            header: '目标主机',
            render: (d) => (
              <div>
                <div>{d.targetName}</div>
                {d.healthNote && d.status === 'running' && (
                  <div className="text-label-sm text-outline mt-0.5">{d.healthNote}</div>
                )}
                {d.healthNote && d.status === 'rolled_back' && (
                  <div className="text-label-sm text-outline mt-0.5" title={d.healthNote}>
                    {d.healthNote}
                  </div>
                )}
              </div>
            ),
          },
          {
            key: 'build',
            header: '触发构建',
            render: (d) => (
              <Link to={`/builds/${d.buildNumber}`} className="font-mono text-primary hover:underline">
                #{d.buildNumber}
              </Link>
            ),
          },
          {
            key: 'status',
            header: '状态',
            render: (d) => (
              <StatusBadge tone={STATUS_TONE[d.status]}>{STATUS_TEXT[d.status]}</StatusBadge>
            ),
          },
          {
            key: 'duration',
            header: '耗时',
            render: (d) => (d.durationMs ? <span className="font-mono">{fmtDur(d.durationMs)}</span> : '—'),
          },
          { key: 'time', header: '时间', render: (d) => fmtRel(d.createdAt) },
          {
            key: 'actions',
            header: '操作',
            align: 'right',
            render: (d) => (
              <div className="flex justify-end gap-space-md">
                <Button variant="ghost" size="sm" onClick={() => alert(`查看 ${d.number}（演示）`)}>
                  查看
                </Button>
                {d.status === 'success' && (
                  <Button variant="secondary" size="sm" onClick={() => alert(`回滚 ${d.number}（演示）`)}>
                    回滚
                  </Button>
                )}
              </div>
            ),
          },
        ]}
      />
      <Pagination page={page} pageSize={pageSize} total={filtered.length} onPage={setPage} />
    </>
  )
}

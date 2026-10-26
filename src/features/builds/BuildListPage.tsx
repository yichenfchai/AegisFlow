import { Link } from 'react-router'
import { useQuery } from '@tanstack/react-query'
import { DataTable, PageHeader, Pagination, type Column } from '@/components/ui/DataTable'
import { StatusBadge } from '@/components/ui/Badge'
import { buildsAPI, BUILD_STATUS_TEXT, BUILD_STATUS_TONE, type Build } from '@/api/builds'
import { projectsAPI } from '@/api/projects'
import { fmtRel } from '@/api/format'
import { useState } from 'react'

const PAGE_SIZE = 12

/** 构建列表（真实接口）：P3 第一批，替代原构建流水线演示页。 */
export function BuildListPage() {
  const [page, setPage] = useState(1)
  const { data, isLoading, error } = useQuery({
    queryKey: ['builds', page],
    queryFn: () => buildsAPI.builds(PAGE_SIZE, (page - 1) * PAGE_SIZE),
  })
  // Project names for display; a missing project shows the id prefix.
  const { data: projectPage } = useQuery({
    queryKey: ['projects'],
    queryFn: () => projectsAPI.projects(100, 0),
    staleTime: 60_000,
  })
  const projects = projectPage?.items ?? []
  const nameOf = (projectId: string) => projects.find((p) => p.id === projectId)?.name ?? projectId.slice(0, 8)

  const columns: Column<Build>[] = [
    {
      key: 'buildNumber',
      header: '构建号',
      render: (b) => (
        <Link to={`/builds/${b.id}`} className="font-mono text-primary hover:underline">
          #{b.buildNumber}
        </Link>
      ),
    },
    { key: 'project', header: '项目', render: (b) => <span title={b.projectId}>{nameOf(b.projectId)}</span> },
    { key: 'branch', header: '分支', render: (b) => <span className="font-mono text-body-sm">{b.branch || '—'}</span> },
    {
      key: 'status',
      header: '状态',
      render: (b) => <StatusBadge tone={BUILD_STATUS_TONE[b.status]}>{BUILD_STATUS_TEXT[b.status]}</StatusBadge>,
    },
    {
      key: 'duration',
      header: '耗时',
      render: (b) =>
        b.startedAt && b.finishedAt ? (
          <span className="font-mono">{b.finishedAt - b.startedAt}s</span>
        ) : (
          '—'
        ),
    },
    { key: 'createdAt', header: '触发时间', render: (b) => fmtRel(b.createdAt * 1000) },
  ]

  return (
    <>
      <PageHeader title="构建" subtitle="全部项目的构建记录；在项目列表触发构建" />
      {error ? (
        <p role="alert" className="text-body-md text-error py-space-lg">
          {error instanceof Error ? error.message : '加载构建失败'}
        </p>
      ) : isLoading ? (
        <p className="text-body-md text-outline py-space-lg">加载中...</p>
      ) : (
        <>
          <DataTable
            columns={columns}
            rows={data?.items ?? []}
            empty="还没有构建；到项目列表触发第一次构建"
          />
          <Pagination page={page} pageSize={PAGE_SIZE} total={data?.total ?? 0} onPage={setPage} />
        </>
      )}
    </>
  )
}

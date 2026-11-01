import { useState } from 'react'
import { Link } from 'react-router'
import { useQuery } from '@tanstack/react-query'
import { DataTable, PageHeader, Pagination, Select, Field, type Column } from '@/components/ui/DataTable'
import { StatusBadge } from '@/components/ui/Badge'
import {
  deployTasksAPI,
  DEPLOY_ENVIRONMENT_TEXT,
  DEPLOY_ENVIRONMENT_TONE,
  DEPLOYMENT_STATUS_TEXT,
  DEPLOYMENT_STATUS_TONE,
  type Deployment,
  type DeploymentStatus,
} from '@/api/deployTasks'
import { projectsAPI } from '@/api/projects'
import { ApiError } from '@/api/http'
import { fmtRel } from '@/api/format'

const PAGE_SIZE = 12
const STATUSES: DeploymentStatus[] = [
  'queued',
  'awaiting_approval',
  'running',
  'succeeded',
  'failed',
  'cancelled',
  'interrupted',
  'rejected',
]

function loadError(cause: unknown): string {
  if (cause instanceof ApiError) {
    if (cause.status === 400) return '筛选条件不合法，请重置后重试'
    if (cause.status === 403) return '当前角色没有查看部署记录的权限'
    if (cause.status === 503) return '服务依赖不可用，请稍后重试'
    if (cause.status === 0) return '无法连接服务，请检查网络后重试'
  }
  return cause instanceof Error ? cause.message : '加载部署记录失败'
}

/**
 * 部署记录（真实接口）：管理端全局视图，项目与状态筛选走服务端分页。
 */
export function DeploymentListPage() {
  const [page, setPage] = useState(1)
  const [status, setStatus] = useState<DeploymentStatus | ''>('')
  const [projectFilter, setProjectFilter] = useState('')

  const projectsQuery = useQuery({
    queryKey: ['projects'],
    queryFn: () => projectsAPI.projects(100, 0),
    staleTime: 60_000,
  })
  const projects = projectsQuery.data?.items ?? []

  const { data, isPending, error } = useQuery({
    queryKey: ['deployments', projectFilter, status, page],
    queryFn: () =>
      deployTasksAPI.list({
        projectId: projectFilter || undefined,
        status: status || undefined,
        limit: PAGE_SIZE,
        offset: (page - 1) * PAGE_SIZE,
      }),
  })

  const nameOf = (id: string) => projects.find((p) => p.id === id)?.name ?? id.slice(0, 8)
  const loading = isPending

  const columns: Column<Deployment>[] = [
    {
      key: 'number',
      header: '编号',
      render: (d) => (
        <Link to={`/deploys/${d.id}`} className="font-mono text-primary hover:underline">
          #{d.deploymentNumber}
        </Link>
      ),
    },
    { key: 'project', header: '项目', render: (d) => <span title={d.projectId}>{nameOf(d.projectId)}</span> },
    { key: 'target', header: '目标', render: (d) => <span title={d.targetEndpoint}>{d.targetName}</span> },
    {
      key: 'environment',
      header: '环境',
      render: (d) => (
        <StatusBadge tone={DEPLOY_ENVIRONMENT_TONE[d.targetEnvironment]} dot={false}>
          {DEPLOY_ENVIRONMENT_TEXT[d.targetEnvironment]}
        </StatusBadge>
      ),
    },
    { key: 'version', header: '版本', render: (d) => <span className="font-mono text-body-sm">{d.version}</span> },
    {
      key: 'status',
      header: '状态',
      render: (d) => (
        <StatusBadge tone={DEPLOYMENT_STATUS_TONE[d.status]}>{DEPLOYMENT_STATUS_TEXT[d.status]}</StatusBadge>
      ),
    },
    { key: 'createdAt', header: '创建时间', render: (d) => fmtRel(d.createdAt * 1000) },
    {
      key: 'actions',
      header: '操作',
      align: 'right',
      render: (d) => (
        <Link to={`/deploys/${d.id}`} className="text-label-sm text-primary hover:underline">
          详情
        </Link>
      ),
    },
  ]

  return (
    <>
      <PageHeader
        title="部署记录"
        subtitle="全部项目的部署历史；部署与审批仅管理员可用"
        actions={
          <Link
            to="/deploys/new"
            className="inline-flex items-center gap-space-sm rounded px-space-xl py-1.5 text-body-md bg-primary text-on-primary hover:brightness-110 font-medium"
          >
            新建部署
          </Link>
        }
      />
      <div className="mb-space-lg flex flex-wrap items-end gap-space-md">
        <Field label="项目" htmlFor="deploy-project" className="w-[220px]">
          <Select
            id="deploy-project"
            value={projectFilter}
            onChange={(e) => {
              setProjectFilter(e.target.value)
              setPage(1)
            }}
          >
            <option value="">全部项目</option>
            {projects.map((p) => (
              <option key={p.id} value={p.id}>
                {p.name}
              </option>
            ))}
          </Select>
        </Field>
        <Field label="状态" htmlFor="deploy-status" className="w-[160px]">
          <Select
            id="deploy-status"
            value={status}
            onChange={(e) => {
              setStatus(e.target.value as DeploymentStatus | '')
              setPage(1)
            }}
          >
            <option value="">全部</option>
            {STATUSES.map((value) => (
              <option key={value} value={value}>
                {DEPLOYMENT_STATUS_TEXT[value]}
              </option>
            ))}
          </Select>
        </Field>
      </div>
      {error ? (
        <p role="alert" className="text-body-md text-error py-space-lg">
          {loadError(error)}
        </p>
      ) : loading ? (
        <p className="text-body-md text-outline py-space-lg">加载中...</p>
      ) : (
        <>
          <DataTable columns={columns} rows={data?.items ?? []} empty="还没有部署记录" />
          <Pagination page={page} pageSize={PAGE_SIZE} total={data?.total ?? 0} onPage={setPage} />
        </>
      )}
    </>
  )
}

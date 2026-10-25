import { useState } from 'react'
import { Link } from 'react-router'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { Button, ConfirmDialog, DataTable, PageHeader, Pagination, type Column } from '@/components/ui/DataTable'
import { StatusBadge } from '@/components/ui/Badge'
import { projectsAPI, type Project } from '@/api/projects'
import { useSession } from '@/stores/session'
import { ApiError } from '@/api/http'
import { fmtRel } from '@/api/format'

const PAGE_SIZE = 12

/** 项目列表（真实接口）：P2 主线页面，替代原仪表盘演示。 */
export function ProjectsPage() {
  const { user } = useSession()
  const canEdit = user?.role === 'admin' || user?.role === 'member'
  const queryClient = useQueryClient()
  const [page, setPage] = useState(1)
  const { data, isLoading, error } = useQuery({
    queryKey: ['projects', page],
    queryFn: () => projectsAPI.projects(PAGE_SIZE, (page - 1) * PAGE_SIZE),
  })
  const [toggleTarget, setToggleTarget] = useState<Project | null>(null)
  const [actionError, setActionError] = useState('')
  const [busy, setBusy] = useState(false)

  const refresh = () => queryClient.invalidateQueries({ queryKey: ['projects'] })
  const toggleArchive = async () => {
    const target = toggleTarget
    if (!target) return
    setActionError('')
    setBusy(true)
    try {
      await projectsAPI.update(target.id, { archived: target.status !== 'archived' })
      setToggleTarget(null)
      await refresh()
    } catch (cause) {
      setActionError(cause instanceof ApiError && cause.status === 409 ? '项目已归档，只能先恢复再修改' : cause instanceof Error ? cause.message : '操作失败，请稍后重试')
    } finally {
      setBusy(false)
    }
  }

  const columns: Column<Project>[] = [
    {
      key: 'name',
      header: '项目',
      render: (p) => (
        <div>
          <div className="font-medium">{p.name}</div>
          {p.description && <div className="text-label-sm text-outline">{p.description}</div>}
        </div>
      ),
    },
    {
      key: 'repoUrl',
      header: '仓库 / 路径',
      render: (p) => (
        <span className="font-mono text-label-md text-on-surface-variant break-all" title={p.repoUrl}>
          {p.repoUrl || '—'}
        </span>
      ),
    },
    { key: 'defaultBranch', header: '默认分支', render: (p) => <span className="font-mono text-label-md">{p.defaultBranch || '—'}</span> },
    {
      key: 'status',
      header: '状态',
      render: (p) => (p.status === 'active' ? <StatusBadge tone="success">活跃</StatusBadge> : <StatusBadge tone="pending">已归档</StatusBadge>),
    },
    { key: 'createdAt', header: '创建时间', render: (p) => fmtRel(p.createdAt * 1000) },
    {
      key: 'actions',
      header: '操作',
      align: 'right',
      render: (p) =>
        canEdit ? (
          <Button variant={p.status === 'archived' ? 'secondary' : 'danger'} size="sm" disabled={busy} onClick={() => setToggleTarget(p)}>
            {p.status === 'archived' ? '恢复' : '归档'}
          </Button>
        ) : (
          <span className="text-label-sm text-outline">—</span>
        ),
    },
  ]

  return (
    <>
      <PageHeader
        title="项目"
        subtitle="交付工作的起点；构建与部署将围绕项目展开"
        actions={
          canEdit ? (
            <Link
              to="/projects/new"
              className="inline-flex items-center gap-space-sm rounded px-space-xl py-1.5 text-body-md bg-primary text-on-primary hover:brightness-110 font-medium"
            >
              创建项目
            </Link>
          ) : undefined
        }
      />
      {actionError && (
        <p role="alert" className="mb-space-md text-body-sm text-error bg-error/10 rounded px-space-lg py-space-md">
          {actionError}
        </p>
      )}
      {error ? (
        <p role="alert" className="text-body-md text-error py-space-lg">
          {error instanceof Error ? error.message : '加载项目失败'}
        </p>
      ) : isLoading ? (
        <p className="text-body-md text-outline py-space-lg">加载中...</p>
      ) : (
        <>
          <DataTable columns={columns} rows={data?.items ?? []} empty="还没有项目；点击右上角创建第一个项目" />
          <Pagination page={page} pageSize={PAGE_SIZE} total={data?.total ?? 0} onPage={setPage} />
        </>
      )}
      <ConfirmDialog
        open={toggleTarget !== null}
        title={toggleTarget?.status === 'archived' ? `恢复 ${toggleTarget?.name ?? ''}` : `归档 ${toggleTarget?.name ?? ''}`}
        danger={toggleTarget?.status !== 'archived'}
        confirmText={toggleTarget?.status === 'archived' ? '确认恢复' : '确认归档'}
        onConfirm={() => void toggleArchive()}
        onCancel={() => setToggleTarget(null)}
      >
        {toggleTarget?.status === 'archived'
          ? '恢复后项目可再次编辑，后续构建与部署也能引用它。'
          : '归档后项目变为只读（恢复前不能修改）；后续构建/部署将限制引用已归档项目。'}
      </ConfirmDialog>
    </>
  )
}

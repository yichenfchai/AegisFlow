import { useState } from 'react'
import { Link } from 'react-router'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { Button, ConfirmDialog, DataTable, PageHeader, Pagination, type Column } from '@/components/ui/DataTable'
import { StatusBadge } from '@/components/ui/Badge'
import {
  deploymentTargetsAPI,
  DEPLOY_ENVIRONMENT_TEXT,
  DEPLOY_ENVIRONMENT_TONE,
  DEPLOY_TARGET_STATUS_TEXT,
  DEPLOY_TARGET_STATUS_TONE,
  lastCheckText,
  type DeploymentTarget,
} from '@/api/deployments'
import { ApiError } from '@/api/http'

const PAGE_SIZE = 12

function targetError(cause: unknown, action: string): string {
  if (cause instanceof ApiError) {
    if (cause.status === 400) return '请求不合法：名称需 3-64 字节，端点须为 http(s) URL，健康路径须以 / 开头'
    if (cause.status === 403) return '当前角色没有该操作权限'
    if (cause.status === 404) return '部署目标不存在或已被删除'
    if (cause.status === 409) return '同名部署目标已存在（不区分大小写）'
    if (cause.status === 503) return '服务依赖不可用，请稍后重试'
    if (cause.status === 0) return '无法连接服务，请检查网络后重试'
  }
  return cause instanceof Error ? cause.message : `${action}失败，请稍后重试`
}

/** 部署目标列表（真实接口）：管理员配置部署端点并做连通性检查。 */
export function DeploymentTargetsPage() {
  const queryClient = useQueryClient()
  const [page, setPage] = useState(1)
  const { data, isLoading, error } = useQuery({
    queryKey: ['deployment-targets', page],
    queryFn: () => deploymentTargetsAPI.targets(PAGE_SIZE, (page - 1) * PAGE_SIZE),
  })
  const [actionError, setActionError] = useState('')
  const [checkingId, setCheckingId] = useState<string | null>(null)
  const [busyId, setBusyId] = useState<string | null>(null)
  const [deleteTarget, setDeleteTarget] = useState<DeploymentTarget | null>(null)

  const refresh = () => queryClient.invalidateQueries({ queryKey: ['deployment-targets'] })

  const check = async (target: DeploymentTarget) => {
    setActionError('')
    setCheckingId(target.id)
    try {
      await deploymentTargetsAPI.check(target.id)
      await refresh()
    } catch (cause) {
      setActionError(targetError(cause, '检查'))
    } finally {
      setCheckingId(null)
    }
  }

  const toggle = async (target: DeploymentTarget) => {
    setActionError('')
    setBusyId(target.id)
    try {
      await deploymentTargetsAPI.update(target.id, {
        status: target.status === 'active' ? 'disabled' : 'active',
      })
      await refresh()
    } catch (cause) {
      setActionError(targetError(cause, target.status === 'active' ? '禁用' : '启用'))
    } finally {
      setBusyId(null)
    }
  }

  const remove = async () => {
    const target = deleteTarget
    if (!target) return
    setActionError('')
    setBusyId(target.id)
    try {
      await deploymentTargetsAPI.remove(target.id)
      setDeleteTarget(null)
      if ((data?.items.length ?? 0) <= 1 && page > 1) setPage(page - 1)
      await refresh()
    } catch (cause) {
      setActionError(targetError(cause, '删除'))
    } finally {
      setBusyId(null)
    }
  }

  const columns: Column<DeploymentTarget>[] = [
    {
      key: 'name',
      header: '名称',
      render: (t) => (
        <div>
          <Link to={`/deploy-targets/${t.id}`} className="font-medium text-primary hover:underline">
            {t.name}
          </Link>
          <div className="font-mono text-label-sm text-outline">{t.kind}</div>
        </div>
      ),
    },
    {
      key: 'environment',
      header: '环境',
      render: (t) => (
        <StatusBadge tone={DEPLOY_ENVIRONMENT_TONE[t.environment]} dot={false}>
          {DEPLOY_ENVIRONMENT_TEXT[t.environment]}
        </StatusBadge>
      ),
    },
    {
      key: 'status',
      header: '状态',
      render: (t) => (
        <StatusBadge tone={DEPLOY_TARGET_STATUS_TONE[t.status]}>
          {DEPLOY_TARGET_STATUS_TEXT[t.status]}
        </StatusBadge>
      ),
    },
    {
      key: 'endpoint',
      header: '端点',
      render: (t) => (
        <div className="font-mono text-label-md text-on-surface-variant break-all">
          <div title={t.endpoint}>{t.endpoint}</div>
          <div className="text-outline">{t.healthPath}</div>
        </div>
      ),
    },
    {
      key: 'secret',
      header: '凭据',
      render: (t) =>
        t.hasSecret ? <span className="text-label-md">已配置</span> : <span className="text-label-sm text-outline">—</span>,
    },
    {
      key: 'lastCheck',
      header: '最近检查',
      render: (t) => (
        <span className={`text-label-md ${t.lastCheckAt && !t.lastCheckOk ? 'text-error' : 'text-on-surface-variant'}`}>
          {lastCheckText(t)}
        </span>
      ),
    },
    {
      key: 'actions',
      header: '操作',
      align: 'right',
      render: (t) => (
        <div className="flex items-center justify-end gap-space-sm">
          <Button
            variant="secondary"
            size="sm"
            icon="monitor_heart"
            disabled={checkingId !== null || busyId !== null}
            onClick={() => void check(t)}
          >
            {checkingId === t.id ? '检查中...' : '检查'}
          </Button>
          <Button
            variant={t.status === 'active' ? 'danger' : 'secondary'}
            size="sm"
            disabled={checkingId !== null || busyId !== null}
            onClick={() => void toggle(t)}
          >
            {busyId === t.id ? '处理中...' : t.status === 'active' ? '禁用' : '启用'}
          </Button>
          <Button
            variant="danger"
            size="sm"
            disabled={busyId !== null}
            onClick={() => setDeleteTarget(t)}
          >
            删除
          </Button>
        </div>
      ),
    },
  ]

  return (
    <>
      <PageHeader
        title="部署目标"
        subtitle="部署端点、环境与凭据；修改与连通性检查仅管理员可用"
        actions={
          <Link
            to="/deploy-targets/new"
            className="inline-flex items-center gap-space-sm rounded px-space-xl py-1.5 text-body-md bg-primary text-on-primary hover:brightness-110 font-medium"
          >
            新建部署目标
          </Link>
        }
      />
      {actionError && (
        <p role="alert" className="mb-space-md text-body-sm text-error bg-error/10 rounded px-space-lg py-space-md">
          {actionError}
        </p>
      )}
      {error ? (
        <p role="alert" className="text-body-md text-error py-space-lg">
          {error instanceof Error ? error.message : '加载部署目标失败'}
        </p>
      ) : isLoading ? (
        <p className="text-body-md text-outline py-space-lg">加载中...</p>
      ) : (
        <>
          <DataTable
            columns={columns}
            rows={data?.items ?? []}
            empty="还没有部署目标；点击右上角创建第一个"
          />
          <Pagination page={page} pageSize={PAGE_SIZE} total={data?.total ?? 0} onPage={setPage} />
        </>
      )}
      <ConfirmDialog
        open={deleteTarget !== null}
        title={`删除 ${deleteTarget?.name ?? ''}`}
        danger
        confirmText="确认删除"
        onConfirm={() => void remove()}
        onCancel={() => setDeleteTarget(null)}
      >
        删除后该部署目标不可恢复。
      </ConfirmDialog>
    </>
  )
}

import { useState } from 'react'
import { Link, useNavigate, useParams } from 'react-router'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { Button, ConfirmDialog, Field, Input, PageHeader, Select } from '@/components/ui/DataTable'
import { StatusBadge } from '@/components/ui/Badge'
import {
  deploymentTargetsAPI,
  DEPLOY_ENVIRONMENT_TEXT,
  DEPLOY_ENVIRONMENT_TONE,
  DEPLOY_TARGET_STATUS_TEXT,
  DEPLOY_TARGET_STATUS_TONE,
  lastCheckText,
  type DeploymentEnvironment,
  type DeploymentTarget,
  type DeploymentTargetCheck,
  type DeploymentTargetStatus,
} from '@/api/deployments'
import { ApiError } from '@/api/http'
import { fmtRel } from '@/api/format'

const ENVIRONMENTS: DeploymentEnvironment[] = ['development', 'staging', 'production']
const STATUSES: DeploymentTargetStatus[] = ['active', 'disabled']

function byteLength(value: string): number {
  return new TextEncoder().encode(value).length
}

function targetError(cause: unknown, action: string): string {
  if (cause instanceof ApiError) {
    if (cause.status === 400) {
      return '字段不合法：名称 3-64 字节且单行；端点须为 http(s) URL 且不含用户信息、查询或片段；健康路径以 / 开头；凭据仅可配合 https 或回环地址'
    }
    if (cause.status === 403) return '当前角色没有该操作权限'
    if (cause.status === 404) return '部署目标不存在或已被删除'
    if (cause.status === 409) return '同名部署目标已存在（不区分大小写）'
    if (cause.status === 503) return '服务依赖不可用，请稍后重试'
    if (cause.status === 0) return '无法连接服务，请检查网络后重试'
  }
  return cause instanceof Error ? cause.message : `${action}失败，请稍后重试`
}

/** 部署目标详情（真实接口）：编辑、检查连通性、清除凭据与删除。 */
export function DeploymentTargetDetailPage() {
  const { id = '' } = useParams<{ id: string }>()
  const nav = useNavigate()
  const queryClient = useQueryClient()
  const { data: target, isLoading, error } = useQuery({
    queryKey: ['deployment-target', id],
    queryFn: () => deploymentTargetsAPI.target(id),
    enabled: id !== '',
    retry: false,
  })
  const [actionError, setActionError] = useState('')
  const [notice, setNotice] = useState('')
  const [checking, setChecking] = useState(false)
  const [checkResult, setCheckResult] = useState<DeploymentTargetCheck | null>(null)
  const [deleting, setDeleting] = useState(false)
  const [confirmDelete, setConfirmDelete] = useState(false)

  const refresh = async () => {
    await queryClient.invalidateQueries({ queryKey: ['deployment-target', id] })
    await queryClient.invalidateQueries({ queryKey: ['deployment-targets'] })
  }

  const check = async () => {
    if (!target) return
    setActionError('')
    setNotice('')
    setChecking(true)
    try {
      const result = await deploymentTargetsAPI.check(target.id)
      setCheckResult(result)
      await refresh()
    } catch (cause) {
      setActionError(targetError(cause, '检查'))
    } finally {
      setChecking(false)
    }
  }

  const remove = async () => {
    if (!target) return
    setActionError('')
    setDeleting(true)
    try {
      await deploymentTargetsAPI.remove(target.id)
      await queryClient.invalidateQueries({ queryKey: ['deployment-targets'] })
      nav('/deploy-targets')
    } catch (cause) {
      setActionError(targetError(cause, '删除'))
    } finally {
      setDeleting(false)
    }
  }

  if (error && !target) {
    const status = error instanceof ApiError ? error.status : 0
    return (
      <>
        <PageHeader title="部署目标详情" />
        <p role="alert" className="text-body-md text-error py-space-lg">
          {status === 404 ? '未找到该部署目标' : targetError(error, '读取')}
        </p>
        <Link to="/deploy-targets" className="text-body-md text-primary hover:underline">
          返回部署目标列表
        </Link>
      </>
    )
  }
  if (isLoading || !target) {
    return (
      <>
        <PageHeader title="部署目标详情" subtitle="加载中..." />
        <p className="text-body-md text-outline py-space-lg">加载中...</p>
      </>
    )
  }

  return (
    <>
      <PageHeader
        title={target.name}
        subtitle={`部署目标 · ${DEPLOY_ENVIRONMENT_TEXT[target.environment]} · 创建于 ${fmtRel(target.createdAt * 1000)}`}
        actions={
          <>
            <StatusBadge tone={DEPLOY_TARGET_STATUS_TONE[target.status]}>
              {DEPLOY_TARGET_STATUS_TEXT[target.status]}
            </StatusBadge>
            <Button variant="secondary" icon="monitor_heart" disabled={checking} onClick={() => void check()}>
              {checking ? '检查中...' : '检查连通性'}
            </Button>
            <Button variant="danger" icon="delete" disabled={deleting} onClick={() => setConfirmDelete(true)}>
              删除
            </Button>
          </>
        }
      />
      {actionError && (
        <p role="alert" className="mb-space-md text-body-sm text-error bg-error/10 rounded px-space-lg py-space-md">
          {actionError}
        </p>
      )}
      {notice && (
        <p role="status" className="mb-space-md text-body-sm text-success bg-success/10 rounded px-space-lg py-space-md">
          {notice}
        </p>
      )}

      <div className="grid grid-cols-[340px_1fr] gap-gutter">
        <aside className="bg-surface-container rounded-lg border border-surface-container-highest p-space-xl space-y-space-lg self-start">
          <MetaRow label="环境">
            <StatusBadge tone={DEPLOY_ENVIRONMENT_TONE[target.environment]} dot={false}>
              {DEPLOY_ENVIRONMENT_TEXT[target.environment]}
            </StatusBadge>
          </MetaRow>
          <MetaRow label="端点">
            <span className="font-mono break-all">{target.endpoint}</span>
          </MetaRow>
          <MetaRow label="健康路径">
            <span className="font-mono">{target.healthPath}</span>
          </MetaRow>
          <MetaRow label="凭据">{target.hasSecret ? '已配置' : '未配置'}</MetaRow>
          <MetaRow label="最近检查">
            <span className={target.lastCheckAt && !target.lastCheckOk ? 'text-error' : undefined}>
              {lastCheckText(target)}
            </span>
          </MetaRow>
          <MetaRow label="创建时间">{fmtRel(target.createdAt * 1000)}</MetaRow>
          <MetaRow label="更新时间">{fmtRel(target.updatedAt * 1000)}</MetaRow>
          <p className="text-label-sm text-outline pt-space-md border-t border-surface-container-highest">
            凭据加密存储且永不回显；检查时以 Bearer 头发送。
          </p>
        </aside>

        <div className="space-y-gutter">
          <EditTargetCard
            key={`${target.id}-${target.updatedAt}`}
            target={target}
            onSaved={(message) => {
              setActionError('')
              setNotice(message)
            }}
            onError={(message) => {
              setNotice('')
              setActionError(message)
            }}
          />
          <section className="bg-surface-container rounded-lg border border-surface-container-highest p-space-xl">
            <h2 className="text-headline-md font-semibold mb-space-lg">连通性检查</h2>
            {checkResult ? (
              <div role="status" className="space-y-space-md">
                <div className="flex flex-wrap items-center gap-space-md text-body-sm">
                  <StatusBadge tone={checkResult.ok ? 'success' : 'error'}>
                    {checkResult.ok ? '健康' : '异常'}
                  </StatusBadge>
                  <span className="text-on-surface-variant">状态码 {checkResult.statusCode || '—'}</span>
                  <span className="text-on-surface-variant">延迟 {checkResult.latencyMs}ms</span>
                  <span className="text-outline">{fmtRel(checkResult.checkedAt * 1000)}</span>
                </div>
                <p className="text-body-sm text-on-surface-variant break-all">详情：{checkResult.detail || '—'}</p>
              </div>
            ) : (
              <p className="text-body-sm text-on-surface-variant">
                {target.lastCheckAt
                  ? `最近检查：${lastCheckText(target)}${target.lastCheckDetail ? ` · ${target.lastCheckDetail}` : ''}`
                  : '尚未检查；点击右上角“检查连通性”发起一次探测。'}
              </p>
            )}
          </section>
        </div>
      </div>

      <ConfirmDialog
        open={confirmDelete}
        title={`删除 ${target.name}`}
        danger
        confirmText="确认删除"
        onConfirm={() => void remove()}
        onCancel={() => setConfirmDelete(false)}
      >
        删除后该部署目标不可恢复。
      </ConfirmDialog>
    </>
  )
}

function EditTargetCard({
  target,
  onSaved,
  onError,
}: {
  target: DeploymentTarget
  onSaved: (message: string) => void
  onError: (message: string) => void
}) {
  const queryClient = useQueryClient()
  const [name, setName] = useState(target.name)
  const [environment, setEnvironment] = useState<DeploymentEnvironment>(target.environment)
  const [endpoint, setEndpoint] = useState(target.endpoint)
  const [healthPath, setHealthPath] = useState(target.healthPath)
  const [status, setStatus] = useState<DeploymentTargetStatus>(target.status)
  const [secret, setSecret] = useState('')
  const [busy, setBusy] = useState(false)
  const [confirmClear, setConfirmClear] = useState(false)

  const apply = async (patch: { secret?: string }) => {
    const updated = await deploymentTargetsAPI.update(target.id, {
      name: name.trim(),
      environment,
      endpoint: endpoint.trim(),
      healthPath: healthPath.trim() || '/',
      status,
      ...patch,
    })
    queryClient.setQueryData(['deployment-target', target.id], updated)
    await queryClient.invalidateQueries({ queryKey: ['deployment-targets'] })
    setSecret('')
  }

  const save = async (e: React.FormEvent) => {
    e.preventDefault()
    if (busy) return
    onError('')
    const trimmedName = name.trim()
    const trimmedEndpoint = endpoint.trim()
    const trimmedHealthPath = healthPath.trim() || '/'
    if (byteLength(trimmedName) < 3 || byteLength(trimmedName) > 64) {
      onError('名称需为 3-64 字节')
      return
    }
    if (name !== name.trim() || /[\r\n]/.test(name)) {
      onError('名称不能包含首尾空白或换行')
      return
    }
    if (!/^https?:\/\/\S+$/.test(trimmedEndpoint)) {
      onError('端点须为 http:// 或 https:// 开头的 URL，且不含空白')
      return
    }
    if (!trimmedHealthPath.startsWith('/') || /\s/.test(trimmedHealthPath)) {
      onError('健康路径须以 / 开头且不含空白')
      return
    }
    setBusy(true)
    try {
      await apply(secret ? { secret } : {})
      onSaved('已保存')
    } catch (cause) {
      onError(targetError(cause, '保存'))
    } finally {
      setBusy(false)
    }
  }

  const clearSecret = async () => {
    if (busy) return
    onError('')
    setBusy(true)
    try {
      // Only the secret pointer is sent: pending edits in the other fields stay
      // unsaved until the user submits the form.
      const updated = await deploymentTargetsAPI.update(target.id, { secret: '' })
      queryClient.setQueryData(['deployment-target', target.id], updated)
      await queryClient.invalidateQueries({ queryKey: ['deployment-targets'] })
      setConfirmClear(false)
      onSaved('凭据已清除')
    } catch (cause) {
      onError(targetError(cause, '清除凭据'))
    } finally {
      setBusy(false)
    }
  }

  return (
    <section className="bg-surface-container rounded-lg border border-surface-container-highest p-space-xl">
      <h2 className="text-headline-md font-semibold mb-space-lg">编辑</h2>
      <form onSubmit={save} className="space-y-space-lg" aria-busy={busy}>
        <Field label="名称" hint="3-64 字节，单行，全局唯一" htmlFor="target-name">
          <Input id="target-name" value={name} onChange={(e) => setName(e.target.value)} disabled={busy} />
        </Field>
        <div className="grid grid-cols-2 gap-space-xl">
          <Field label="环境" htmlFor="target-environment">
            <Select
              id="target-environment"
              value={environment}
              onChange={(e) => setEnvironment(e.target.value as DeploymentEnvironment)}
              disabled={busy}
            >
              {ENVIRONMENTS.map((value) => (
                <option key={value} value={value}>
                  {DEPLOY_ENVIRONMENT_TEXT[value]}
                </option>
              ))}
            </Select>
          </Field>
          <Field label="状态" hint="已禁用的目标仍可检查" htmlFor="target-status">
            <Select
              id="target-status"
              value={status}
              onChange={(e) => setStatus(e.target.value as DeploymentTargetStatus)}
              disabled={busy}
            >
              {STATUSES.map((value) => (
                <option key={value} value={value}>
                  {DEPLOY_TARGET_STATUS_TEXT[value]}
                </option>
              ))}
            </Select>
          </Field>
        </div>
        <Field label="端点" hint="http(s) URL，不含用户信息、查询参数或片段" htmlFor="target-endpoint">
          <Input
            id="target-endpoint"
            mono
            value={endpoint}
            onChange={(e) => setEndpoint(e.target.value)}
            disabled={busy}
          />
        </Field>
        <Field label="健康路径" hint="以 / 开头" htmlFor="target-health-path">
          <Input
            id="target-health-path"
            mono
            value={healthPath}
            onChange={(e) => setHealthPath(e.target.value)}
            disabled={busy}
          />
        </Field>
        <Field
          label="替换凭据"
          hint={
            target.hasSecret
              ? '当前已配置凭据；留空表示保持不变'
              : '当前未配置；仅 https 或回环地址可配置'
          }
          htmlFor="target-secret"
        >
          <Input
            id="target-secret"
            type="password"
            autoComplete="new-password"
            value={secret}
            onChange={(e) => setSecret(e.target.value)}
            disabled={busy}
            placeholder="留空保持不变"
          />
        </Field>
        <div className="flex items-center gap-space-md">
          <Button type="submit" disabled={busy}>
            {busy ? '保存中...' : '保存修改'}
          </Button>
          <Button
            type="button"
            variant="danger"
            disabled={busy || !target.hasSecret}
            onClick={() => setConfirmClear(true)}
          >
            清除凭据
          </Button>
        </div>
      </form>
      <ConfirmDialog
        open={confirmClear}
        title={`清除 ${target.name} 的凭据`}
        danger
        confirmText="确认清除"
        onConfirm={() => void clearSecret()}
        onCancel={() => setConfirmClear(false)}
      >
        清除后检查请求不再携带 Bearer 凭据，且不可恢复。
      </ConfirmDialog>
    </section>
  )
}

function MetaRow({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="flex items-start justify-between gap-space-lg text-body-sm">
      <span className="text-outline shrink-0">{label}</span>
      <span className="text-on-surface text-right min-w-0">{children}</span>
    </div>
  )
}

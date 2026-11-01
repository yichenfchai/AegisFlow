import { useEffect, useRef, useState } from 'react'
import { Link, useLocation, useNavigate, useParams } from 'react-router'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { Button, ConfirmDialog, PageHeader } from '@/components/ui/DataTable'
import { StatusBadge } from '@/components/ui/Badge'
import {
  deployTasksAPI,
  isDeploymentTerminal,
  DEPLOY_ENVIRONMENT_TEXT,
  DEPLOY_ENVIRONMENT_TONE,
  DEPLOYMENT_STATUS_TEXT,
  DEPLOYMENT_STATUS_TONE,
  type DeploymentEvent,
  type DeploymentStatus,
} from '@/api/deployTasks'
import { projectsAPI } from '@/api/projects'
import { ApiError } from '@/api/http'
import { fmtRel } from '@/api/format'

const CANCELABLE: DeploymentStatus[] = ['queued', 'awaiting_approval', 'running']
const ROLLBACKABLE: DeploymentStatus[] = ['succeeded', 'failed']
/** 终态后继续轮询一小段时间，等后端把最后一批事件 flush 落库。 */
const EVENT_TAIL_GRACE_MS = 4000
const EVENT_POLL_MS = 1500

function deployError(cause: unknown, action: string): string {
  if (cause instanceof ApiError) {
    if (cause.status === 400) return `无法${action}：没有更早的成功版本可回滚，或目标已禁用`
    if (cause.status === 403) return '当前角色没有该操作权限'
    if (cause.status === 404) return '部署不存在或已被删除'
    if (cause.status === 409) return `无法${action}：部署当前状态不允许该操作`
    if (cause.status === 503) return '服务依赖不可用，请稍后重试'
    if (cause.status === 0) return '无法连接服务，请检查网络后重试'
  }
  return cause instanceof Error ? cause.message : `${action}失败，请稍后重试`
}

interface EventState {
  deploymentId: string
  lines: DeploymentEvent[]
  error: string
}

/** 事件轮询：按事件 id 游标累积，终态后多读一个宽限期再停止。 */
function useDeploymentEvents(
  deploymentId: string,
  terminal: boolean,
  enabled: boolean,
): { lines: DeploymentEvent[]; error: string } {
  const [state, setState] = useState<EventState>({ deploymentId, lines: [], error: '' })
  const cursor = useRef(0)
  const terminalRef = useRef(terminal)

  useEffect(() => {
    terminalRef.current = terminal
  }, [terminal])

  useEffect(() => {
    if (!enabled) return
    let cancelled = false
    let timer: ReturnType<typeof setTimeout> | undefined
    let tailDeadline = 0
    cursor.current = 0

    const poll = async () => {
      try {
        const page = await deployTasksAPI.events(deploymentId, cursor.current)
        if (cancelled) return
        if (page.items.length > 0) cursor.current = page.items[page.items.length - 1].id
        setState((prev) => ({
          deploymentId,
          lines: [...(prev.deploymentId === deploymentId ? prev.lines : []), ...page.items],
          error: '',
        }))
      } catch (cause) {
        if (cancelled) return
        setState((prev) => ({
          deploymentId,
          lines: prev.deploymentId === deploymentId ? prev.lines : [],
          error: cause instanceof Error ? cause.message : '事件加载失败',
        }))
      }
      if (cancelled) return
      if (terminalRef.current) {
        if (tailDeadline === 0) tailDeadline = Date.now() + EVENT_TAIL_GRACE_MS
        if (Date.now() >= tailDeadline) return
      } else {
        tailDeadline = 0
      }
      timer = setTimeout(poll, EVENT_POLL_MS)
    }

    void poll()
    return () => {
      cancelled = true
      if (timer) clearTimeout(timer)
    }
  }, [deploymentId, enabled])

  return state.deploymentId === deploymentId
    ? { lines: state.lines, error: state.error }
    : { lines: [], error: '' }
}

/** 部署详情（真实接口）：状态、审批、事件与取消/回滚。 */
export function DeploymentDetailPage() {
  const { id = '' } = useParams<{ id: string }>()
  const location = useLocation()
  const nav = useNavigate()
  const queryClient = useQueryClient()
  const [notice, setNotice] = useState((location.state as { notice?: string } | null)?.notice ?? '')
  const [actionError, setActionError] = useState('')
  const [busy, setBusy] = useState(false)
  const [confirmCancel, setConfirmCancel] = useState(false)
  const [confirmRollback, setConfirmRollback] = useState(false)

  const { data: deployment, isLoading, error } = useQuery({
    queryKey: ['deployment', id],
    queryFn: () => deployTasksAPI.deployment(id),
    enabled: id !== '',
    retry: false,
    refetchInterval: (query) => {
      const status = query.state.data?.status
      return status && !isDeploymentTerminal(status) ? EVENT_POLL_MS : false
    },
  })

  const projectsQuery = useQuery({
    queryKey: ['projects'],
    queryFn: () => projectsAPI.projects(100, 0),
    staleTime: 60_000,
  })
  const rollbackOf = deployment?.rollbackOf ?? ''
  const { data: rollbackSource } = useQuery({
    queryKey: ['deployment', rollbackOf],
    queryFn: () => deployTasksAPI.deployment(rollbackOf),
    enabled: rollbackOf !== '',
    retry: false,
    staleTime: 60_000,
  })

  const terminal = deployment ? isDeploymentTerminal(deployment.status) : false
  const events = useDeploymentEvents(id, terminal, Boolean(deployment))
  const endRef = useRef<HTMLDivElement>(null)
  useEffect(() => {
    endRef.current?.scrollIntoView({ block: 'nearest' })
  }, [events.lines.length])

  const projectName = projectsQuery.data?.items.find((p) => p.id === deployment?.projectId)?.name

  const cancel = async () => {
    if (!deployment) return
    setActionError('')
    setBusy(true)
    try {
      await deployTasksAPI.cancel(deployment.id)
      setConfirmCancel(false)
      setNotice('已取消该部署')
      await queryClient.invalidateQueries({ queryKey: ['deployment'] })
      await queryClient.invalidateQueries({ queryKey: ['deployments'] })
    } catch (cause) {
      setActionError(deployError(cause, '取消'))
    } finally {
      setBusy(false)
    }
  }

  const rollback = async () => {
    if (!deployment) return
    setActionError('')
    setBusy(true)
    try {
      const next = await deployTasksAPI.rollback(deployment.id)
      setConfirmRollback(false)
      await queryClient.invalidateQueries({ queryKey: ['deployments'] })
      nav(`/deploys/${next.id}`)
    } catch (cause) {
      setActionError(deployError(cause, '回滚'))
    } finally {
      setBusy(false)
    }
  }

  if (error && !deployment) {
    const status = error instanceof ApiError ? error.status : 0
    return (
      <>
        <PageHeader title="部署详情" />
        <p role="alert" className="text-body-md text-error py-space-lg">
          {status === 404 ? '未找到该部署' : deployError(error, '读取')}
        </p>
        <Link to="/deploys" className="text-body-md text-primary hover:underline">
          返回部署记录
        </Link>
      </>
    )
  }
  if (isLoading || !deployment) {
    return (
      <>
        <PageHeader title="部署详情" subtitle="加载中..." />
        <p className="text-body-md text-outline py-space-lg">加载中...</p>
      </>
    )
  }

  const canCancel = CANCELABLE.includes(deployment.status)
  const canRollback = ROLLBACKABLE.includes(deployment.status)

  return (
    <>
      <PageHeader
        title={`部署 #${deployment.deploymentNumber}`}
        subtitle={`${projectName ?? deployment.projectId.slice(0, 8)} · ${deployment.version} · ${DEPLOY_ENVIRONMENT_TEXT[deployment.targetEnvironment]}`}
        actions={
          <>
            <StatusBadge tone={DEPLOYMENT_STATUS_TONE[deployment.status]}>
              {DEPLOYMENT_STATUS_TEXT[deployment.status]}
            </StatusBadge>
            {deployment.approvalId && (
              <Link to="/approvals" className="text-body-md text-primary hover:underline">
                查看审批
              </Link>
            )}
            {canCancel && (
              <Button variant="danger" icon="stop_circle" disabled={busy} onClick={() => setConfirmCancel(true)}>
                取消部署
              </Button>
            )}
            {canRollback && (
              <Button icon="restart_alt" disabled={busy} onClick={() => setConfirmRollback(true)}>
                回滚
              </Button>
            )}
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
          <MetaRow label="项目">
            <span title={deployment.projectId}>{projectName ?? deployment.projectId.slice(0, 8)}</span>
          </MetaRow>
          <MetaRow label="目标">
            <span className="font-mono break-all" title={deployment.targetEndpoint}>
              {deployment.targetName}
            </span>
          </MetaRow>
          <MetaRow label="环境">
            <StatusBadge tone={DEPLOY_ENVIRONMENT_TONE[deployment.targetEnvironment]} dot={false}>
              {DEPLOY_ENVIRONMENT_TEXT[deployment.targetEnvironment]}
            </StatusBadge>
          </MetaRow>
          <MetaRow label="版本">
            <span className="font-mono">{deployment.version}</span>
          </MetaRow>
          <MetaRow label="触发人">
            <span className="font-mono" title={deployment.triggeredBy}>
              {deployment.triggeredBy.slice(0, 8)}
            </span>
          </MetaRow>
          {deployment.approvalId && (
            <MetaRow label="审批 ID">
              <span className="font-mono" title={deployment.approvalId}>
                {deployment.approvalId.slice(0, 8)}
              </span>
            </MetaRow>
          )}
          {deployment.rollbackOf && (
            <MetaRow label="回滚自">
              <Link to={`/deploys/${deployment.rollbackOf}`} className="text-primary hover:underline">
                回滚自 #{rollbackSource?.deploymentNumber ?? deployment.rollbackOf.slice(0, 8)}
              </Link>
            </MetaRow>
          )}
          {deployment.buildId && (
            <MetaRow label="构建">
              <span className="font-mono" title={deployment.buildId}>
                {deployment.buildId.slice(0, 8)}
              </span>
            </MetaRow>
          )}
          <MetaRow label="创建时间">{fmtRel(deployment.createdAt * 1000)}</MetaRow>
          <MetaRow label="开始时间">{deployment.startedAt ? fmtRel(deployment.startedAt * 1000) : '—'}</MetaRow>
          <MetaRow label="结束时间">{deployment.finishedAt ? fmtRel(deployment.finishedAt * 1000) : '—'}</MetaRow>
          {deployment.detail && (
            <MetaRow label="详情">
              <span className="break-all">{deployment.detail}</span>
            </MetaRow>
          )}
          <p className="text-label-sm text-outline pt-space-md border-t border-surface-container-highest">
            目标是创建部署时的快照；之后修改目标不影响本次记录。
          </p>
        </aside>

        <section className="bg-surface-container rounded-lg border border-surface-container-highest p-space-xl self-start">
          <div className="flex items-center justify-between mb-space-md">
            <h2 className="text-headline-md font-semibold">事件</h2>
            <span className="text-label-sm text-outline">
              {terminal ? '部署已结束' : '每 1.5 秒轮询'}
            </span>
          </div>
          <div className="bg-surface-container-lowest border border-surface-container-highest rounded max-h-[460px] overflow-y-auto p-space-lg font-mono text-body-sm">
            {events.lines.length === 0 && !events.error ? (
              <p className="text-outline">等待事件...</p>
            ) : (
              events.lines.map((line) => (
                <div key={line.id} className="whitespace-pre-wrap break-all leading-relaxed">
                  <span className="text-outline">[{line.stream}]</span>{' '}
                  <span className={line.stream === 'system' ? 'text-on-surface-variant' : 'text-on-surface'}>
                    {line.message}
                  </span>
                </div>
              ))
            )}
            <div ref={endRef} />
          </div>
          {events.error && (
            <p role="alert" className="mt-space-md text-body-sm text-error">
              {events.error}
            </p>
          )}
        </section>
      </div>

      <ConfirmDialog
        open={confirmCancel}
        title={`取消部署 #${deployment.deploymentNumber}`}
        danger
        confirmText="确认取消"
        onConfirm={() => void cancel()}
        onCancel={() => setConfirmCancel(false)}
      >
        取消后无法恢复；若部署正在等待审批，对应审批会同时关闭。
      </ConfirmDialog>
      <ConfirmDialog
        open={confirmRollback}
        title={`回滚部署 #${deployment.deploymentNumber}`}
        confirmText="确认回滚"
        onConfirm={() => void rollback()}
        onCancel={() => setConfirmRollback(false)}
      >
        回滚会创建一个新的部署，使用同一项目与目标上更早的最近成功版本；原部署记录保持不变。
      </ConfirmDialog>
    </>
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

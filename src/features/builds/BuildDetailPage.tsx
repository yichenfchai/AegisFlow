import { useEffect, useRef, useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { Link, useParams } from 'react-router'
import { Button, PageHeader } from '@/components/ui/DataTable'
import { StatusBadge } from '@/components/ui/Badge'
import { LogViewer } from '@/components/domain/LogViewer'
import {
  buildsAPI,
  BUILD_STATUS_TEXT,
  BUILD_STATUS_TONE,
  type BuildStatus,
} from '@/api/builds'
import { projectsAPI } from '@/api/projects'
import { ApiError } from '@/api/http'
import { fmtRel } from '@/api/format'
import { useSession } from '@/stores/session'
import type { LogLine } from '@/api/types'

const TERMINAL_STATUSES: BuildStatus[] = ['succeeded', 'failed', 'cancelled', 'interrupted']
/** 未结束的构建（排队/运行）用短周期刷新；终态停止。 */
const STATUS_POLL_MS = 1500
const LOG_POLL_MS = 1000
/**
 * 终态先落库、最后一批日志随后 flush（后端用独立的 5s 背景上下文），
 * 所以状态结束后仍以固定上限继续读一段日志，避免丢掉收尾行。
 */
const LOG_TAIL_GRACE_MS = 6000

const isTerminal = (status: BuildStatus) => TERMINAL_STATUSES.includes(status)

function opError(cause: unknown): string {
  if (cause instanceof ApiError) {
    if (cause.status === 409) return '构建已结束，无法取消'
    if (cause.status === 403) return '当前角色没有取消构建的权限'
    if (cause.status === 404) return '构建不存在或已被清理'
    if (cause.status === 0) return '无法连接服务，请检查网络后重试'
  }
  return cause instanceof Error ? cause.message : '操作失败，请稍后重试'
}

const emptyLogs: LogLine[] = []

interface LogState {
  buildID: string
  lines: LogLine[]
  error: string
}

/**
 * 按游标累积构建日志：after 取上一条事件 id，只追加新事件，不整页重刷。
 * cursor 用 ref 保存，轮询循环不因状态变化重启；终态后继续读满宽限期。
 */
function useBuildLogs(buildID: string, terminal: boolean) {
  const [state, setState] = useState<LogState>({ buildID, lines: emptyLogs, error: '' })
  const cursor = useRef(0)
  const terminalRef = useRef(terminal)

  useEffect(() => {
    terminalRef.current = terminal
  }, [terminal])

  useEffect(() => {
    let cancelled = false
    let timer: ReturnType<typeof setTimeout> | undefined
    let tailDeadline = 0
    cursor.current = 0

    const poll = async () => {
      try {
        const page = await buildsAPI.logs(buildID, cursor.current)
        if (cancelled) return
        if (page.items.length > 0) cursor.current = page.items[page.items.length - 1].id
        setState((prev) => {
          // 切换构建时直接以空列表重建，不依赖 effect 里的同步重置。
          const lines = prev.buildID === buildID ? prev.lines : emptyLogs
          const added = page.items.map((event, index) => ({
            seq: lines.length + index + 1,
            stream: event.stream,
            content: event.message,
            ts: event.createdAt,
          }))
          return { buildID, lines: [...lines, ...added], error: '' }
        })
      } catch (cause) {
        if (cancelled) return
        setState((prev) => ({
          buildID,
          lines: prev.buildID === buildID ? prev.lines : emptyLogs,
          error: cause instanceof Error ? cause.message : '日志加载失败',
        }))
      }
      if (cancelled) return
      if (!terminalRef.current) {
        tailDeadline = 0
      } else {
        if (tailDeadline === 0) tailDeadline = Date.now() + LOG_TAIL_GRACE_MS
        if (Date.now() >= tailDeadline) return
      }
      timer = setTimeout(poll, LOG_POLL_MS)
    }

    void poll()
    return () => {
      cancelled = true
      if (timer) clearTimeout(timer)
    }
  }, [buildID])

  return state.buildID === buildID
    ? { lines: state.lines, error: state.error }
    : { lines: emptyLogs, error: '' }
}

/** 构建详情（真实接口）：状态、命令快照、触发信息与游标累积的日志流。 */
export function BuildDetailPage() {
  const { id = '' } = useParams<{ id: string }>()
  const { user } = useSession()
  const canWrite = user?.role === 'admin' || user?.role === 'member'
  const queryClient = useQueryClient()
  const [actionError, setActionError] = useState('')
  const [cancelling, setCancelling] = useState(false)

  const { data: build, isLoading, error } = useQuery({
    queryKey: ['build', id],
    queryFn: () => buildsAPI.build(id),
    enabled: id !== '',
    refetchInterval: (query) => {
      const status = query.state.data?.status
      return status && !isTerminal(status) ? STATUS_POLL_MS : false
    },
  })
  const projectID = build?.projectId
  const { data: project } = useQuery({
    queryKey: ['project', projectID],
    queryFn: () => {
      if (!projectID) throw new Error('missing project id')
      return projectsAPI.project(projectID)
    },
    enabled: Boolean(projectID),
    staleTime: 60_000,
  })

  const terminal = build ? isTerminal(build.status) : false
  const { lines, error: logError } = useBuildLogs(id, terminal)

  const cancel = async () => {
    if (!build) return
    setActionError('')
    setCancelling(true)
    try {
      await buildsAPI.cancel(build.id)
      await queryClient.invalidateQueries({ queryKey: ['build', build.id] })
      await queryClient.invalidateQueries({ queryKey: ['builds'] })
    } catch (cause) {
      setActionError(opError(cause))
    } finally {
      setCancelling(false)
    }
  }

  if (error && !build) {
    return (
      <>
        <PageHeader title="构建详情" />
        <p role="alert" className="text-body-md text-error py-space-lg">
          {opError(error)}
        </p>
        <Link to="/builds" className="text-body-md text-primary hover:underline">
          返回构建列表
        </Link>
      </>
    )
  }
  if (isLoading || !build) {
    return (
      <>
        <PageHeader title="构建详情" subtitle="加载中..." />
        <p className="text-body-md text-outline py-space-lg">加载中...</p>
      </>
    )
  }

  const duration =
    build.startedAt && build.finishedAt ? `${build.finishedAt - build.startedAt}s` : '—'

  return (
    <>
      <PageHeader
        title={`构建 #${build.buildNumber}`}
        subtitle={`${project?.name ?? build.projectId.slice(0, 8)} · 分支 ${build.branch || '—'} · ${fmtRel(build.createdAt * 1000)}`}
        actions={
          <>
            <StatusBadge tone={BUILD_STATUS_TONE[build.status]}>{BUILD_STATUS_TEXT[build.status]}</StatusBadge>
            {build.status === 'queued' || build.status === 'running' ? (
              canWrite ? (
                <Button variant="danger" icon="stop_circle" disabled={cancelling} onClick={() => void cancel()}>
                  {cancelling ? '取消中...' : '取消构建'}
                </Button>
              ) : null
            ) : null}
          </>
        }
      />

      {actionError && (
        <p role="alert" className="mb-space-md text-body-sm text-error bg-error/10 rounded px-space-lg py-space-md">
          {actionError}
        </p>
      )}

      <div className="grid grid-cols-[340px_1fr] gap-gutter">
        <div className="bg-surface-container rounded-lg border border-surface-container-highest p-space-xl space-y-space-lg self-start">
          <MetaRow label="项目">
            <Link to="/" className="hover:underline">
              {project?.name ?? build.projectId.slice(0, 8)}
            </Link>
          </MetaRow>
          <MetaRow label="分支">
            <span className="font-mono">{build.branch || '—'}</span>
          </MetaRow>
          <MetaRow label="构建命令">
            <span className="font-mono break-all">{build.command}</span>
          </MetaRow>
          <MetaRow label="触发者">
            <span className="font-mono" title={build.triggeredBy}>
              {build.triggeredBy.slice(0, 8)}
            </span>
          </MetaRow>
          <MetaRow label="触发时间">{fmtRel(build.createdAt * 1000)}</MetaRow>
          <MetaRow label="开始时间">{build.startedAt ? fmtRel(build.startedAt * 1000) : '—'}</MetaRow>
          <MetaRow label="结束时间">{build.finishedAt ? fmtRel(build.finishedAt * 1000) : '—'}</MetaRow>
          <MetaRow label="耗时">{duration}</MetaRow>
          <p className="text-label-sm text-outline pt-space-md border-t border-surface-container-highest">
            命令快照在触发时固化，之后修改项目命令不影响本次构建。
          </p>
        </div>

        <div className="space-y-space-sm">
          <LogViewer lines={lines} running={!terminal} title={build.command} maxHeight={560} />
          {logError && (
            <p role="alert" className="text-body-sm text-error">
              {logError}
            </p>
          )}
          {lines.length === 0 && !logError && (
            <p className="text-body-sm text-outline">
              {build.status === 'queued' ? '构建排队中，日志会在开始执行后出现。' : '暂时没有日志输出。'}
            </p>
          )}
        </div>
      </div>
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

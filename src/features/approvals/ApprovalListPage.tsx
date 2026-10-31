import { useState } from 'react'
import { Link } from 'react-router'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import {
  Button,
  ConfirmDialog,
  DataTable,
  Field,
  Input,
  PageHeader,
  Pagination,
  Select,
  type Column,
} from '@/components/ui/DataTable'
import { StatusBadge } from '@/components/ui/Badge'
import {
  approvalsAPI,
  APPROVAL_STATUS_TEXT,
  APPROVAL_STATUS_TONE,
  type Approval,
  type ApprovalStatus,
} from '@/api/deployTasks'
import { projectsAPI } from '@/api/projects'
import { ApiError } from '@/api/http'
import { fmtRel } from '@/api/format'

const PAGE_SIZE = 12
const STATUSES: ApprovalStatus[] = ['pending', 'approved', 'rejected', 'expired']

function byteLength(value: string): number {
  return new TextEncoder().encode(value).length
}

function approvalError(cause: unknown): string {
  if (cause instanceof ApiError) {
    if (cause.status === 400) return '拒绝理由不合法：需为单行且最多 512 字节'
    if (cause.status === 403) return '当前角色没有该操作权限'
    if (cause.status === 404) return '审批不存在或已被删除'
    if (cause.status === 409) return '审批已被处理，或部署状态已变化，请刷新后重试'
    if (cause.status === 503) return '服务依赖不可用，请稍后重试'
    if (cause.status === 0) return '无法连接服务，请检查网络后重试'
  }
  return cause instanceof Error ? cause.message : '操作失败，请稍后重试'
}

/** 审批中心（真实接口）：生产部署的审批队列，仅待审批可操作。 */
export function ApprovalListPage() {
  const queryClient = useQueryClient()
  const [page, setPage] = useState(1)
  const [status, setStatus] = useState<ApprovalStatus | ''>('')
  const { data, isLoading, error } = useQuery({
    queryKey: ['approvals', status, page],
    queryFn: () => approvalsAPI.approvals(PAGE_SIZE, (page - 1) * PAGE_SIZE, status || undefined),
  })
  const projectsQuery = useQuery({
    queryKey: ['projects'],
    queryFn: () => projectsAPI.projects(100, 0),
    staleTime: 60_000,
  })
  const projects = projectsQuery.data?.items ?? []

  const [approveTarget, setApproveTarget] = useState<Approval | null>(null)
  const [rejectTarget, setRejectTarget] = useState<Approval | null>(null)
  const [reason, setReason] = useState('')
  const [actionError, setActionError] = useState('')
  const [notice, setNotice] = useState('')
  const [busy, setBusy] = useState(false)

  const refresh = async () => {
    await queryClient.invalidateQueries({ queryKey: ['approvals'] })
    await queryClient.invalidateQueries({ queryKey: ['deployment'] })
    await queryClient.invalidateQueries({ queryKey: ['deployments'] })
  }

  const approve = async () => {
    const target = approveTarget
    if (!target) return
    setActionError('')
    setBusy(true)
    try {
      await approvalsAPI.approve(target.id)
      setApproveTarget(null)
      setNotice('已批准该审批，部署将进入队列执行')
      await refresh()
    } catch (cause) {
      setActionError(approvalError(cause))
    } finally {
      setBusy(false)
    }
  }

  const reject = async () => {
    const target = rejectTarget
    if (!target) return
    setActionError('')
    const trimmed = reason.trim()
    if (/[\r\n]/.test(reason)) {
      setActionError('拒绝理由需为单行')
      return
    }
    if (byteLength(trimmed) > 512) {
      setActionError('拒绝理由最多 512 字节')
      return
    }
    setBusy(true)
    try {
      await approvalsAPI.reject(target.id, trimmed || undefined)
      setRejectTarget(null)
      setReason('')
      setNotice('已拒绝该审批')
      await refresh()
    } catch (cause) {
      setActionError(approvalError(cause))
    } finally {
      setBusy(false)
    }
  }

  const projectName = (id: string) => projects.find((p) => p.id === id)?.name ?? id.slice(0, 8)

  const columns: Column<Approval>[] = [
    {
      key: 'deployment',
      header: '部署',
      render: (a) => (
        <Link
          to={`/deploys/${a.deploymentId}`}
          className="font-mono text-primary hover:underline"
          title={a.deploymentId}
        >
          #{a.deploymentId.slice(0, 8)}
        </Link>
      ),
    },
    { key: 'project', header: '项目', render: (a) => <span title={a.projectId}>{projectName(a.projectId)}</span> },
    {
      key: 'status',
      header: '状态',
      render: (a) => <StatusBadge tone={APPROVAL_STATUS_TONE[a.status]}>{APPROVAL_STATUS_TEXT[a.status]}</StatusBadge>,
    },
    { key: 'createdAt', header: '申请时间', render: (a) => fmtRel(a.createdAt * 1000) },
    {
      key: 'expiresAt',
      header: '过期时间',
      render: (a) => (a.expiresAt ? new Date(a.expiresAt * 1000).toLocaleString() : '—'),
    },
    {
      key: 'decision',
      header: '决定人 / 理由',
      render: (a) => (
        <div>
          <div className="font-mono text-label-sm text-outline" title={a.decidedBy}>
            {a.decidedBy ? a.decidedBy.slice(0, 8) : '—'}
          </div>
          {a.reason && <div className="text-label-sm text-on-surface-variant break-all">{a.reason}</div>}
        </div>
      ),
    },
    {
      key: 'actions',
      header: '操作',
      align: 'right',
      render: (a) =>
        a.status === 'pending' ? (
          <div className="flex items-center justify-end gap-space-sm">
            <Button variant="secondary" size="sm" disabled={busy} onClick={() => setApproveTarget(a)}>
              批准
            </Button>
            <Button
              variant="danger"
              size="sm"
              disabled={busy}
              onClick={() => {
                setReason('')
                setRejectTarget(a)
              }}
            >
              拒绝
            </Button>
          </div>
        ) : (
          <span className="text-label-sm text-outline">—</span>
        ),
    },
  ]

  return (
    <>
      <PageHeader title="审批中心" subtitle="生产部署的审批请求；仅待审批的请求可以批准或拒绝" />
      <div className="mb-space-lg flex flex-wrap items-end gap-space-md">
        <Field label="状态" htmlFor="approval-status" className="w-[160px]">
          <Select
            id="approval-status"
            value={status}
            onChange={(e) => {
              setStatus(e.target.value as ApprovalStatus | '')
              setPage(1)
            }}
          >
            <option value="">全部</option>
            {STATUSES.map((value) => (
              <option key={value} value={value}>
                {APPROVAL_STATUS_TEXT[value]}
              </option>
            ))}
          </Select>
        </Field>
      </div>
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
      {error ? (
        <p role="alert" className="text-body-md text-error py-space-lg">
          {error instanceof Error ? error.message : '加载审批失败'}
        </p>
      ) : isLoading ? (
        <p className="text-body-md text-outline py-space-lg">加载中...</p>
      ) : (
        <>
          <DataTable columns={columns} rows={data?.items ?? []} empty="没有待处理的审批" />
          <Pagination page={page} pageSize={PAGE_SIZE} total={data?.total ?? 0} onPage={setPage} />
        </>
      )}

      <ConfirmDialog
        open={approveTarget !== null}
        title="批准部署审批"
        confirmText="确认批准"
        onConfirm={() => void approve()}
        onCancel={() => setApproveTarget(null)}
      >
        批准后部署会进入队列并开始执行；已批准不可撤销。
      </ConfirmDialog>
      <ConfirmDialog
        open={rejectTarget !== null}
        title="拒绝部署审批"
        danger
        confirmText="确认拒绝"
        onConfirm={() => void reject()}
        onCancel={() => {
          setRejectTarget(null)
          setReason('')
        }}
      >
        <p className="mb-space-md">拒绝后对应部署直接结束为已拒绝，不能再次审批。</p>
        <Field label="拒绝理由" hint="选填，单行，最多 512 字节" htmlFor="approval-reason">
          <Input
            id="approval-reason"
            value={reason}
            maxLength={512}
            onChange={(e) => setReason(e.target.value)}
            placeholder="例如：变更窗口已关闭"
          />
        </Field>
      </ConfirmDialog>
    </>
  )
}

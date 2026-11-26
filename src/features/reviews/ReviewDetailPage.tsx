import { useState } from 'react'
import { Link, useLocation, useParams } from 'react-router'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { Button, ConfirmDialog, Field, Input, PageHeader } from '@/components/ui/DataTable'
import { StatusBadge } from '@/components/ui/Badge'
import { REVIEW_STATUS_TEXT, REVIEW_STATUS_TONE, REVIEW_VERDICT_TEXT, REVIEW_VERDICT_TONE, reviewsAPI, type Review } from '@/api/reviews'
import { projectsAPI } from '@/api/projects'
import { ApiError } from '@/api/http'
import { errorText } from '@/api/errors'
import { fmtRel } from '@/api/format'
import { useSession } from '@/stores/session'
import { reviewActionError, reviewReasonValidation } from './reviewForm'

/**
 * 审查详情（真实接口）：展示审查正文、状态、决定人与理由、外部链接，以及外部
 * 工具签名回写的只读结果（E4B）；待审查时按角色展示批准/拒绝（维护者或管理员）
 * 与取消（发起人、维护者或管理员）。按钮只是交互镜像，服务端逐请求校验。
 */
export function ReviewDetailPage() {
  const { id = '' } = useParams<{ id: string }>()
  const location = useLocation()
  const queryClient = useQueryClient()
  const { user } = useSession()
  const [notice, setNotice] = useState((location.state as { notice?: string } | null)?.notice ?? '')
  const [approveOpen, setApproveOpen] = useState(false)
  const [rejectOpen, setRejectOpen] = useState(false)
  const [cancelOpen, setCancelOpen] = useState(false)
  const [approveReason, setApproveReason] = useState('')
  const [rejectReason, setRejectReason] = useState('')
  const [approveError, setApproveError] = useState('')
  const [rejectError, setRejectError] = useState('')
  const [cancelError, setCancelError] = useState('')
  const [busy, setBusy] = useState(false)

  const reviewQuery = useQuery({
    queryKey: ['review', id],
    queryFn: () => reviewsAPI.get(id),
    enabled: id !== '',
    retry: false,
  })
  const review = reviewQuery.data
  const projectId = review?.projectId ?? ''
  // 外部结果是只读证据；独立查询失败不影响审查本体与决策交互。
  const externalResultsQuery = useQuery({
    queryKey: ['review-external-results', id],
    queryFn: () => reviewsAPI.listExternalResults(id),
    enabled: id !== '',
    retry: false,
  })
  const externalResults = externalResultsQuery.data?.items ?? []
  const projectsQuery = useQuery({
    queryKey: ['projects'],
    queryFn: () => projectsAPI.projects(100, 0),
    staleTime: 60_000,
  })
  // 项目成员用于判断「当前 member 是否该项目的维护者」；管理员不需要，viewer 永远不能决策。
  const membersQuery = useQuery({
    queryKey: ['project-members', projectId],
    queryFn: () => projectsAPI.members(projectId),
    enabled: projectId !== '' && review?.status === 'pending' && user?.role === 'member',
    retry: false,
  })

  const isAdmin = user?.role === 'admin'
  const myProjectRole = membersQuery.data?.find((member) => member.userId === user?.id)?.role
  const canDecide = isAdmin || (user?.role === 'member' && myProjectRole === 'maintainer')
  const canCancel =
    review?.status === 'pending' && (isAdmin || myProjectRole === 'maintainer' || review.requestedBy === user?.id)
  const projectName = projectsQuery.data?.items.find((project) => project.id === projectId)?.name

  const applyDecision = async (action: '批准' | '拒绝' | '取消', run: () => Promise<Review>, success: string) => {
    if (busy) return
    setBusy(true)
    try {
      const updated = await run()
      queryClient.setQueryData(['review', id], updated)
      setApproveOpen(false)
      setRejectOpen(false)
      setCancelOpen(false)
      setApproveReason('')
      setRejectReason('')
      setNotice(success)
      await queryClient.invalidateQueries({ queryKey: ['reviews'] })
    } catch (cause) {
      const failure = reviewActionError(cause, action)
      if (action === '批准') setApproveError(failure)
      else if (action === '拒绝') setRejectError(failure)
      else setCancelError(failure)
    } finally {
      setBusy(false)
    }
  }

  const approve = async () => {
    if (!review) return
    setApproveError('')
    const invalid = reviewReasonValidation(approveReason, false)
    if (invalid) {
      setApproveError(invalid)
      return
    }
    await applyDecision('批准', () => reviewsAPI.approve(review.id, approveReason.trim() || undefined), '已批准该审查')
  }

  const reject = async () => {
    if (!review) return
    setRejectError('')
    const invalid = reviewReasonValidation(rejectReason, true)
    if (invalid) {
      setRejectError(invalid)
      return
    }
    await applyDecision('拒绝', () => reviewsAPI.reject(review.id, rejectReason.trim()), '已拒绝该审查')
  }

  const cancel = async () => {
    if (!review) return
    setCancelError('')
    await applyDecision('取消', () => reviewsAPI.cancel(review.id), '已取消该审查')
  }

  if (reviewQuery.error && !review) {
    const status = reviewQuery.error instanceof ApiError ? reviewQuery.error.status : 0
    return (
      <>
        <PageHeader title="审查详情" />
        <p role="alert" className="text-body-md text-error py-space-lg">
          {status === 404 ? '未找到该审查：它可能属于你没有权限查看的项目。' : errorText(reviewQuery.error, '加载审查失败')}
        </p>
        <Link to="/reviews" className="text-body-md text-primary hover:underline">
          返回审查中心
        </Link>
      </>
    )
  }
  if (reviewQuery.isLoading || !review) {
    return (
      <>
        <PageHeader title="审查详情" subtitle="加载中..." />
        <p className="text-body-md text-outline py-space-lg">加载中...</p>
      </>
    )
  }

  return (
    <>
      <PageHeader
        title={`审查 #${review.number}`}
        subtitle={`${projectName ?? projectId.slice(0, 8)} · ${review.subject}`}
        actions={
          <>
            <StatusBadge tone={REVIEW_STATUS_TONE[review.status]}>{REVIEW_STATUS_TEXT[review.status]}</StatusBadge>
            {review.status === 'pending' && canDecide && (
              <>
                <Button
                  variant="secondary"
                  icon="done"
                  disabled={busy}
                  onClick={() => {
                    setApproveError('')
                    setApproveReason('')
                    setApproveOpen(true)
                  }}
                >
                  批准
                </Button>
                <Button
                  variant="danger"
                  icon="error"
                  disabled={busy}
                  onClick={() => {
                    setRejectError('')
                    setRejectReason('')
                    setRejectOpen(true)
                  }}
                >
                  拒绝
                </Button>
              </>
            )}
            {canCancel && (
              <Button
                variant="ghost"
                icon="stop_circle"
                disabled={busy}
                onClick={() => {
                  setCancelError('')
                  setCancelOpen(true)
                }}
              >
                取消审查
              </Button>
            )}
            <Link to="/reviews" className="text-body-md text-primary hover:underline">
              返回审查中心
            </Link>
          </>
        }
      />
      {notice && (
        <p role="status" className="mb-space-md text-body-sm text-success bg-success/10 rounded px-space-lg py-space-md">
          {notice}
        </p>
      )}

      <div className="grid grid-cols-[340px_1fr] gap-gutter">
        <aside className="bg-surface-container rounded-lg border border-surface-container-highest p-space-xl space-y-space-lg self-start">
          <MetaRow label="项目">
            <span title={review.projectId}>{projectName ?? review.projectId.slice(0, 8)}</span>
          </MetaRow>
          <MetaRow label="发起人">
            <span title={review.requestedBy}>{review.requestedByName || review.requestedBy.slice(0, 8)}</span>
          </MetaRow>
          <MetaRow label="版本">
            <span className="font-mono">{review.version || '—'}</span>
          </MetaRow>
          <MetaRow label="发起时间">{fmtRel(review.createdAt * 1000)}</MetaRow>
          <MetaRow label="更新时间">{fmtRel(review.updatedAt * 1000)}</MetaRow>
          {review.decidedAt ? (
            <>
              <MetaRow label={review.status === 'cancelled' ? '取消人' : '决定人'}>
                <span title={review.decidedBy}>
                  {review.decidedByName || review.decidedBy?.slice(0, 8) || '—'}
                </span>
              </MetaRow>
              <MetaRow label={review.status === 'cancelled' ? '取消时间' : '决定时间'}>
                {new Date(review.decidedAt * 1000).toLocaleString()}
              </MetaRow>
              <MetaRow label={review.status === 'rejected' ? '拒绝理由' : '理由'}>
                <span className="break-all whitespace-pre-wrap">{review.reason || '—'}</span>
              </MetaRow>
            </>
          ) : (
            <p className="text-label-sm text-outline pt-space-md border-t border-surface-container-highest">
              该审查仍在等待决定；批准或拒绝后记录决定人与理由。
            </p>
          )}
        </aside>

        <section className="bg-surface-container rounded-lg border border-surface-container-highest p-space-xl self-start space-y-space-lg">
          <div>
            <h2 className="text-headline-md font-semibold mb-space-md">审查内容</h2>
            <p className="text-body-md text-on-surface whitespace-pre-wrap break-all">{review.subject}</p>
          </div>
          <div>
            <h3 className="text-label-md text-outline mb-space-sm">描述</h3>
            <p className="text-body-md text-on-surface-variant whitespace-pre-wrap break-all">
              {review.description || '（未填写描述）'}
            </p>
          </div>
          <div>
            <h3 className="text-label-md text-outline mb-space-sm">外部链接</h3>
            {review.externalUrl ? (
              <div>
                <a
                  href={review.externalUrl}
                  target="_blank"
                  rel="noopener noreferrer"
                  className="text-body-md text-primary hover:underline"
                >
                  在外部系统查看
                </a>
                <div className="font-mono text-label-sm text-outline break-all mt-space-xs">{review.externalUrl}</div>
                <p className="text-label-sm text-outline mt-space-xs">仅作展示引用；AegisFlow 不会抓取该地址。</p>
              </div>
            ) : (
              <p className="text-body-md text-outline">—</p>
            )}
          </div>
        </section>
      </div>

      <section
        aria-labelledby="review-external-results-heading"
        className="mt-gutter bg-surface-container rounded-lg border border-surface-container-highest p-space-xl"
      >
        <div className="flex flex-wrap items-end justify-between gap-space-md mb-space-lg">
          <div>
            <h2 id="review-external-results-heading" className="text-headline-md font-semibold">
              外部结果
            </h2>
            <p className="text-label-sm text-outline mt-space-xs">
              外部扫描/审查工具通过签名接口回写的只读证据，不会改变审查状态；AegisFlow 不会抓取报告地址。
            </p>
          </div>
          {externalResults.length > 0 && (
            <span className="text-label-sm text-outline">共 {externalResults.length} 条 · 最新在前</span>
          )}
        </div>
        {externalResultsQuery.isPending ? (
          <div role="status">
            <span className="sr-only">正在加载外部结果</span>
            <div aria-hidden className="space-y-space-md">
              {[0, 1].map((row) => (
                <div
                  key={row}
                  className="animate-pulse rounded border border-surface-container-highest bg-surface-container-low p-space-lg"
                >
                  <div className="h-4 w-32 rounded bg-surface-container-highest/70" />
                  <div className="mt-space-md h-3 w-3/4 rounded bg-surface-container-highest/70" />
                </div>
              ))}
            </div>
          </div>
        ) : externalResultsQuery.error ? (
          <div className="rounded border border-error/40 bg-error/5 p-space-lg">
            <p role="alert" className="text-body-sm text-error">
              {errorText(externalResultsQuery.error, '加载外部结果失败，请稍后重试')}
            </p>
            <Button
              variant="secondary"
              size="sm"
              className="mt-space-md"
              disabled={externalResultsQuery.isFetching}
              onClick={() => void externalResultsQuery.refetch()}
            >
              {externalResultsQuery.isFetching ? '正在重试...' : '重试加载外部结果'}
            </Button>
          </div>
        ) : externalResults.length === 0 ? (
          <p className="py-space-lg text-body-md text-outline">暂无外部结果</p>
        ) : (
          <ul className="space-y-space-md">
            {externalResults.map((result) => (
              <li
                key={result.id}
                className="rounded border border-surface-container-highest bg-surface-container-low p-space-lg"
              >
                <div className="flex flex-wrap items-center gap-space-md">
                  <StatusBadge tone={REVIEW_VERDICT_TONE[result.verdict]}>
                    {REVIEW_VERDICT_TEXT[result.verdict]}
                  </StatusBadge>
                  <span className="break-all text-body-md font-medium text-on-surface">{result.source}</span>
                  <span
                    className="ml-auto text-label-sm text-outline"
                    title={new Date(result.receivedAt * 1000).toLocaleString()}
                  >
                    {fmtRel(result.receivedAt * 1000)}
                  </span>
                </div>
                {result.summary ? (
                  <p className="mt-space-md max-h-40 overflow-y-auto whitespace-pre-wrap break-all text-body-sm text-on-surface-variant">
                    {result.summary}
                  </p>
                ) : (
                  <p className="mt-space-md text-body-sm text-outline">（未提供摘要）</p>
                )}
                {result.url && (
                  <div className="mt-space-md">
                    <a
                      href={result.url}
                      target="_blank"
                      rel="noopener noreferrer"
                      className="text-body-sm text-primary hover:underline"
                    >
                      查看外部报告
                    </a>
                    <div className="mt-space-xs break-all font-mono text-label-sm text-outline">{result.url}</div>
                  </div>
                )}
              </li>
            ))}
          </ul>
        )}
      </section>

      <ConfirmDialog
        open={approveOpen}
        title={`批准审查 #${review.number}`}
        confirmText="确认批准"
        onConfirm={() => void approve()}
        onCancel={() => setApproveOpen(false)}
      >
        <p className="mb-space-md">批准后审查进入终态，可用于启用审查门禁环境的部署，且不可撤销。</p>
        <Field label="批准理由" hint="选填；单行，最多 512 字节" htmlFor="review-approve-reason">
          <Input
            id="review-approve-reason"
            value={approveReason}
            maxLength={512}
            onChange={(event) => setApproveReason(event.target.value)}
            placeholder="例如：变更范围与验证记录已确认"
          />
        </Field>
        {approveError && (
          <p role="alert" className="mt-space-md text-body-sm text-error">
            {approveError}
          </p>
        )}
      </ConfirmDialog>
      <ConfirmDialog
        open={rejectOpen}
        title={`拒绝审查 #${review.number}`}
        danger
        confirmText="确认拒绝"
        onConfirm={() => void reject()}
        onCancel={() => setRejectOpen(false)}
      >
        <p className="mb-space-md">拒绝理由为必填项，会保存在审查记录中并进入审计；拒绝后不可撤销。</p>
        <Field label="拒绝理由" hint="必填；单行，最多 512 字节" htmlFor="review-reject-reason">
          <Input
            id="review-reject-reason"
            value={rejectReason}
            maxLength={512}
            onChange={(event) => setRejectReason(event.target.value)}
            placeholder="例如：缺少回滚方案"
          />
        </Field>
        {rejectError && (
          <p role="alert" className="mt-space-md text-body-sm text-error">
            {rejectError}
          </p>
        )}
      </ConfirmDialog>
      <ConfirmDialog
        open={cancelOpen}
        title={`取消审查 #${review.number}`}
        danger
        confirmText="确认取消"
        onConfirm={() => void cancel()}
        onCancel={() => setCancelOpen(false)}
      >
        <p className="mb-space-md">取消后审查进入终态（已取消），不能再批准或拒绝；记录仍保留在列表中。</p>
        {cancelError && (
          <p role="alert" className="text-body-sm text-error">
            {cancelError}
          </p>
        )}
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

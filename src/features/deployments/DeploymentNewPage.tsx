import { useState } from 'react'
import { useNavigate } from 'react-router'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { Button, Field, Input, PageHeader, Select } from '@/components/ui/DataTable'
import {
  deployTasksAPI,
  listTargets,
  DEPLOY_ENVIRONMENT_TEXT,
  type DeploymentTargetOption,
} from '@/api/deployTasks'
import { projectsAPI } from '@/api/projects'
import { reviewsAPI, reviewOptionLabel, type Review } from '@/api/reviews'
import { ApiError } from '@/api/http'
import { errorText } from '@/api/errors'

const VERSION_PATTERN = /^[A-Za-z0-9._+-]{1,64}$/

function createError(cause: unknown): string {
  if (cause instanceof ApiError) {
    if (cause.status === 400) {
      return '请求不合法：检查版本格式（1-64 位字母、数字、. _ + -）、目标是否启用、构建 ID 是否为本项目的成功构建；若目标环境启用了审查门禁，请先选择一条该项目已批准的审查（没有则先到审查中心发起并等待批准）'
    }
    if (cause.status === 403) return '你在该项目没有部署权限：需要项目维护者或被允许的成员角色'
    if (cause.status === 404) return '项目不存在、对你不可见，或你在该项目没有写权限'
  }
  return errorText(cause, '创建部署失败，请稍后重试')
}

const targetLabel = (target: DeploymentTargetOption) =>
  `${target.name} · ${DEPLOY_ENVIRONMENT_TEXT[target.environment]} · ${target.endpoint}`

/** 新建部署（真实接口）：生产目标会先进入审批。 */
export function DeploymentNewPage() {
  const nav = useNavigate()
  const queryClient = useQueryClient()
  const [projectId, setProjectId] = useState('')
  const [targetId, setTargetId] = useState('')
  const [reviewId, setReviewId] = useState('')
  const [version, setVersion] = useState('')
  const [buildId, setBuildId] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)

  const projectsQuery = useQuery({
    queryKey: ['projects'],
    queryFn: () => projectsAPI.projects(100, 0),
    staleTime: 60_000,
  })
  const targetsQuery = useQuery({
    queryKey: ['deployment-target-options'],
    queryFn: listTargets,
    staleTime: 60_000,
  })
  // 可选「关联网关审查」：仅该项目已批准的审查；门禁环境要求必须关联，默认关闭时可选。
  const approvedReviewsQuery = useQuery({
    queryKey: ['project-reviews', projectId, 'approved'],
    queryFn: () => reviewsAPI.listByProject(projectId, { status: 'approved', limit: 100 }),
    enabled: projectId !== '',
    staleTime: 30_000,
  })
  const projects = projectsQuery.data?.items ?? []
  const targets = targetsQuery.data?.items ?? []
  const approvedReviews: Review[] = approvedReviewsQuery.data?.items ?? []

  const submit = async (e?: React.FormEvent) => {
    e?.preventDefault()
    if (busy) return
    setError('')
    const trimmedVersion = version.trim()
    if (!projectId) {
      setError('请选择项目')
      return
    }
    if (!targetId) {
      setError('请选择部署目标')
      return
    }
    if (!VERSION_PATTERN.test(trimmedVersion)) {
      setError('版本需为 1-64 位字母、数字、. _ + -，例如 v1.2.3')
      return
    }
    setBusy(true)
    try {
      const deployment = await deployTasksAPI.create(projectId, {
        targetId,
        version: trimmedVersion,
        buildId: buildId.trim() || undefined,
        reviewId: reviewId || undefined,
      })
      await queryClient.invalidateQueries({ queryKey: ['deployments'] })
      nav(`/deploys/${deployment.id}`, {
        state:
          deployment.status === 'awaiting_approval'
            ? { notice: '已提交审批，等待管理员批准' }
            : undefined,
      })
    } catch (cause) {
      setError(createError(cause))
    } finally {
      setBusy(false)
    }
  }

  return (
    <>
      <PageHeader title="新建部署" subtitle="从项目与已启用的部署目标创建一次部署；生产环境自动进入审批" />
      <form onSubmit={submit} className="max-w-[640px] space-y-space-xl" aria-busy={busy}>
        <Field label="项目" hint="部署编号按项目分配" htmlFor="deploy-project">
          <Select
            id="deploy-project"
            value={projectId}
            onChange={(e) => {
              setProjectId(e.target.value)
              setReviewId('')
            }}
            disabled={busy || projectsQuery.isLoading}
          >
            <option value="">请选择项目</option>
            {projects.map((p) => (
              <option key={p.id} value={p.id}>
                {p.name}
              </option>
            ))}
          </Select>
        </Field>
        <Field
          label="关联网关审查"
          hint="选填；只列出该项目已批准的审查。目标环境启用审查门禁时必须关联"
          htmlFor="deploy-review"
        >
          <Select
            id="deploy-review"
            value={reviewId}
            onChange={(e) => setReviewId(e.target.value)}
            disabled={busy || !projectId || approvedReviewsQuery.isLoading}
          >
            <option value="">不关联审查</option>
            {approvedReviews.map((review) => (
              <option key={review.id} value={review.id}>
                {reviewOptionLabel(review)}
              </option>
            ))}
          </Select>
          {projectId !== '' && !approvedReviewsQuery.isLoading && approvedReviews.length === 0 && (
            <p className="text-label-sm text-warning mt-space-xs">
              该项目还没有已批准的审查；启用审查门禁的环境需要先到审查中心发起并批准一条审查。
            </p>
          )}
        </Field>
        <Field label="目标" hint="只列出启用中的目标；凭据不会回显" htmlFor="deploy-target">
          <Select
            id="deploy-target"
            value={targetId}
            onChange={(e) => setTargetId(e.target.value)}
            disabled={busy || targetsQuery.isLoading}
          >
            <option value="">请选择部署目标</option>
            {targets.map((t) => (
              <option key={t.id} value={t.id}>
                {targetLabel(t)}
              </option>
            ))}
          </Select>
        </Field>
        <Field label="版本" hint="标签、短 SHA 或语义版本，最多 64 字符" htmlFor="deploy-version">
          <Input
            id="deploy-version"
            mono
            value={version}
            onChange={(e) => setVersion(e.target.value)}
            disabled={busy}
            placeholder="v1.2.3"
          />
        </Field>
        <Field label="构建 ID" hint="选填；必须是本项目的成功构建" htmlFor="deploy-build">
          <Input
            id="deploy-build"
            mono
            value={buildId}
            onChange={(e) => setBuildId(e.target.value)}
            disabled={busy}
            placeholder="build id"
          />
        </Field>
        {error && (
          <p role="alert" className="text-body-sm text-error bg-error/10 rounded px-space-lg py-space-md">
            {error}
          </p>
        )}
        <div className="flex gap-space-md">
          <Button type="submit" disabled={busy || !projectId || !targetId || !version.trim()}>
            {busy ? '创建中...' : '创建部署'}
          </Button>
          <Button type="button" variant="secondary" disabled={busy} onClick={() => nav(-1)}>
            返回
          </Button>
        </div>
      </form>
    </>
  )
}

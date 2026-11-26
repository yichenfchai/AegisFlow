import { useState } from 'react'
import { Link, useNavigate, useParams } from 'react-router'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { Button, Field, PageHeader, Select } from '@/components/ui/DataTable'
import { projectsAPI } from '@/api/projects'
import {
  policiesAPI,
  policyGateHints,
  REQUIRED_SCAN_VERDICT_TEXT,
  type PolicyInput,
  type ProjectPolicy,
  type RequiredScanVerdict,
} from '@/api/policies'
import { useSession } from '@/stores/session'
import { ApiError } from '@/api/http'
import { errorText } from '@/api/errors'
import { fmtRel } from '@/api/format'

function policySaveError(cause: unknown): string {
  if (cause instanceof ApiError) {
    if (cause.status === 400) return '保存失败：策略参数不合法；扫描结论只能是「不要求」或「必须通过」'
    if (cause.status === 403) return '保存失败：只有项目维护者或全局管理员可以修改自动化策略'
    if (cause.status === 404) return '保存失败：项目不存在或对你不可见'
  }
  return errorText(cause, '保存自动化策略失败，请稍后重试')
}

/**
 * 项目自动化策略（真实接口）：requireReview 与 requiredScanVerdict 存入数据库，
 * 在部署创建与回滚时叠加到既有环境变量门禁之上；admin 或项目维护者可写，
 * 其他角色只读。项目详情不单独存在，因此策略作为项目子页面挂在项目列表入口下。
 */
export function ProjectPolicyPage() {
  const { id = '' } = useParams<{ id: string }>()
  const { user } = useSession()
  const queryClient = useQueryClient()
  const [notice, setNotice] = useState('')

  const project = useQuery({
    queryKey: ['project', id],
    queryFn: () => projectsAPI.project(id),
    enabled: id !== '',
    retry: false,
  })
  const members = useQuery({
    queryKey: ['project-members', id],
    queryFn: () => projectsAPI.members(id),
    enabled: id !== '',
    retry: false,
  })
  const policy = useQuery({
    queryKey: ['project-policy', id],
    queryFn: () => policiesAPI.get(id),
    enabled: id !== '',
    retry: false,
  })

  const isAdmin = user?.role === 'admin'
  const myRole = (members.data ?? []).find((member) => member.userId === user?.id)?.role
  const canEdit = isAdmin || myRole === 'maintainer'

  if (project.error) {
    const status = project.error instanceof ApiError ? project.error.status : 0
    return (
      <>
        <PageHeader title="自动化策略" />
        <p role="alert" className="text-body-md text-error py-space-lg">
          {status === 404
            ? '未找到该项目：它可能是私有项目，而你不是它的成员。'
            : errorText(project.error, '加载项目失败')}
        </p>
        <Link to="/" className="text-body-md text-primary hover:underline">
          返回项目列表
        </Link>
      </>
    )
  }
  if (policy.error) {
    return (
      <>
        <PageHeader title="自动化策略" />
        <p role="alert" className="text-body-md text-error py-space-lg">
          {errorText(policy.error, '加载自动化策略失败')}
        </p>
        <Link to="/" className="text-body-md text-primary hover:underline">
          返回项目列表
        </Link>
      </>
    )
  }
  if (!project.data || !policy.data) {
    return (
      <>
        <PageHeader title="自动化策略" subtitle="加载中..." />
        <p className="text-body-md text-outline py-space-lg">加载中...</p>
      </>
    )
  }

  return (
    <PolicyForm
      key={`${policy.data.projectId}-${policy.data.updatedAt}`}
      projectId={project.data.id}
      projectName={project.data.name}
      policy={policy.data}
      canEdit={canEdit}
      notice={notice}
      onSaved={async (saved) => {
        queryClient.setQueryData(['project-policy', saved.projectId], saved)
        await queryClient.invalidateQueries({ queryKey: ['project-policy', saved.projectId] })
        setNotice('自动化策略已保存，新的门禁规则立即对部署创建与回滚生效')
      }}
    />
  )
}

function PolicyForm({
  projectId,
  projectName,
  policy,
  canEdit,
  notice,
  onSaved,
}: {
  projectId: string
  projectName: string
  policy: ProjectPolicy
  canEdit: boolean
  notice: string
  onSaved: (saved: ProjectPolicy) => void | Promise<void>
}) {
  const [requireReview, setRequireReview] = useState(policy.requireReview)
  const [requiredScanVerdict, setRequiredScanVerdict] = useState<RequiredScanVerdict>(
    policy.requiredScanVerdict,
  )
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const nav = useNavigate()

  const unchanged =
    requireReview === policy.requireReview && requiredScanVerdict === policy.requiredScanVerdict
  const hints = policyGateHints({ requireReview, requiredScanVerdict })
  const stored = policy.updatedAt > 0

  const submit = async (e?: React.FormEvent) => {
    e?.preventDefault()
    if (busy || !canEdit) return
    setError('')
    setBusy(true)
    try {
      const input: PolicyInput = { requireReview, requiredScanVerdict }
      const updated = await policiesAPI.update(projectId, input)
      await onSaved(updated)
    } catch (cause) {
      setError(policySaveError(cause))
    } finally {
      setBusy(false)
    }
  }

  return (
    <>
      <PageHeader
        title={`${projectName} · 自动化策略`}
        subtitle="按项目配置的部署门禁：需要关联已批准审查、或要求最新扫描通过；与全局环境变量门禁叠加，只会加严"
        actions={
          <Link to="/" className="text-body-md text-primary hover:underline">
            返回项目列表
          </Link>
        }
      />
      {!canEdit && (
        <p
          role="status"
          className="mb-space-md text-body-sm text-on-surface-variant bg-surface-container rounded px-space-lg py-space-md border border-surface-container-highest"
        >
          当前角色只读：只有项目维护者或全局管理员可以修改自动化策略。
        </p>
      )}
      {(notice) && (
        <p role="status" className="mb-space-md text-body-sm text-on-surface bg-surface-container border border-surface-container-highest rounded px-space-lg py-space-md">
          {notice}
        </p>
      )}
      <div className="mb-space-lg rounded-lg border border-surface-container-highest bg-surface-container p-space-xl max-w-[880px]">
        <p className="text-body-sm text-outline">
          {stored
            ? `上次更新：${fmtRel(policy.updatedAt * 1000)} · ${policy.updatedByName || policy.updatedBy || '未知操作人'}`
            : '尚未配置：当前使用默认策略（不要求关联审查、不要求扫描结论），部署行为与引入策略前一致。'}
        </p>
        <dl className="mt-space-md grid grid-cols-2 gap-space-md text-body-sm">
          <div>
            <dt className="text-outline">关联已批准审查</dt>
            <dd className="font-medium">{policy.requireReview ? '要求' : '不要求'}</dd>
          </div>
          <div>
            <dt className="text-outline">最新扫描结论</dt>
            <dd className="font-medium">
              {REQUIRED_SCAN_VERDICT_TEXT[policy.requiredScanVerdict]}
            </dd>
          </div>
        </dl>
      </div>
      <form onSubmit={submit} className="max-w-[640px] space-y-space-xl" aria-busy={busy}>
        <Field
          label="关联已批准审查"
          hint="开启后，无论部署目标环境是否在全局审查门禁列表中，创建与回滚部署都必须选择一条本项目已批准的审查。"
          htmlFor="policy-require-review"
        >
          <div className="flex items-center gap-space-sm">
            <input
              id="policy-require-review"
              type="checkbox"
              checked={requireReview}
              onChange={(e) => {
                setRequireReview(e.target.checked)
              }}
              disabled={busy || !canEdit}
              className="accent-primary"
            />
            <span className="text-body-md">要求部署关联已批准审查</span>
          </div>
        </Field>
        <Field
          label="最新扫描结论"
          hint="选择「最新扫描必须通过」后，项目最新一条扫描必须存在且结论为通过，否则部署创建与回滚会被拒绝。"
          htmlFor="policy-scan-verdict"
        >
          <Select
            id="policy-scan-verdict"
            value={requiredScanVerdict}
            onChange={(e) => {
              setRequiredScanVerdict(e.target.value as RequiredScanVerdict)
            }}
            disabled={busy || !canEdit}
          >
            <option value="none">不要求</option>
            <option value="passed">最新扫描必须通过</option>
          </Select>
        </Field>
        {hints.length > 0 && (
          <div role="status" className="rounded border border-surface-container-highest bg-surface-container px-space-lg py-space-md text-body-sm text-on-surface-variant space-y-space-xs">
            {hints.map((hint) => (
              <p key={hint}>{hint}</p>
            ))}
          </div>
        )}
        {error && (
          <p role="alert" className="text-body-sm text-error bg-error/10 rounded px-space-lg py-space-md">
            {error}
          </p>
        )}
        <div className="flex gap-space-md">
          <Button type="submit" disabled={busy || !canEdit || unchanged}>
            {busy ? '保存中...' : '保存策略'}
          </Button>
          <Button type="button" variant="secondary" disabled={busy} onClick={() => nav(-1)}>
            返回
          </Button>
        </div>
      </form>
    </>
  )
}

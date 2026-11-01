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
import { ApiError } from '@/api/http'

const VERSION_PATTERN = /^[A-Za-z0-9._+-]{1,64}$/

function createError(cause: unknown): string {
  if (cause instanceof ApiError) {
    if (cause.status === 400) {
      return '请求不合法：检查版本格式（1-64 位字母、数字、. _ + -）、目标是否启用、构建 ID 是否为本项目的成功构建'
    }
    if (cause.status === 403) return '当前角色没有发起部署的权限'
    if (cause.status === 404) return '项目不存在或已被删除'
    if (cause.status === 503) return '服务依赖不可用，请稍后重试'
    if (cause.status === 0) return '无法连接服务，请检查网络后重试'
  }
  return cause instanceof Error ? cause.message : '创建部署失败，请稍后重试'
}

const targetLabel = (target: DeploymentTargetOption) =>
  `${target.name} · ${DEPLOY_ENVIRONMENT_TEXT[target.environment]} · ${target.endpoint}`

/** 新建部署（真实接口）：生产目标会先进入审批。 */
export function DeploymentNewPage() {
  const nav = useNavigate()
  const queryClient = useQueryClient()
  const [projectId, setProjectId] = useState('')
  const [targetId, setTargetId] = useState('')
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
  const projects = projectsQuery.data?.items ?? []
  const targets = (targetsQuery.data?.items ?? []).filter((t) => t.status === 'active')

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
            onChange={(e) => setProjectId(e.target.value)}
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

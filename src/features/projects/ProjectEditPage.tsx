import { useState } from 'react'
import { Link, useNavigate, useParams } from 'react-router'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { Button, Field, Input, PageHeader, Select } from '@/components/ui/DataTable'
import { StatusBadge } from '@/components/ui/Badge'
import {
  projectsAPI,
  PROJECT_VISIBILITY_TEXT,
  type Project,
  type ProjectPatch,
  type ProjectVisibility,
} from '@/api/projects'
import { ApiError } from '@/api/http'
import { errorText } from '@/api/errors'
import { fmtRel } from '@/api/format'
import { projectFormError, projectWriteError, type ProjectFormValues } from './projectForm'

function formValues(project: Project): ProjectFormValues {
  return {
    name: project.name,
    description: project.description,
    repoUrl: project.repoUrl,
    defaultBranch: project.defaultBranch,
    buildCommand: project.buildCommand,
    visibility: project.visibility,
  }
}

/** 只把真正变化的字段放进 PATCH，避免无修改提交产生审计噪音。 */
function buildPatch(project: Project, values: ProjectFormValues): ProjectPatch {
  const patch: ProjectPatch = {}
  if (values.name.trim() !== project.name) patch.name = values.name.trim()
  if (values.description.trim() !== project.description) patch.description = values.description.trim()
  if (values.repoUrl.trim() !== project.repoUrl) patch.repoUrl = values.repoUrl.trim()
  if (values.defaultBranch.trim() !== project.defaultBranch) patch.defaultBranch = values.defaultBranch.trim()
  if (values.buildCommand.trim() !== project.buildCommand) patch.buildCommand = values.buildCommand.trim()
  if (values.visibility !== project.visibility) patch.visibility = values.visibility
  return patch
}

/**
 * 项目编辑（真实接口）：复用 PATCH /projects/{id} 修改全部可写字段。
 * 归档项目是只读的——服务端会拒绝字段修改，这里同时禁用表单并说明原因。
 */
export function ProjectEditPage() {
  const { id = '' } = useParams<{ id: string }>()
  const project = useQuery({
    queryKey: ['project', id],
    queryFn: () => projectsAPI.project(id),
    enabled: id !== '',
    retry: false,
  })

  if (project.error) {
    const status = project.error instanceof ApiError ? project.error.status : 0
    return (
      <>
        <PageHeader title="编辑项目" />
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
  if (!project.data) {
    return (
      <>
        <PageHeader title="编辑项目" subtitle="加载中..." />
        <p className="text-body-md text-outline py-space-lg">加载中...</p>
      </>
    )
  }
  // key 里带 updatedAt：数据重新拉取后表单以服务端值为准。
  return <ProjectEditForm key={`${project.data.id}-${project.data.updatedAt}`} project={project.data} />
}

function ProjectEditForm({ project }: { project: Project }) {
  const nav = useNavigate()
  const queryClient = useQueryClient()
  const archived = project.status === 'archived'
  const [values, setValues] = useState<ProjectFormValues>(() => formValues(project))
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)

  const field = (key: keyof ProjectFormValues) => (event: React.ChangeEvent<HTMLInputElement | HTMLSelectElement>) =>
    setValues((prev) => ({ ...prev, [key]: event.target.value }))

  const submit = async (e?: React.FormEvent) => {
    e?.preventDefault()
    if (busy || archived) return
    setError('')
    const invalid = projectFormError(values)
    if (invalid) {
      setError(invalid)
      return
    }
    const patch = buildPatch(project, values)
    if (Object.keys(patch).length === 0) {
      setError('没有需要保存的修改')
      return
    }
    setBusy(true)
    try {
      await projectsAPI.update(project.id, patch)
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: ['projects'] }),
        queryClient.invalidateQueries({ queryKey: ['project', project.id] }),
      ])
      nav('/', { replace: true, state: { notice: `项目「${patch.name ?? project.name}」已更新` } })
    } catch (cause) {
      setError(projectWriteError(cause, '保存'))
    } finally {
      setBusy(false)
    }
  }

  return (
    <>
      <PageHeader
        title={`编辑项目 · ${project.name}`}
        subtitle="保存后写入项目审计；构建命令的改动只影响之后的构建（已触发构建保留命令快照）"
        actions={
          <Link to="/" className="text-body-md text-primary hover:underline">
            返回项目列表
          </Link>
        }
      />
      {archived && (
        <p role="status" className="mb-space-md text-body-sm text-on-surface-variant bg-surface-container rounded px-space-lg py-space-md border border-surface-container-highest">
          项目已归档，当前为只读状态。请回到项目列表点击「恢复」后再编辑。
        </p>
      )}
      <p className="mb-space-lg text-body-sm text-outline">
        当前可见性：
        {project.visibility === 'private' ? (
          <StatusBadge tone="warning">{PROJECT_VISIBILITY_TEXT.private}</StatusBadge>
        ) : (
          <StatusBadge tone="neutral">{PROJECT_VISIBILITY_TEXT.public}</StatusBadge>
        )}
        <span className="ml-space-sm">创建于 {fmtRel(project.createdAt * 1000)}</span>
      </p>
      <form onSubmit={submit} className="max-w-[640px] space-y-space-xl" aria-busy={busy}>
        <Field label="项目名称" hint="3-64 字节，全局唯一（不区分大小写）" htmlFor="project-name">
          <Input
            id="project-name"
            value={values.name}
            onChange={field('name')}
            disabled={busy || archived}
            placeholder="web-api"
          />
        </Field>
        <Field label="描述" hint="选填，最多 512 字节" htmlFor="project-description">
          <Input
            id="project-description"
            value={values.description}
            onChange={field('description')}
            disabled={busy || archived}
            placeholder="主服务"
          />
        </Field>
        <Field label="仓库地址 / 本地路径" hint="https:// 或 git@ 的 git 地址，或部署机上的绝对路径；选填" htmlFor="project-repo">
          <Input
            id="project-repo"
            mono
            value={values.repoUrl}
            onChange={field('repoUrl')}
            disabled={busy || archived}
            placeholder="https://github.com/example/web-api.git"
          />
        </Field>
        <Field label="默认分支" hint="普通 git 引用名，例如 main、release/2026；选填" htmlFor="project-branch">
          <Input
            id="project-branch"
            mono
            value={values.defaultBranch}
            onChange={field('defaultBranch')}
            disabled={busy || archived}
            placeholder="main"
          />
        </Field>
        <Field
          label="可见性"
          hint="公开：所有角色可见；私有：仅管理员、创建者与项目成员可见（非成员访问返回未找到）"
          htmlFor="project-visibility"
        >
          <Select
            id="project-visibility"
            value={values.visibility}
            onChange={(e) => setValues((prev) => ({ ...prev, visibility: e.target.value as ProjectVisibility }))}
            disabled={busy || archived}
          >
            <option value="public">公开</option>
            <option value="private">私有</option>
          </Select>
        </Field>
        <Field
          label="构建命令"
          hint="单行 shell 命令，最多 512 字节；在临时目录执行。选填，未配置的项目不能触发构建"
          htmlFor="project-build-command"
        >
          <Input
            id="project-build-command"
            mono
            value={values.buildCommand}
            onChange={field('buildCommand')}
            disabled={busy || archived}
            placeholder="echo hello"
          />
        </Field>
        {error && (
          <p role="alert" className="text-body-sm text-error bg-error/10 rounded px-space-lg py-space-md">
            {error}
          </p>
        )}
        <div className="flex gap-space-md">
          <Button type="submit" disabled={busy || archived || !values.name.trim()}>
            {busy ? '保存中...' : '保存修改'}
          </Button>
          <Button type="button" variant="secondary" disabled={busy} onClick={() => nav('/')}>
            返回
          </Button>
        </div>
      </form>
    </>
  )
}

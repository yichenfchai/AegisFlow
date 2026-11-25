import { useState } from 'react'
import { useNavigate } from 'react-router'
import { useQueryClient } from '@tanstack/react-query'
import { Button, Field, Input, PageHeader, Select } from '@/components/ui/DataTable'
import { projectsAPI, type ProjectVisibility } from '@/api/projects'
import { projectFormError, projectWriteError, type ProjectFormValues } from './projectForm'

const emptyForm: ProjectFormValues = {
  name: '',
  description: '',
  repoUrl: '',
  defaultBranch: 'main',
  buildCommand: '',
  visibility: 'public',
}

/** 创建项目（真实接口）：默认分支为自由输入的 git 引用名。 */
export function ProjectWizardPage() {
  const nav = useNavigate()
  const queryClient = useQueryClient()
  const [values, setValues] = useState<ProjectFormValues>(emptyForm)
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)

  const field = (key: keyof ProjectFormValues) => (event: React.ChangeEvent<HTMLInputElement | HTMLSelectElement>) =>
    setValues((prev) => ({ ...prev, [key]: event.target.value }))

  const submit = async (e?: React.FormEvent) => {
    e?.preventDefault()
    if (busy) return
    setError('')
    const invalid = projectFormError(values)
    if (invalid) {
      setError(invalid)
      return
    }
    setBusy(true)
    try {
      await projectsAPI.create({
        name: values.name.trim(),
        description: values.description.trim() || undefined,
        repoUrl: values.repoUrl.trim() || undefined,
        defaultBranch: values.defaultBranch.trim() || undefined,
        buildCommand: values.buildCommand.trim() || undefined,
        visibility: values.visibility,
      })
      await queryClient.invalidateQueries({ queryKey: ['projects'] })
      nav('/', { replace: true })
    } catch (cause) {
      setError(projectWriteError(cause, '创建'))
    } finally {
      setBusy(false)
    }
  }

  return (
    <>
      <PageHeader title="创建项目" subtitle="登记仓库地址或本地路径；构建与部署将以项目为单位组织" />
      <form onSubmit={submit} className="max-w-[640px] space-y-space-xl" aria-busy={busy}>
        <Field label="项目名称" hint="3-64 字节，全局唯一（不区分大小写）" htmlFor="project-name">
          <Input id="project-name" value={values.name} onChange={field('name')} disabled={busy} placeholder="web-api" />
        </Field>
        <Field label="描述" hint="选填，最多 512 字节" htmlFor="project-description">
          <Input id="project-description" value={values.description} onChange={field('description')} disabled={busy} placeholder="主服务" />
        </Field>
        <Field label="仓库地址 / 本地路径" hint="https:// 或 git@ 的 git 地址，或部署机上的绝对路径；选填" htmlFor="project-repo">
          <Input
            id="project-repo"
            mono
            value={values.repoUrl}
            onChange={field('repoUrl')}
            disabled={busy}
            placeholder="https://github.com/example/web-api.git"
          />
        </Field>
        <Field label="默认分支" hint="普通 git 引用名，例如 main、release/2026；选填" htmlFor="project-branch">
          <Input
            id="project-branch"
            mono
            value={values.defaultBranch}
            onChange={field('defaultBranch')}
            disabled={busy}
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
            disabled={busy}
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
            disabled={busy}
            placeholder="echo hello"
          />
        </Field>
        {error && (
          <p role="alert" className="text-body-sm text-error bg-error/10 rounded px-space-lg py-space-md">
            {error}
          </p>
        )}
        <div className="flex gap-space-md">
          <Button type="submit" disabled={busy || !values.name.trim()}>
            {busy ? '创建中...' : '创建项目'}
          </Button>
          <Button type="button" variant="secondary" disabled={busy} onClick={() => nav(-1)}>
            返回
          </Button>
        </div>
      </form>
    </>
  )
}

import { useState } from 'react'
import { useNavigate } from 'react-router'
import { useQueryClient } from '@tanstack/react-query'
import { Button, Field, Input, PageHeader, Select } from '@/components/ui/DataTable'
import { projectsAPI, type ProjectVisibility } from '@/api/projects'
import { ApiError } from '@/api/http'

function byteLength(value: string): number {
  return new TextEncoder().encode(value).length
}

function opError(cause: unknown): string {
  if (cause instanceof ApiError) {
    if (cause.status === 409) return '同名项目已存在（不区分大小写）'
    if (cause.status === 400) return '字段不合法：名称 3-64 字节且不含路径字符；仓库地址须为 https/git@ URL 或绝对路径；分支为普通 git 引用名'
    if (cause.status === 0) return '无法连接服务，请检查网络后重试'
  }
  return cause instanceof Error ? cause.message : '创建失败，请稍后重试'
}

/** 创建项目（真实接口）：P2 落地后替代原三步演示向导。 */
export function ProjectWizardPage() {
  const nav = useNavigate()
  const queryClient = useQueryClient()
  const [name, setName] = useState('')
  const [description, setDescription] = useState('')
  const [repoUrl, setRepoUrl] = useState('')
  const [defaultBranch, setDefaultBranch] = useState('main')
  const [buildCommand, setBuildCommand] = useState('')
  const [visibility, setVisibility] = useState<ProjectVisibility>('public')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)

  const submit = async (e?: React.FormEvent) => {
    e?.preventDefault()
    if (busy) return
    setError('')
    if (byteLength(name.trim()) < 3 || byteLength(name) > 64) {
      setError('项目名称需为 3-64 字节')
      return
    }
    if (name !== name.trim() || /^[.-\s]/.test(name) || /[\\/:?!*"<>|]/.test(name)) {
      setError('项目名称不能以点、连字符或空白开头，且不含路径与特殊字符')
      return
    }
    if (/[\r\n]/.test(buildCommand) || byteLength(buildCommand) > 512) {
      setError('构建命令需为单行，最多 512 字节')
      return
    }
    setBusy(true)
    try {
      await projectsAPI.create({
        name: name.trim(),
        description: description.trim() || undefined,
        repoUrl: repoUrl.trim() || undefined,
        defaultBranch: defaultBranch.trim() || undefined,
        buildCommand: buildCommand.trim() || undefined,
        visibility,
      })
      await queryClient.invalidateQueries({ queryKey: ['projects'] })
      nav('/', { replace: true })
    } catch (cause) {
      setError(opError(cause))
    } finally {
      setBusy(false)
    }
  }

  return (
    <>
      <PageHeader title="创建项目" subtitle="登记仓库地址或本地路径；构建与部署将以项目为单位组织" />
      <form onSubmit={submit} className="max-w-[640px] space-y-space-xl" aria-busy={busy}>
        <Field label="项目名称" hint="3-64 字节，全局唯一（不区分大小写）" htmlFor="project-name">
          <Input id="project-name" value={name} onChange={(e) => setName(e.target.value)} disabled={busy} placeholder="web-api" />
        </Field>
        <Field label="描述" hint="选填，最多 512 字节" htmlFor="project-description">
          <Input id="project-description" value={description} onChange={(e) => setDescription(e.target.value)} disabled={busy} placeholder="主服务" />
        </Field>
        <Field label="仓库地址 / 本地路径" hint="https:// 或 git@ 的 git 地址，或部署机上的绝对路径；选填" htmlFor="project-repo">
          <Input id="project-repo" mono value={repoUrl} onChange={(e) => setRepoUrl(e.target.value)} disabled={busy} placeholder="https://github.com/example/web-api.git" />
        </Field>
        <Field label="默认分支" hint="普通 git 引用名；选填" htmlFor="project-branch">
          <Select id="project-branch" value={defaultBranch} onChange={(e) => setDefaultBranch(e.target.value)} disabled={busy}>
            <option value="main">main</option>
            <option value="master">master</option>
            <option value="">（不设置）</option>
          </Select>
        </Field>
        <Field
          label="可见性"
          hint="公开：所有角色可见；私有：仅管理员、创建者与项目成员可见（非成员访问返回未找到）"
          htmlFor="project-visibility"
        >
          <Select
            id="project-visibility"
            value={visibility}
            onChange={(e) => setVisibility(e.target.value as ProjectVisibility)}
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
            value={buildCommand}
            onChange={(e) => setBuildCommand(e.target.value)}
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
          <Button type="submit" disabled={busy || !name.trim()}>
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

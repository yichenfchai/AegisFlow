import { useState } from 'react'
import { useNavigate } from 'react-router'
import { useQueryClient } from '@tanstack/react-query'
import { Button, Field, Input, PageHeader, Select } from '@/components/ui/DataTable'
import {
  deploymentTargetsAPI,
  DEPLOY_ENVIRONMENT_TEXT,
  DEPLOY_TARGET_STATUS_TEXT,
  type DeploymentEnvironment,
  type DeploymentTargetStatus,
} from '@/api/deployments'
import { ApiError } from '@/api/http'

const ENVIRONMENTS: DeploymentEnvironment[] = ['development', 'staging', 'production']
const STATUSES: DeploymentTargetStatus[] = ['active', 'disabled']

function byteLength(value: string): number {
  return new TextEncoder().encode(value).length
}

function opError(cause: unknown): string {
  if (cause instanceof ApiError) {
    if (cause.status === 409) return '同名部署目标已存在（不区分大小写）'
    if (cause.status === 400) {
      return '字段不合法：名称 3-64 字节且单行；端点须为 http(s) URL 且不含用户信息、查询或片段；健康路径以 / 开头；凭据仅可配合 https 或回环地址'
    }
    if (cause.status === 403) return '当前角色没有创建部署目标的权限'
    if (cause.status === 503) return '服务依赖不可用，请稍后重试'
    if (cause.status === 0) return '无法连接服务，请检查网络后重试'
  }
  return cause instanceof Error ? cause.message : '创建失败，请稍后重试'
}

/** 新建部署目标（真实接口）：管理员配置端点、环境与可选凭据。 */
export function DeploymentTargetWizardPage() {
  const nav = useNavigate()
  const queryClient = useQueryClient()
  const [name, setName] = useState('')
  const [environment, setEnvironment] = useState<DeploymentEnvironment>('development')
  const [endpoint, setEndpoint] = useState('')
  const [healthPath, setHealthPath] = useState('/')
  const [secret, setSecret] = useState('')
  const [status, setStatus] = useState<DeploymentTargetStatus>('active')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)

  const submit = async (e?: React.FormEvent) => {
    e?.preventDefault()
    if (busy) return
    setError('')
    const trimmedName = name.trim()
    const trimmedEndpoint = endpoint.trim()
    const trimmedHealthPath = healthPath.trim() || '/'
    if (byteLength(trimmedName) < 3 || byteLength(trimmedName) > 64) {
      setError('名称需为 3-64 字节')
      return
    }
    if (name !== name.trim() || /[\r\n]/.test(name)) {
      setError('名称不能包含首尾空白或换行')
      return
    }
    if (!/^https?:\/\/\S+$/.test(trimmedEndpoint)) {
      setError('端点须为 http:// 或 https:// 开头的 URL，且不含空白')
      return
    }
    if (!trimmedHealthPath.startsWith('/') || /\s/.test(trimmedHealthPath)) {
      setError('健康路径须以 / 开头且不含空白')
      return
    }
    if (byteLength(secret) > 4096) {
      setError('凭据最多 4096 字节')
      return
    }
    setBusy(true)
    try {
      await deploymentTargetsAPI.create({
        name: trimmedName,
        environment,
        endpoint: trimmedEndpoint,
        healthPath: trimmedHealthPath,
        status,
        secret: secret || undefined,
      })
      await queryClient.invalidateQueries({ queryKey: ['deployment-targets'] })
      nav('/deploy-targets')
    } catch (cause) {
      setError(opError(cause))
    } finally {
      setBusy(false)
    }
  }

  return (
    <>
      <PageHeader title="新建部署目标" subtitle="登记部署端点；凭据加密存储，仅用于连通性检查" />
      <form onSubmit={submit} className="max-w-[640px] space-y-space-xl" aria-busy={busy}>
        <Field label="名称" hint="3-64 字节，单行，全局唯一（不区分大小写）" htmlFor="target-name">
          <Input
            id="target-name"
            value={name}
            onChange={(e) => setName(e.target.value)}
            disabled={busy}
            placeholder="生产 Web"
          />
        </Field>
        <Field label="环境" hint="用于区分目标的部署环境" htmlFor="target-environment">
          <Select
            id="target-environment"
            value={environment}
            onChange={(e) => setEnvironment(e.target.value as DeploymentEnvironment)}
            disabled={busy}
          >
            {ENVIRONMENTS.map((value) => (
              <option key={value} value={value}>
                {DEPLOY_ENVIRONMENT_TEXT[value]}
              </option>
            ))}
          </Select>
        </Field>
        <Field
          label="端点"
          hint="http(s) URL，不含用户信息、查询参数或片段；检查时追加健康路径"
          htmlFor="target-endpoint"
        >
          <Input
            id="target-endpoint"
            mono
            value={endpoint}
            onChange={(e) => setEndpoint(e.target.value)}
            disabled={busy}
            placeholder="https://app.example.com"
          />
        </Field>
        <Field label="健康路径" hint="以 / 开头；默认 /" htmlFor="target-health-path">
          <Input
            id="target-health-path"
            mono
            value={healthPath}
            onChange={(e) => setHealthPath(e.target.value)}
            disabled={busy}
            placeholder="/healthz"
          />
        </Field>
        <Field
          label="凭据"
          hint="选填；仅 https 或回环地址可配置，保存后不再回显"
          htmlFor="target-secret"
        >
          <Input
            id="target-secret"
            type="password"
            autoComplete="new-password"
            value={secret}
            onChange={(e) => setSecret(e.target.value)}
            disabled={busy}
            placeholder="Bearer 凭据"
          />
        </Field>
        <Field label="状态" hint="已禁用的目标仍可检查" htmlFor="target-status">
          <Select
            id="target-status"
            value={status}
            onChange={(e) => setStatus(e.target.value as DeploymentTargetStatus)}
            disabled={busy}
          >
            {STATUSES.map((value) => (
              <option key={value} value={value}>
                {DEPLOY_TARGET_STATUS_TEXT[value]}
              </option>
            ))}
          </Select>
        </Field>
        {error && (
          <p role="alert" className="text-body-sm text-error bg-error/10 rounded px-space-lg py-space-md">
            {error}
          </p>
        )}
        <div className="flex gap-space-md">
          <Button type="submit" disabled={busy || !name.trim() || !endpoint.trim()}>
            {busy ? '创建中...' : '创建'}
          </Button>
          <Button type="button" variant="secondary" disabled={busy} onClick={() => nav(-1)}>
            返回
          </Button>
        </div>
      </form>
    </>
  )
}

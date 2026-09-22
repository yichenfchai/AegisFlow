import { useState } from 'react'
import { PageHeader, Button, Field, Input, Select, Stepper } from '@/components/ui/DataTable'
import { StatusBadge } from '@/components/ui/Badge'
import { Icon } from '@/components/ui/Icon'
import { useQuery } from '@tanstack/react-query'
import { api } from '@/api'

export function DeployTargetListPage() {
  const { data: targets = [] } = useQuery({ queryKey: ['deployTargets'], queryFn: api.deployTargets })

  return (
    <>
      <PageHeader
        title="部署目标"
        subtitle="SSH 直连的目标主机与镜像保留策略"
        actions={<Button icon="add">添加主机</Button>}
      />
      <div className="grid grid-cols-2 gap-gutter">
        {targets.map((t) => (
          <div key={t.id} className="bg-surface-container rounded-lg border border-surface-container-highest p-space-xl space-y-space-md">
            <div className="flex items-center justify-between">
              <div className="flex items-center gap-space-md">
                <Icon name="dns" size={20} className="text-on-surface-variant" />
                <span className="text-headline-sm font-semibold">{t.name}</span>
              </div>
              <StatusBadge
                tone={t.status === 'online' ? 'success' : t.status === 'degraded' ? 'pending' : 'error'}
                dot={t.status !== 'offline'}
              >
                {t.status === 'online' ? '主机直连在线' : t.status === 'degraded' ? '降级告警' : '超时未响应'}
              </StatusBadge>
            </div>
            <div className="grid grid-cols-2 gap-space-md text-body-sm">
              <Row label="SSH 连接地址" value={<span className="font-mono">{t.sshAddr}</span>} />
              <Row label="登录用户名" value={t.sshUser} />
              <Row label="Runtime 版本" value={<span className="font-mono">{t.runtimeVersion}</span>} />
              <Row label="远程工作目录" value={<span className="font-mono">{t.remoteWorkdir}</span>} />
              <Row
                label="健康检查"
                value={
                  <span className={t.healthPass === t.healthTotal ? 'text-secondary' : 'text-error'}>
                    {t.healthPass}/{t.healthTotal} {t.healthPass === t.healthTotal ? '全部通过' : '超时未响应'}
                  </span>
                }
              />
              <Row label="镜像 Tag 保留数" value={<span className="font-mono">{t.keepTags} 个</span>} />
            </div>
            <div className="flex items-center justify-between pt-space-sm border-t border-surface-container-highest">
              <span className="text-label-sm text-outline">
                最近连通: {new Date(t.lastSeenAt).toLocaleString('zh-CN')}
              </span>
              <div className="flex gap-space-sm">
                <Button variant="secondary" size="sm" icon="wifi" onClick={() => alert('连通性测试（演示）')}>
                  测试连接
                </Button>
                <Button variant="ghost" size="sm">
                  配置
                </Button>
              </div>
            </div>
          </div>
        ))}
      </div>
    </>
  )
}

function Row({ label, value }: { label: string; value: React.ReactNode }) {
  return (
    <div className="flex flex-col gap-0.5">
      <span className="text-label-sm text-outline">{label}</span>
      <span className="text-on-surface">{value}</span>
    </div>
  )
}

export function DeployTargetFormPage() {
  const [keepTags, setKeepTags] = useState(5)
  const [retry, setRetry] = useState(3)
  return (
    <>
      <PageHeader title="添加部署目标主机" subtitle="通过 SSH Agent 直连目标主机执行部署" />
      <div className="max-w-[640px] bg-surface-container rounded-lg border border-surface-container-highest p-space-xl space-y-space-lg">
        <Field label="主机名称" hint="用于部署单展示与审计日志">
          <Input placeholder="如 prod-vps-01" />
        </Field>
        <div className="grid grid-cols-2 gap-gutter">
          <Field label="SSH 连接地址">
            <Input mono placeholder="10.0.0.8:22" />
          </Field>
          <Field label="登录用户名">
            <Input placeholder="deploy" />
          </Field>
        </div>
        <Field label="SSH 认证" hint="复用密钥库托管的 SSH 私钥">
          <Select>
            <option>SSH 私钥 (1) · SHA256:9f3a…c1de</option>
            <option>+ 新建密钥</option>
          </Select>
        </Field>
        <Field label="远程工作目录">
          <Input mono placeholder="/srv/web-api" />
        </Field>
        <div className="grid grid-cols-2 gap-gutter">
          <Field label="镜像 Tag 保留数" hint="超出后自动清理最旧镜像">
            <div className="pt-1">
              <Stepper value={keepTags} onChange={setKeepTags} min={1} max={20} />
            </div>
          </Field>
          <Field label="连通重试次数">
            <div className="pt-1">
              <Stepper value={retry} onChange={setRetry} min={0} max={10} />
            </div>
          </Field>
        </div>
        <div className="flex items-center gap-space-md bg-surface-container-low rounded p-space-lg">
          <Icon name="wifi" size={16} className="text-secondary" />
          <span className="text-body-sm text-on-surface-variant flex-1">
            SSH Agent 就绪 · 保存后自动执行连通性测试
          </span>
          <Button size="sm" variant="secondary">
            立即测试
          </Button>
        </div>
        <div className="flex justify-end gap-space-md">
          <Button variant="secondary">取消</Button>
          <Button icon="check">保存并验证</Button>
        </div>
      </div>
    </>
  )
}

import { useQuery } from '@tanstack/react-query'
import { useState } from 'react'
import { api } from '@/api'
import { PageHeader, Button, DataTable, ConfirmDialog } from '@/components/ui/DataTable'
import { StatCard } from '@/components/ui/Badge'
import { StatusBadge } from '@/components/ui/Badge'
import { Icon } from '@/components/ui/Icon'
import type { SecretItem } from '@/api/types'

const TYPE_TEXT: Record<SecretItem['type'], string> = {
  ssh_key: 'SSH 私钥',
  registry: 'Registry 凭据',
  env_var: '环境变量',
}

export function SecretsPage() {
  const { data: secrets = [] } = useQuery({ queryKey: ['secrets'], queryFn: api.secrets })
  const [confirmDel, setConfirmDel] = useState<SecretItem | null>(null)

  return (
    <>
      <PageHeader
        title="密钥管理"
        subtitle="AES-256-GCM 加密托管 · API 永不回传明文"
        actions={<Button icon="add">新建密钥</Button>}
      />
      <div className="grid grid-cols-4 gap-gutter mb-space-xl">
        <StatCard label="托管密钥总量" value={`${secrets.length} / 50`} sub="上限 50" />
        <StatCard label="审计合规率" value="100%" accent="success" />
        <StatCard label="本周读取调用" value={secrets.reduce((a, s) => a + s.readCountWeek, 0) + ' 次'} />
        <StatCard
          label="过期风险"
          value={secrets.filter((s) => s.expiresAt).length + ' 项'}
          accent="warning"
          sub="9 天后到期"
        />
      </div>
      <DataTable
        rows={secrets}
        columns={[
          {
            key: 'name',
            header: '名称',
            render: (s) => (
              <span className="flex items-center gap-space-md">
                <Icon
                  name={s.type === 'ssh_key' ? 'key' : s.type === 'registry' ? 'badge' : 'variables'}
                  size={16}
                  className="text-on-surface-variant"
                />
                {s.name}
              </span>
            ),
          },
          { key: 'type', header: '类型', render: (s) => TYPE_TEXT[s.type] },
          {
            key: 'scope',
            header: '作用域',
            render: (s) => (s.scope === 'global' ? '全局' : <span className="font-mono text-body-sm">{s.scope}</span>),
          },
          {
            key: 'fp',
            header: '指纹',
            render: (s) => <span className="font-mono text-label-md text-on-surface-variant">{s.fingerprint}</span>,
          },
          {
            key: 'targets',
            header: '关联目标',
            render: (s) => (s.linkedTargets ? `${s.linkedTargets} 个部署目标` : '—'),
          },
          {
            key: 'expires',
            header: '过期风险',
            render: (s) =>
              s.expiresAt ? (
                <StatusBadge tone="warning">{Math.ceil((s.expiresAt - Date.now()) / 86400000)} 天后到期</StatusBadge>
              ) : (
                <span className="text-outline">—</span>
              ),
          },
          { key: 'reads', header: '本周读取', render: (s) => <span className="font-mono">{s.readCountWeek} 次</span> },
          {
            key: 'ops',
            header: '操作',
            align: 'right',
            render: (s) => (
              <div className="flex justify-end gap-space-md">
                <Button variant="ghost" size="sm" icon="content_copy" onClick={() => alert('已复制指纹')} />
                <Button variant="ghost" size="sm">
                  编辑
                </Button>
                <Button variant="ghost" size="sm" onClick={() => setConfirmDel(s)}>
                  删除
                </Button>
              </div>
            ),
          },
        ]}
      />
      <ConfirmDialog
        open={!!confirmDel}
        danger
        title="删除密钥"
        confirmText="删除"
        onConfirm={() => setConfirmDel(null)}
        onCancel={() => setConfirmDel(null)}
      >
        确认删除「{confirmDel?.name}」？关联的 {confirmDel?.linkedTargets || 0} 个部署目标将无法认证。此操作记入审计日志。
      </ConfirmDialog>
    </>
  )
}

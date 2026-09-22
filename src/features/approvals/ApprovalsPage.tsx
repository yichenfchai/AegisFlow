import { useQuery } from '@tanstack/react-query'
import { useState } from 'react'
import { api } from '@/api'
import { PageHeader, Button, EmptyState } from '@/components/ui/DataTable'
import { StatusBadge, LoaBadge } from '@/components/ui/Badge'
import { CountdownButton } from '@/components/ui/CountdownButton'
import type { Approval } from '@/api/types'
import { fmtRel } from '@/api/format'

export function ApprovalsPage() {
  const { data: approvals = [], refetch } = useQuery({ queryKey: ['approvals'], queryFn: api.approvals })
  const pending = approvals.filter((a) => a.status === 'pending')
  const decided = approvals.filter((a) => a.status !== 'pending')
  const [tab, setTab] = useState<'pending' | 'done'>('pending')

  const decide = (a: Approval, action: 'approve' | 'reject') => {
    alert(`${action === 'approve' ? '已批准' : '已驳回'}：${a.title}（演示 · 状态更替将记入审计）`)
    refetch()
  }

  return (
    <>
      <PageHeader
        title="审批中心"
        subtitle="LOA 3/4 治理关口待办 · 超时按策略自动裁决"
        actions={
          <span className="flex items-center gap-space-md text-body-sm text-on-surface-variant">
            <span className="font-mono text-warning">{pending.length}</span> 件需裁决
          </span>
        }
      />

      <div className="flex gap-space-md mb-space-xl">
        <button
          onClick={() => setTab('pending')}
          className={`px-space-xl py-space-md rounded-lg border text-body-md ${
            tab === 'pending' ? 'border-warning/50 bg-warning/10 text-warning' : 'border-surface-container-highest text-on-surface-variant'
          }`}
        >
          待审批 ({pending.length})
        </button>
        <button
          onClick={() => setTab('done')}
          className={`px-space-xl py-space-md rounded-lg border text-body-md ${
            tab === 'done' ? 'border-primary/50 bg-primary/10 text-primary' : 'border-surface-container-highest text-on-surface-variant'
          }`}
        >
          已处理 ({decided.length})
        </button>
      </div>

      {tab === 'pending' && (
        <div className="space-y-gutter max-w-[880px]">
          {pending.length === 0 && <EmptyState icon="task_alt" text="无待办 · 治理关口清空" />}
          {pending.map((a) => (
            <div key={a.id} className="bg-surface-container rounded-lg border border-surface-container-highest p-space-xl space-y-space-lg">
              <div className="flex items-start justify-between">
                <div>
                  <div className="flex items-center gap-space-md mb-space-sm">
                    <StatusBadge tone="pending">待审批</StatusBadge>
                    <LoaBadge loa={a.loa} label={`LOA ${a.loa} 审批`} />
                    <span className="text-label-sm text-text-3">{fmtRel(a.createdAt)}</span>
                  </div>
                  <h3 className="text-headline-md font-semibold">{a.title}</h3>
                </div>
              </div>

              <div className="grid grid-cols-2 gap-space-xl text-body-sm">
                <div className="space-y-space-md">
                  <Row label="触发人" value={a.triggerActor} />
                  {a.geoInfo && <Row label="来源" value={a.geoInfo} mono />}
                  <Row
                    label="推荐修复动作"
                    value={<span className="text-primary">{a.recommended}</span>}
                  />
                </div>
                <div>
                  <div className="text-label-sm text-outline mb-space-sm">证据锚点</div>
                  <div className="flex flex-wrap gap-space-md">
                    {a.evidenceRefs.map((ref) => (
                      <button key={ref} className="font-mono text-label-md text-brand bg-brand/10 rounded px-space-md py-0.5 hover:bg-brand/20">
                        {ref}
                      </button>
                    ))}
                  </div>
                </div>
              </div>

              <div className="bg-surface-container-low rounded p-space-lg">
                <div className="text-label-sm text-outline mb-space-sm">审计声明</div>
                <p className="text-body-sm text-on-surface-variant">
                  {a.autoOnExpire === 'approve'
                    ? '15 分钟后自动放行（安全回滚基线）· 冷却至 22:00'
                    : '29:48 后自动驳回 · 状态更替将记入审计日志'}
                </p>
                <div className="mt-space-md">
                  <CountdownButton
                    totalSec={a.countdownSec}
                    remainingSec={Math.max(0, a.countdownSec - Math.floor((Date.now() - a.createdAt) / 1000))}
                    label={a.autoOnExpire === 'approve' ? '后自动放行' : '后自动驳回'}
                    onExpire={() => refetch()}
                  />
                </div>
              </div>

              <div className="flex justify-end gap-space-md">
                <Button variant="danger" icon="close" onClick={() => decide(a, 'reject')}>
                  驳回
                </Button>
                <Button icon="check" onClick={() => decide(a, 'approve')}>
                  批准
                </Button>
              </div>
            </div>
          ))}
        </div>
      )}

      {tab === 'done' && (
        <div className="space-y-gutter max-w-[880px]">
          {decided.map((a) => (
            <div key={a.id} className="bg-surface-container rounded-lg border border-surface-container-highest p-space-lg flex items-center justify-between">
              <div className="flex items-center gap-space-md">
                <StatusBadge tone={a.status.includes('approve') || a.status === 'approved' ? 'success' : 'error'}>
                  {a.status === 'approved' ? '已批准' : a.status === 'rejected' ? '已驳回' : a.status === 'auto_approved' ? '自动放行' : '自动驳回'}
                </StatusBadge>
                <span className="text-body-md">{a.title}</span>
              </div>
              <span className="text-label-sm text-text-3">{fmtRel(a.createdAt)}</span>
            </div>
          ))}
        </div>
      )}
    </>
  )
}

function Row({ label, value, mono }: { label: string; value: React.ReactNode; mono?: boolean }) {
  return (
    <div className="flex items-center gap-space-lg">
      <span className="text-outline shrink-0 w-24">{label}</span>
      <span className={`${mono ? 'font-mono' : ''} text-on-surface`}>{value}</span>
    </div>
  )
}

import { useQuery } from '@tanstack/react-query'
import { useState } from 'react'
import { api } from '@/api'
import { DataTable, PageHeader, Button } from '@/components/ui/DataTable'
import { Segmented } from '@/components/ui/Badge'
import { LoaBadge, StatCard } from '@/components/ui/Badge'
import { Icon } from '@/components/ui/Icon'
import type { AuditLog } from '@/api/types'
import { fmtRel } from '@/api/format'

const RESULT_TEXT: Record<AuditLog['result'], string> = {
  success: '成功',
  failed: '失败',
  retry: '成功 (重试#2)',
  rolled_back: '成功 (回滚)',
  escalated: '已转审批',
  circuit_broken: '熔断触发',
}
const RESULT_CLS: Record<AuditLog['result'], string> = {
  success: 'text-secondary',
  failed: 'text-error',
  retry: 'text-primary',
  rolled_back: 'text-warning',
  escalated: 'text-warning',
  circuit_broken: 'text-error',
}

type Domain = 'all' | 'build' | 'deploy' | 'review' | 'defense' | 'ops'

/** 自动化审计（_5 稿） */
export function AuditPage() {
  const { data: logs = [] } = useQuery({ queryKey: ['audit'], queryFn: api.auditLogs })
  const [domain, setDomain] = useState<Domain>('all')
  const [detail, setDetail] = useState<AuditLog | null>(null)

  const filtered = domain === 'all' ? logs : logs.filter((l) => l.domain === domain)

  return (
    <>
      <PageHeader
        title="自动化审计"
        subtitle="全部自动化动作流水 · 证据链可导出"
        actions={
          <>
            <Button variant="secondary" icon="description" onClick={() => alert('昨夜托管报告（演示）')}>
              昨夜托管报告
            </Button>
            <Button icon="download" onClick={() => alert('审计 JSON 已导出（演示）')}>
              导出 JSON
            </Button>
          </>
        }
      />

      <div className="grid grid-cols-4 gap-gutter mb-space-xl">
        <StatCard label="今日自动执行" value={41} unit="次" sub="预算余量 155 / 200" />
        <StatCard label="自动止血成功率" value="92.7%" accent="success" sub="修复重跑通过 3 次" />
        <StatCard label="已推送告警" value={4} accent="warning" sub="健康失败即刻回退" />
        <StatCard label="需要人工介入" value={2} accent="error" sub="已转 LOA 4 审批" />
      </div>

      <div className="flex items-center justify-between mb-space-lg">
        <Segmented
          value={domain}
          onChange={setDomain}
          options={[
            { value: 'all' as const, label: '全部' },
            { value: 'build' as const, label: '构建' },
            { value: 'deploy' as const, label: '部署' },
            { value: 'defense' as const, label: '防御' },
            { value: 'ops' as const, label: '运维' },
          ]}
        />
        <span className="text-label-md text-outline">
          近 24 小时 · LOA 档位 <LoaBadge loa={3} /> 阈值信任为主
        </span>
      </div>

      <DataTable
        rows={filtered}
        columns={[
          { key: 'time', header: '时间', render: (l) => <span className="font-mono text-body-sm">{fmtRel(l.ts)}</span> },
          { key: 'domain', header: '域', render: (l) => <span className="text-on-surface-variant">{l.domain}</span> },
          {
            key: 'action',
            header: '动作 (Action)',
            render: (l) => (
              <div>
                <div>{l.action}</div>
                <div className="text-label-sm text-outline truncate max-w-[320px]">{l.detail}</div>
              </div>
            ),
          },
          { key: 'actor', header: '执行者', render: (l) => <span className="font-mono text-label-md">{l.actor}</span> },
          { key: 'loa', header: 'LOA', render: (l) => <LoaBadge loa={l.loa} /> },
          {
            key: 'result',
            header: '执行结果',
            render: (l) => <span className={`text-body-sm ${RESULT_CLS[l.result]}`}>{RESULT_TEXT[l.result]}</span>,
          },
          {
            key: 'ops',
            header: '详情',
            align: 'right',
            render: (l) => (
              <button className="text-body-sm text-primary hover:underline" onClick={() => setDetail(l)}>
                证据链
              </button>
            ),
          },
        ]}
      />

      {detail && (
        <div className="fixed inset-0 z-overlay bg-black/60" onClick={() => setDetail(null)}>
          <div
            className="absolute right-0 top-0 bottom-0 w-[440px] bg-surface-container border-l border-surface-container-highest p-space-xl overflow-y-auto"
            onClick={(e) => e.stopPropagation()}
          >
            <div className="flex items-center justify-between mb-space-lg">
              <h2 className="text-headline-md font-semibold">证据链</h2>
              <button onClick={() => setDetail(null)} className="text-outline hover:text-on-surface">
                <Icon name="close" size={20} />
              </button>
            </div>
            <p className="text-label-sm text-outline mb-space-xl">al-{detail.id} · 触发源 {detail.actor} · 全链路证据链关联</p>
            <div className="space-y-0">
              {[
                ['触发', `${detail.domain} · ${detail.detail}`],
                ['档位仲裁', `LOA ${detail.loa} · Gate 链通过（预算 ✓ 阈值 ✓）`],
                ['执行结果', RESULT_TEXT[detail.result]],
                ['日志锚点', '→ 容器执行日志 L118 · L124 · L45'],
                ['关联实体', '构建 #1091 · 部署单 DP-311'],
              ].map(([t, d], i, arr) => (
                <div key={t} className="flex gap-space-lg">
                  <div className="flex flex-col items-center">
                    <span className="w-2 h-2 rounded-full bg-primary mt-1.5" />
                    {i < arr.length - 1 && <span className="w-px flex-1 bg-surface-container-highest my-1" />}
                  </div>
                  <div className="pb-space-xl">
                    <div className="text-label-md text-outline mb-0.5">{t}</div>
                    <div className="text-body-md text-on-surface">{d}</div>
                  </div>
                </div>
              ))}
            </div>
            <div className="bg-surface-container-low rounded p-space-lg font-mono text-label-sm text-outline mt-space-md">
              HMAC 链校验: a3f8…e21c → 7be0…d9f2 ✓ 完整性验证通过
            </div>
          </div>
        </div>
      )}
    </>
  )
}

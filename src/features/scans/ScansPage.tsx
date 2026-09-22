import { useQuery } from '@tanstack/react-query'
import { useState } from 'react'
import { api } from '@/api'
import { PageHeader, Button } from '@/components/ui/DataTable'
import { Segmented } from '@/components/ui/Badge'
import { StatCard } from '@/components/ui/Badge'
import { Icon } from '@/components/ui/Icon'
import type { ScanFinding } from '@/api/types'

const SEV_TEXT: Record<ScanFinding['severity'], string> = { high: '高危', medium: '中危', low: '低危', nit: '风格' }
const SEV_CLS: Record<ScanFinding['severity'], string> = {
  high: 'text-error',
  medium: 'text-warning',
  low: 'text-primary',
  nit: 'text-outline',
}

export function ScansPage() {
  const { data: findings = [] } = useQuery({ queryKey: ['scans'], queryFn: api.scanFindings })
  const [sev, setSev] = useState<'all' | ScanFinding['severity']>('all')
  const [exemptOnly, setExemptOnly] = useState(false)

  const filtered = findings.filter(
    (f) => (sev === 'all' || f.severity === sev) && (!exemptOnly || !f.exempted),
  )
  const count = (s: ScanFinding['severity']) => findings.filter((f) => f.severity === s).length

  return (
    <>
      <PageHeader
        title="安全扫描"
        subtitle="引擎: Semgrep + AST 污点跟踪 + 依赖审计 · 增量 4.2s"
        actions={
          <>
            <Button variant="secondary" icon="history">
              审计追踪日志
            </Button>
            <Button icon="radar" onClick={() => alert('增量扫描已触发（演示）')}>
              立即扫描
            </Button>
          </>
        }
      />

      <div className="grid grid-cols-4 gap-gutter mb-space-xl">
        <StatCard label="高危" value={count('high')} accent="error" sub="阻断模式 · 需修复/豁免" />
        <StatCard label="中危" value={count('medium')} accent="warning" sub="待跟进闭环" />
        <StatCard label="低危 / Nit" value={count('low') + count('nit')} sub="非阻断建议" />
        <StatCard label="内置规则" value="184 条" sub="有效规则 · 代码覆盖 4 个组件" />
      </div>

      <div className="flex items-center justify-between mb-space-lg">
        <Segmented
          value={sev}
          onChange={setSev}
          options={[
            { value: 'all' as const, label: '全部' },
            { value: 'high' as const, label: '高危' },
            { value: 'medium' as const, label: '中危' },
            { value: 'low' as const, label: '低危' },
            { value: 'nit' as const, label: '风格' },
          ]}
        />
        <button
          onClick={() => setExemptOnly((v) => !v)}
          className={`flex items-center gap-space-sm text-body-sm rounded px-space-lg py-1 border ${
            exemptOnly ? 'border-primary/50 text-primary bg-primary/10' : 'border-surface-container-highest text-on-surface-variant'
          }`}
        >
          <Icon name={exemptOnly ? 'filter_alt' : 'filter_alt_off'} size={14} />
          仅看未豁免
        </button>
      </div>

      <div className="space-y-gutter">
        {filtered.map((f) => (
          <div
            key={f.id}
            className="bg-surface-container rounded-lg border border-surface-container-highest p-space-lg flex items-center gap-space-xl"
          >
            <span className={`font-mono text-label-md font-semibold w-12 ${SEV_CLS[f.severity]}`}>
              {SEV_TEXT[f.severity]}
            </span>
            <div className="flex-1 min-w-0">
              <div className="text-body-md">{f.title}</div>
              <div className="text-label-sm text-outline font-mono">
                {f.filePath}
                {f.line ? `:${f.line}` : ''} · {f.ruleId}
              </div>
            </div>
            {f.exempted ? (
              <span className="flex items-center gap-space-sm text-label-md text-outline">
                <Icon name="shield_moon" size={14} />
                已豁免
              </span>
            ) : (
              <div className="flex gap-space-md">
                <Button size="sm" variant="ghost">
                  在 diff 中查看
                </Button>
                <Button size="sm" variant="secondary" onClick={() => alert('豁免申请已提交（演示）')}>
                  申请豁免
                </Button>
              </div>
            )}
          </div>
        ))}
      </div>

      <p className="text-label-sm text-outline mt-space-lg">
        规则引擎: 全部 · 审计追踪: 近 24h 无策略变更 · 扫描模式: 阻断（高危卡点）
      </p>
    </>
  )
}

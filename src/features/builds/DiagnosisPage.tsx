import { useParams } from 'react-router'
import { PageHeader, Button } from '@/components/ui/DataTable'
import { ConfidenceMeter, LoaBadge } from '@/components/ui/Badge'
import { Icon } from '@/components/ui/Icon'
import { useEffect, useState } from 'react'

type Phase = 'analyzing' | 'done'

/** AI 诊断面板（1091_ai_2 稿）：置信度流式增长 → 根因 → 修复建议 → 反馈 */
export function DiagnosisPage() {
  const { id } = useParams()
  const [phase, setPhase] = useState<Phase>('analyzing')
  const [confidence, setConfidence] = useState(0)

  useEffect(() => {
    // 模拟置信度流式增长
    let v = 0
    const t = setInterval(() => {
      v += 0.06
      setConfidence(Math.min(0.75, v))
      if (v >= 0.75) {
        clearInterval(t)
        setPhase('done')
      }
    }, 160)
    return () => clearInterval(t)
  }, [])

  return (
    <>
      <PageHeader
        title={`AI 诊断 · 构建 #${id}`}
        subtitle="全新推理 · 未命中知识条目 · 分析耗时 8.4s"
        actions={
          <>
            <Button variant="secondary" onClick={() => alert('重试构建（演示）')}>
              重试构建
            </Button>
            <Button icon="smart_toy">应用修复建议</Button>
          </>
        }
      />

      <div className="grid grid-cols-[1fr_320px] gap-gutter">
        <div className="space-y-gutter">
          {/* 置信度 */}
          <div className="bg-surface-container rounded-lg border border-surface-container-highest p-space-xl">
            <div className="flex items-center justify-between mb-space-md">
              <span className="text-headline-sm font-semibold flex items-center gap-space-sm">
                <Icon name="smart_toy" size={16} className="text-brand" />
                匹配置信度
                {phase === 'analyzing' && (
                  <span className="text-label-sm text-brand gp-spin">AI 代理活跃中</span>
                )}
              </span>
              <LoaBadge loa={1} label="仅建议" />
            </div>
            <ConfidenceMeter value={confidence} />
            <p className="text-label-sm text-outline mt-space-sm">
              当前置信进度: {Math.round(confidence * 100)}% · 阈值 75% 后可建议修复动作
            </p>
          </div>

          {/* 原始日志锚点 */}
          <div className="bg-surface-container rounded-lg border border-surface-container-highest p-space-xl">
            <h3 className="text-headline-sm font-semibold mb-space-md">原始日志锚点</h3>
            <div className="space-y-space-md font-mono text-[12px]">
              {[
                ['L241', "src/auth/client.ts:12:34 - error TS2571: Object is of type 'undefined'."],
                ['L242', '12   const kid = payload.claims[kid];'],
                ['L248', 'Failed to compile. Exit code 1.'],
              ].map(([ln, text]) => (
                <div key={ln} className="flex items-center gap-space-lg bg-surface-container-lowest rounded p-space-md">
                  <button className="text-brand shrink-0 hover:underline">→ 日志 {ln}</button>
                  <span className="text-error">{text}</span>
                </div>
              ))}
            </div>
            <button className="text-label-md text-primary mt-space-md hover:underline">查看完整原始日志</button>
          </div>

          {/* 根因 + 建议 */}
          {phase === 'done' && (
            <>
              <div className="bg-surface-container rounded-lg border border-surface-container-highest p-space-xl">
                <h3 className="text-headline-sm font-semibold mb-space-md">根因诊断</h3>
                <p className="text-body-md text-on-surface-variant leading-relaxed">
                  TokenParser 在解析 JWT 时，<span className="font-mono text-error text-[12px]">payload.claims</span>{' '}
                  切片在缺失 <span className="font-mono text-[12px]">kid</span> 头的情况下为 undefined，
                  直接索引导致 TS2571。类构造签名要求 claims 非空，但调用方未做空值防护，最近由成员
                  Alex Mercer 3 天前在 <span className="font-mono text-[12px]">client.ts</span> 引入。
                </p>
              </div>
              <div className="bg-surface-container rounded-lg border border-surface-container-highest p-space-xl">
                <h3 className="text-headline-sm font-semibold mb-space-md">修复建议</h3>
                <pre className="bg-surface-container-lowest rounded p-space-lg font-mono text-[12px] leading-relaxed overflow-x-auto">
                  <span className="text-outline select-none mr-3">1</span>
                  <span className="text-primary">const</span> claims = payload?.claims{' '}
                  <span className="text-error">??</span> <span className="text-brand">throwClaimsMissing</span>(payload);{'\n'}
                  <span className="text-outline select-none mr-3">2</span>
                  <span className="text-primary">const</span> kid = claims[kid];
                </pre>
                <div className="flex items-center gap-space-md mt-space-md text-label-md text-outline">
                  <Icon name="auto_fix_high" size={14} className="text-brand" />
                  +1 -0 变更安全 · 该模式与本团队已验证 5 次的同类修复一致
                </div>
              </div>
            </>
          )}
        </div>

        {/* 右列：诊断元信息 + 反馈 */}
        <div className="space-y-gutter">
          <div className="bg-surface-container rounded-lg border border-surface-container-highest p-space-xl space-y-space-lg">
            <h3 className="text-headline-sm font-semibold">诊断元信息</h3>
            <MetaRow label="推理模式" value="全新推理（未命中 kb）" />
            <MetaRow label="分析耗时" value="8.4s" mono />
            <MetaRow label="模型" value="deepseek-chat" mono />
            <MetaRow label="退出码" value="1" mono error />
            <MetaRow label="相似历史构建" value="#1078 · #1051 · #1044" mono />
          </div>

          {phase === 'done' && (
            <div className="bg-surface-container rounded-lg border border-surface-container-highest p-space-xl">
              <h3 className="text-headline-sm font-semibold mb-space-md">该诊断是否准确？</h3>
              <div className="grid grid-cols-3 gap-space-md">
                {[
                  ['准确', 'thumb_up', 'secondary'],
                  ['部分准确', 'rule', 'warning'],
                  ['不准确', 'thumb_down', 'error'],
                ].map(([label, icon, color]) => (
                  <button
                    key={label}
                    onClick={() => alert(`反馈已提交：${label} · 将回流知识库（演示）`)}
                    className={`flex flex-col items-center gap-space-sm rounded border py-space-lg transition-colors ${
                      color === 'secondary'
                        ? 'border-secondary/40 text-secondary hover:bg-secondary/10'
                        : color === 'warning'
                          ? 'border-warning/40 text-warning hover:bg-warning/10'
                          : 'border-error/40 text-error hover:bg-error/10'
                    }`}
                  >
                    <Icon name={icon} size={18} />
                    <span className="text-body-sm">{label}</span>
                  </button>
                ))}
              </div>
              <p className="text-label-sm text-outline mt-space-md">
                人工修改 · 79% 的同类诊断最终被采纳 · 反馈将作为知识条目验证依据
              </p>
            </div>
          )}

          {phase === 'done' && (
            <div className="bg-surface-container rounded-lg border border-surface-container-highest p-space-xl">
              <h3 className="text-headline-sm font-semibold mb-space-md">LOA 治理状态</h3>
              <div className="space-y-space-md">
                <MetaRow label="自动化自治评级" value="L2 当前档位" />
                <MetaRow label="升级条件" value="命中已验证规则 ≥1 次后可升档" />
                <div className="flex items-center gap-space-sm text-label-md text-outline">
                  <LoaBadge loa={4} />
                  <Icon name="arrow_forward" size={12} />
                  <LoaBadge loa={7} />
                  <span className="ml-1">100% (确定性规则)</span>
                </div>
              </div>
            </div>
          )}
        </div>
      </div>
    </>
  )
}

function MetaRow({ label, value, mono, error }: { label: string; value: string; mono?: boolean; error?: boolean }) {
  return (
    <div className="flex items-center justify-between text-body-sm">
      <span className="text-outline">{label}</span>
      <span className={`${mono ? 'font-mono' : ''} ${error ? 'text-error' : 'text-on-surface'}`}>{value}</span>
    </div>
  )
}

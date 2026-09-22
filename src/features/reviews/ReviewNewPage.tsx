import { PageHeader, Button } from '@/components/ui/DataTable'
import { StatusBadge } from '@/components/ui/Badge'
import { Icon } from '@/components/ui/Icon'
import { useSSE } from '@/api/sse'
import { useState } from 'react'

/** 提交审查请求（_2 稿）：分叉点检测 + AI 代理实时判定 */
export function ReviewNewPage() {
  const [source, setSource] = useState('fix-tls')
  const [started, setStarted] = useState(false)
  const [progress, setProgress] = useState({ batches: 0, total: 19, verified: 0, verifiedTotal: 8 })

  const { connected } = useSSE(
    started ? 'mock://review' : null,
    (emit) => {
      let b = 0
      let v = 0
      const t = setInterval(() => {
        b = Math.min(19, b + 1)
        if (b % 3 === 0) v = Math.min(8, v + 1)
        setProgress({ batches: b, total: 19, verified: v, verifiedTotal: 8 })
        emit('ai', { batches: b, verified: v })
        if (b >= 19) {
          clearInterval(t)
          emit('status', { done: true })
        }
      }, 280)
      return () => clearInterval(t)
    },
  )

  const done = progress.batches >= progress.total

  return (
    <>
      <PageHeader
        title="提交审查请求"
        subtitle="AI 代理联合审查 · 分叉点检测与变更集分析"
        actions={
          <Button icon="play_arrow" onClick={() => setStarted(true)} disabled={started}>
            {started ? '审查运行中...' : '开始审查'}
          </Button>
        }
      />

      <div className="grid grid-cols-[380px_1fr] gap-gutter">
        {/* 左：请求配置 */}
        <div className="bg-surface-container rounded-lg border border-surface-container-highest p-space-xl space-y-space-lg">
          <h3 className="text-headline-sm font-semibold">新建审查请求</h3>
          <div>
            <label className="block text-label-md text-on-surface-variant mb-space-sm">合并目标 · 基线分支 (Base)</label>
            <div className="bg-surface-container-lowest border border-surface-container-highest rounded px-space-lg py-space-md font-mono text-body-sm">
              main
            </div>
          </div>
          <div>
            <label className="block text-label-md text-on-surface-variant mb-space-sm">源分支</label>
            <select
              value={source}
              onChange={(e) => setSource(e.target.value)}
              className="w-full bg-surface-container-lowest border border-surface-container-highest rounded px-space-lg py-space-md font-mono text-body-sm cursor-pointer"
            >
              <option>fix-tls</option>
              <option>feat-lock</option>
              <option>grpc-up</option>
            </select>
          </div>
          <div>
            <label className="block text-label-md text-on-surface-variant mb-space-sm">变更意图说明</label>
            <textarea
              rows={3}
              defaultValue="refresh token 的 kid 头缺失时未做空值防护，补充显式失败路径。"
              className="w-full bg-surface-container-lowest border border-surface-container-highest rounded px-space-lg py-space-md text-body-md focus:outline-none focus:border-primary/60 resize-none"
            />
          </div>
          {/* 分叉点 */}
          <div className="bg-surface-container-low rounded p-space-lg font-mono text-[12px] space-y-1">
            <div className="flex items-center gap-space-sm text-outline">
              <Icon name="call_split" size={14} />
              ← 分叉点 a3f8c21 · 1 小时前
            </div>
            <div className="flex items-center gap-space-sm text-secondary">
              <Icon name="check" size={14} />
              检测到变更集 · 无未解决冲突
            </div>
          </div>
        </div>

        {/* 右：检查项与 AI 进度 */}
        <div className="space-y-gutter">
          <div className="bg-surface-container rounded-lg border border-surface-container-highest p-space-xl">
            <h3 className="text-headline-sm font-semibold mb-space-lg">检查项</h3>
            <div className="space-y-space-md">
              {[
                ['分支对齐', 'feat-lock → main 分支流动正常'],
                ['无未解决冲突', '与基线无冲突文件'],
                ['基线校验一致', '基线未前移'],
                ['闸门状态', '阻断拦截就绪'],
              ].map(([t, d]) => (
                <div key={t} className="flex items-center gap-space-md text-body-md">
                  <Icon name="check_circle" size={16} className="text-secondary" fill />
                  <span className="font-medium">{t}</span>
                  <span className="text-label-sm text-outline">{d}</span>
                </div>
              ))}
            </div>
          </div>

          <div className="bg-surface-container rounded-lg border border-surface-container-highest p-space-xl">
            <div className="flex items-center justify-between mb-space-lg">
              <h3 className="text-headline-sm font-semibold flex items-center gap-space-sm">
                <Icon name="smart_toy" size={16} className="text-brand" />
                AI 代理
                {started && !done && <span className="text-label-sm text-brand gp-spin">AI 代理活跃中</span>}
                {done && <StatusBadge tone="success">全部通过 (3/3)</StatusBadge>}
              </h3>
              {connected && <span className="font-mono text-label-sm text-outline">SSE 已连接</span>}
            </div>

            {!started ? (
              <p className="text-body-md text-outline py-space-xl text-center">
                配置源分支后点击「开始审查」启动 AI 判定
              </p>
            ) : (
              <div className="space-y-space-lg">
                <ProgressRow
                  label="变更集分批判定"
                  value={`${progress.batches} / ${progress.total} 批次`}
                  pct={(progress.batches / progress.total) * 100}
                />
                <ProgressRow
                  label="安全与基线验证"
                  value={`${progress.verified} / ${progress.verifiedTotal} 验证`}
                  pct={(progress.verified / progress.verifiedTotal) * 100}
                />
                {done && (
                  <div className="bg-secondary/10 rounded p-space-lg text-body-sm text-secondary flex items-center gap-space-md">
                    <Icon name="verified" size={16} />
                    审查完成：平均判定延时 4.1s · 无 blocker · 2 条 nit 建议
                  </div>
                )}
              </div>
            )}
          </div>
        </div>
      </div>
    </>
  )
}

function ProgressRow({ label, value, pct }: { label: string; value: string; pct: number }) {
  return (
    <div>
      <div className="flex items-center justify-between text-body-sm mb-space-sm">
        <span className="text-on-surface-variant">{label}</span>
        <span className="font-mono text-label-md text-outline">{value}</span>
      </div>
      <div className="h-[3px] rounded-full bg-surface-2 overflow-hidden">
        <div
          className="h-full rounded-full bg-brand transition-all duration-300"
          style={{ width: `${pct}%` }}
        />
      </div>
    </div>
  )
}

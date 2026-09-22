import { useEffect, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Link, useParams } from 'react-router'
import { api } from '@/api'
import { startLogStream } from '@/api/mock/logSimulator'
import { PageHeader, Button } from '@/components/ui/DataTable'
import { StatusBadge, ConfidenceMeter } from '@/components/ui/Badge'
import { Icon } from '@/components/ui/Icon'
import { LogViewer } from '@/components/domain/LogViewer'
import { StageTimeline } from '@/components/domain/StageTimeline'
import { fmtRel } from '@/api/format'
import type { BuildStage, LogLine } from '@/api/types'

/** 薄 hook：mock 层日志流的 React 绑定（接真实后端后改走 useSSE） */
function useLogSimulator(buildId: string, failed: boolean) {
  const [lines, setLines] = useState<LogLine[]>([])

  useEffect(() => {
    setLines([])
    return startLogStream(failed, (line) => setLines((prev) => [...prev, line]))
  }, [buildId, failed])

  return lines
}

export function BuildDetailPage() {
  const { id } = useParams()
  const { data: builds = [] } = useQuery({ queryKey: ['builds'], queryFn: api.builds })
  const build = builds.find((b) => b.number === Number(id)) ?? builds[0]
  const failed = build?.status === 'failed'

  const lines = useLogSimulator(build?.id ?? '', failed)

  if (!build) return null

  return (
    <>
      <PageHeader
        title={`构建 #${build.number}`}
        subtitle={`${build.triggerDetail} · 分支 ${build.branch} · ${fmtRel(build.createdAt)}`}
        actions={
          <>
            {failed && (
              <Button icon="play_arrow" onClick={() => alert('重试构建 · 走 Governor 裁决（演示）')}>
                重新构建 (Retry)
              </Button>
            )}
            <Button variant="secondary" icon="code">
              查看提交 {build.commitSha}
            </Button>
          </>
        }
      />

      <div className="grid grid-cols-[340px_1fr] gap-gutter">
        {/* 左列：状态 + Stage 时间线 */}
        <div className="space-y-gutter">
          <div className="bg-surface-container rounded-lg border border-surface-container-highest p-space-xl space-y-space-lg">
            <div className="flex items-center justify-between">
              <StatusBadge
                tone={build.status === 'success' ? 'success' : build.status === 'failed' ? 'error' : 'running'}
              >
                {build.status === 'success' ? '运行中 #' + build.number : build.status === 'failed' ? `#${build.number} (失败)` : `运行中 #${build.number}`}
              </StatusBadge>
              <span className="font-mono text-label-sm text-outline">{build.commitSha}</span>
            </div>
            <StageTimeline stages={failed ? failedStages : runningStages} />
          </div>

          {failed && (
            <div className="bg-surface-container rounded-lg border border-surface-container-highest p-space-xl">
              <div className="flex items-center gap-space-sm mb-space-md">
                <Icon name="smart_toy" size={16} className="text-brand" />
                <span className="text-headline-sm font-semibold">AI 诊断</span>
                <span className="text-label-sm bg-brand/12 text-brand rounded px-1">
                  LOA 1 仅建议
                </span>
              </div>
              <ConfidenceMeter value={0.75} className="mb-space-md" />
              <p className="text-body-sm text-on-surface-variant leading-relaxed mb-space-lg">
                TokenParser 索引越界：<span className="text-error font-mono text-[12px]">client.ts:12</span> 处
                payload.claims 在缺失 kid 头时为 undefined，未做空值防护。命中规则库候选条目，置信度 75%。
              </p>
              <Link
                to={`/builds/${build.number}/diagnosis`}
                className="flex items-center justify-center gap-space-sm rounded border border-brand/40 text-brand py-space-md text-body-md hover:bg-brand/10 transition-colors"
              >
                <Icon name="smart_toy" size={16} />
                查看 AI 诊断
              </Link>
            </div>
          )}
        </div>

        {/* 右列：日志流 */}
        <LogViewer lines={lines} running={!failed} highlightSeq={failed ? 13 : undefined} maxHeight={560} />
      </div>
    </>
  )
}

const runningStages: BuildStage[] = [
  { seq: 1, name: 'Checkout', status: 'success', startedAt: 0, endedAt: 8000 },
  { seq: 2, name: 'Install', status: 'success', startedAt: 8000, endedAt: 42000 },
  { seq: 3, name: 'Build', status: 'running', startedAt: 42000 },
  { seq: 4, name: 'Deploy', status: 'pending' },
]
const failedStages: BuildStage[] = [
  { seq: 1, name: 'Checkout', status: 'success', startedAt: 0, endedAt: 8000 },
  { seq: 2, name: 'Install', status: 'success', startedAt: 8000, endedAt: 41000 },
  { seq: 3, name: 'Build', status: 'failed', startedAt: 41000, endedAt: 184000 },
  { seq: 4, name: 'Deploy', status: 'skipped' },
]

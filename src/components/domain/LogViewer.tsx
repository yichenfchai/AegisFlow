import { useEffect, useRef } from 'react'
import { Icon } from '@/components/ui/Icon'
import type { LogLine } from '@/api/types'

export type { LogLine }

/** 日志流查看器：虚拟行渲染、自动滚屏开关、行锚点 */
export function LogViewer({
  lines,
  running,
  highlightSeq,
  maxHeight = 420,
}: {
  lines: LogLine[]
  running?: boolean
  /** 高亮行（AI 锚点跳转） */
  highlightSeq?: number
  maxHeight?: number
}) {
  const boxRef = useRef<HTMLDivElement>(null)
  const autoScroll = useRef(true)

  useEffect(() => {
    if (autoScroll.current && boxRef.current) {
      boxRef.current.scrollTop = boxRef.current.scrollHeight
    }
  }, [lines.length])

  return (
    <div className="relative bg-surface-container-lowest rounded-lg border border-surface-container-highest overflow-hidden">
      {/* 工具条 */}
      <div className="flex items-center justify-between px-space-lg py-space-sm border-b border-surface-container-highest bg-surface-container-low">
        <div className="flex items-center gap-space-md text-label-md text-outline">
          <Icon name="terminal" size={14} />
          <span className="font-mono">bash — web-api</span>
          {running && (
            <span className="flex items-center gap-space-sm text-secondary">
              <span className="w-1.5 h-1.5 rounded-full bg-secondary gp-breathe" />
              流式写入保持活跃
            </span>
          )}
        </div>
        <div className="flex items-center gap-space-lg">
          <label className="flex items-center gap-space-sm text-label-md text-outline cursor-pointer select-none">
            <input
              type="checkbox"
              defaultChecked
              onChange={(e) => (autoScroll.current = e.target.checked)}
              className="accent-primary"
            />
            自动滚屏
          </label>
          <span className="font-mono text-label-md text-outline">{lines.length} 行</span>
        </div>
      </div>

      {/* 日志区 */}
      <div ref={boxRef} className="overflow-y-auto font-mono text-[12px] leading-[1.7]" style={{ maxHeight }}>
        {lines.map((l) => (
          <div
            key={l.seq}
            id={`log-line-${l.seq}`}
            className={`flex px-space-lg ${
              l.seq === highlightSeq
                ? 'bg-brand/10 border-l-2 border-brand'
                : l.stream === 'stderr'
                  ? 'hover:bg-error/5'
                  : 'hover:bg-surface-container/50'
            }`}
          >
            <span className="select-none text-outline/60 w-12 shrink-0 text-right pr-space-lg">{l.seq}</span>
            <span
              className={
                l.stream === 'stderr'
                  ? 'text-error'
                  : l.stream === 'system'
                    ? 'text-primary'
                    : 'text-on-surface-variant'
              }
            >
              {l.content}
            </span>
          </div>
        ))}
        {running && (
          <div className="px-space-lg py-1">
            <span className="inline-block w-2 h-4 bg-secondary gp-breathe" />
          </div>
        )}
      </div>

      {/* 底部统计栏 */}
      <div className="flex items-center justify-between px-space-lg py-space-sm border-t border-surface-container-highest bg-surface-container-low text-label-sm text-outline font-mono">
        <span>{stats(lines)}</span>
        <span>UTF-8 · LF</span>
      </div>
    </div>
  )
}

function stats(lines: LogLine[]) {
  const err = lines.filter((l) => l.stream === 'stderr').length
  const first = lines.find((l) => l.stream === 'stderr')
  return (
    <>
      共 {lines.length} 行 · stderr {err} {first ? `· 首个错误 L${first.seq}` : ''}
    </>
  )
}

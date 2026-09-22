import { Icon } from '@/components/ui/Icon'
import type { BuildStage } from '@/api/types'

/** 构建 Stage 时间线 */
export function StageTimeline({ stages }: { stages: BuildStage[] }) {
  const ICONS: Record<BuildStage['status'], { icon: string; cls: string }> = {
    success: { icon: 'check_circle', cls: 'text-secondary' },
    failed: { icon: 'cancel', cls: 'text-error' },
    running: { icon: 'pending', cls: 'text-primary gp-breathe' },
    pending: { icon: 'radio_button_unchecked', cls: 'text-outline' },
    skipped: { icon: 'remove_circle_outline', cls: 'text-outline' },
  }

  return (
    <div className="flex flex-col gap-0">
      {stages.map((s, i) => {
        const st = ICONS[s.status]
        const dur = s.startedAt !== undefined && s.endedAt !== undefined ? ((s.endedAt - s.startedAt) / 1000).toFixed(1) : null
        return (
          <div key={s.seq} className="flex items-stretch gap-space-lg">
            {/* 连线列 */}
            <div className="flex flex-col items-center w-5">
              <Icon name={st.icon} size={18} className={`${st.cls} mt-1.5`} fill={s.status === 'success'} />
              {i < stages.length - 1 && <div className="w-px flex-1 bg-surface-container-highest my-0.5" />}
            </div>
            {/* 内容 */}
            <div className={`flex-1 flex items-center justify-between pb-space-lg ${i === stages.length - 1 ? 'pb-0' : ''}`}>
              <div className="flex items-center gap-space-md">
                <span className={`text-body-md ${s.status === 'pending' || s.status === 'skipped' ? 'text-outline' : 'text-on-surface'}`}>
                  {s.name}
                </span>
                {s.status === 'running' && <span className="text-label-sm text-primary">进行中</span>}
                {s.status === 'failed' && (
                  <span className="text-label-sm bg-error-container/40 text-on-error-container rounded px-1">
                    Stage {s.seq}/4 [{s.name.toLowerCase()}] 阻断
                  </span>
                )}
              </div>
              {dur && <span className="font-mono text-label-md text-outline">{dur}s</span>}
            </div>
          </div>
        )
      })}
    </div>
  )
}

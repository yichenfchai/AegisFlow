export type BadgeTone = 'success' | 'error' | 'running' | 'pending' | 'neutral' | 'ai' | 'warning'

const TONES: Record<BadgeTone, { bg: string; text: string; breathe?: boolean }> = {
  success: { bg: 'bg-success/12', text: 'text-success' },
  error: { bg: 'bg-danger/12', text: 'text-danger' },
  running: { bg: 'bg-brand/12', text: 'text-brand', breathe: true },
  pending: { bg: 'bg-warning/12', text: 'text-warning' },
  warning: { bg: 'bg-warning/12', text: 'text-warning' },
  neutral: { bg: 'bg-surface-2', text: 'text-text-2' },
  ai: { bg: 'bg-brand/12', text: 'text-brand', breathe: true },
}

/** 状态徽章：成功绿/失败红/进行中蓝(呼吸)/待批准琥珀/AI=品牌蓝 */
export function StatusBadge({
  tone,
  children,
  dot = true,
  className = '',
}: {
  tone: BadgeTone
  children: React.ReactNode
  dot?: boolean
  className?: string
}) {
  const t = TONES[tone]
  return (
    <span
      className={`inline-flex items-center gap-space-sm rounded px-space-sm py-0.5 text-label-md ${t.bg} ${t.text} ${t.breathe ? 'gp-breathe' : ''} ${className}`}
    >
      {dot && <span className={`w-1.5 h-1.5 rounded-full bg-current ${t.breathe ? 'gp-breathe' : ''}`} />}
      {children}
    </span>
  )
}

/** 分段选择器 */
export function Segmented<T extends string>({
  options,
  value,
  onChange,
  className = '',
}: {
  options: { value: T; label: string }[]
  value: T
  onChange: (v: T) => void
  className?: string
}) {
  return (
    <div className={`inline-flex bg-surface rounded border border-border p-0.5 ${className}`}>
      {options.map((opt) => (
        <button
          key={opt.value}
          onClick={() => onChange(opt.value)}
          className={`px-space-lg py-1 rounded text-body-sm transition-colors ${
            value === opt.value
              ? 'bg-brand/12 text-brand font-medium'
              : 'text-on-surface-variant hover:text-on-surface'
          }`}
        >
          {opt.label}
        </button>
      ))}
    </div>
  )
}

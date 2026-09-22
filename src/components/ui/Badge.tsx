import { Icon } from './Icon'

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

const LOA_STYLES: Record<number, string> = {
  1: 'bg-surface-2 text-text-2 border-border',
  2: 'bg-warning/12 text-warning border-warning/40',
  3: 'bg-brand/12 text-brand border-brand/40',
  4: 'bg-success/12 text-success border-success/40',
  6: 'bg-brand/12 text-brand border-brand/40',
  7: 'bg-danger/12 text-danger border-danger/40',
}

/** LOA 档位徽章：L1灰/L2琥珀/L3蓝/L4绿/L6蓝/L7红 */
export function LoaBadge({ loa, label }: { loa: number; label?: string }) {
  return (
    <span
      className={`inline-flex items-center font-mono text-label-md rounded border px-space-sm py-0.5 ${LOA_STYLES[loa] ?? LOA_STYLES[1]}`}
    >
      L{loa}
      {label && <span className="ml-1 font-ui">{label}</span>}
    </span>
  )
}

/** 统计卡：大数字 + 可选环比箭头 */
export function StatCard({
  label,
  value,
  unit,
  trend,
  trendDir,
  sub,
  accent,
}: {
  label: string
  value: string | number
  unit?: string
  /** 环比文本，如 "3" 或 "-12.7%" */
  trend?: string
  trendDir?: 'up' | 'down'
  sub?: string
  accent?: 'default' | 'success' | 'warning' | 'error' | 'ai'
}) {
  const valueColor =
    accent === 'success'
      ? 'text-success'
      : accent === 'warning'
        ? 'text-warning'
        : accent === 'error'
          ? 'text-danger'
          : accent === 'ai'
            ? 'text-brand'
            : 'text-text'
  const trendColor =
    trendDir === 'up' ? 'text-success' : trendDir === 'down' ? 'text-danger' : 'text-text-2'
  return (
    <div className="bg-surface rounded-lg border border-border p-space-xl flex flex-col gap-space-sm">
      <div className="flex items-center justify-between">
        <span className="text-body-md text-text-2">{label}</span>
        {trend && (
          <span className={`text-label-md font-mono flex items-center gap-0.5 ${trendColor}`}>
            {trendDir === 'up' && <Icon name="arrow_upward" size={12} />}
            {trendDir === 'down' && <Icon name="arrow_downward" size={12} />}
            {trend}
          </span>
        )}
      </div>
      <div className="flex items-baseline gap-space-sm">
        <span className={`font-mono text-[28px] leading-9 font-semibold ${valueColor}`}>
          {value}
        </span>
        {unit && <span className="text-label-md text-outline">{unit}</span>}
      </div>
      {sub && <span className="text-label-sm text-outline">{sub}</span>}
    </div>
  )
}

/** 置信度/采纳率进度条（实色 3px + mono 百分比，AI 数字与条均为品牌蓝） */
export function ConfidenceMeter({ value, className = '' }: { value: number; className?: string }) {
  const pct = Math.round(value * 100)
  return (
    <div className={`flex items-center gap-space-md ${className}`}>
      <div className="flex-1 h-[3px] rounded-full bg-surface-2 overflow-hidden">
        <div
          className="h-full rounded-full bg-brand transition-all duration-500"
          style={{ width: `${pct}%` }}
        />
      </div>
      <span className="font-mono text-label-md text-brand">{pct}%</span>
    </div>
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

/** 拨动开关 */
export function Toggle({
  checked,
  onChange,
  disabled,
}: {
  checked: boolean
  onChange?: (v: boolean) => void
  disabled?: boolean
}) {
  return (
    <button
      role="switch"
      aria-checked={checked}
      disabled={disabled}
      onClick={() => onChange?.(!checked)}
      className={`w-9 h-5 rounded flex items-center px-0.5 transition-colors ${
        checked ? 'bg-success justify-end' : 'bg-surface-2 justify-start'
      } ${disabled ? 'opacity-40 cursor-not-allowed' : 'cursor-pointer'}`}
    >
      <span className="w-4 h-4 rounded-full bg-surface shadow-none" />
    </button>
  )
}

/** 展示格式化工具（与传输层无关，供任意页面使用） */

/** 相对时间：刚刚 / N 分钟前 / N 小时前 / 昨天 / N 天前 */
export function fmtRel(ts: number): string {
  const diff = Date.now() - ts
  if (diff < 60_000) return '刚刚'
  if (diff < 3_600_000) return `${Math.floor(diff / 60_000)} 分钟前`
  if (diff < 86_400_000) return `${Math.floor(diff / 3_600_000)} 小时前`
  if (diff < 2 * 86_400_000) return '昨天'
  return `${Math.floor(diff / 86_400_000)} 天前`
}

/** 毫秒 → "2m 38s" */
export function fmtDur(ms: number): string {
  const m = Math.floor(ms / 60_000)
  const s = Math.round((ms % 60_000) / 1000)
  return `${m}m ${String(s).padStart(2, '0')}s`
}

/** 秒 → "mm:ss"（不出现负数，超时钳到 00:00） */
export function fmtCountdown(totalSec: number): string {
  const s = Math.max(0, Math.floor(totalSec))
  const mm = String(Math.floor(s / 60)).padStart(2, '0')
  const ss = String(s % 60).padStart(2, '0')
  return `${mm}:${ss}`
}

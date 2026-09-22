import { useEffect, useRef, useState } from 'react'
import { fmtCountdown } from '@/api/format'

/** 审批倒计时按钮：<5min 转警示橙，到期自动裁决；剩余时间钳到 0，不出现负数 */
export function CountdownButton({
  totalSec,
  remainingSec,
  label,
  onExpire,
}: {
  totalSec: number
  remainingSec: number
  label: string
  onExpire?: () => void
}) {
  const [remain, setRemain] = useState(() => Math.max(0, remainingSec))
  const expiredRef = useRef(false)

  useEffect(() => {
    const init = Math.max(0, remainingSec)
    expiredRef.current = init === 0
    setRemain(init)
    // 已过期（如页面停留超过倒计时）：立即触发裁决，不渲染负数
    if (init === 0) {
      onExpireRef.current?.()
      return
    }
    const id = setInterval(() => {
      setRemain((r) => {
        if (r <= 1) {
          clearInterval(id)
          if (!expiredRef.current) {
            expiredRef.current = true
            onExpireRef.current?.()
          }
          return 0
        }
        return r - 1
      })
    }, 1000)
    return () => clearInterval(id)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [remainingSec])

  // onExpire 存 ref，避免父组件重渲染造成的闭包过期
  const onExpireRef = useRef(onExpire)
  onExpireRef.current = onExpire

  const urgent = remain < 300
  const pct = totalSec > 0 ? Math.max(0, (remain / totalSec) * 100) : 0

  return (
    <div className="flex items-center gap-space-md">
      <div className="flex-1 h-[3px] rounded-full bg-surface-2 overflow-hidden">
        <div
          className={`h-full transition-all duration-1000 ${urgent ? 'bg-warning' : 'bg-primary'}`}
          style={{ width: `${pct}%` }}
        />
      </div>
      <span
        className={`font-mono text-label-md ${urgent ? 'text-warning gp-breathe' : 'text-on-surface-variant'}`}
      >
        {fmtCountdown(remain)}
      </span>
      <span className="text-label-sm text-outline">{label}</span>
    </div>
  )
}

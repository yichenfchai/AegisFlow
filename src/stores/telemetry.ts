import { create } from 'zustand'
import { useEffect } from 'react'

interface TelemetryState {
  daemonOnline: boolean
  memory: string
  throughput: string
  cpu: string
  setTelemetry: (t: Partial<Omit<TelemetryState, 'setTelemetry'>>) => void
}

export const useTelemetryStore = create<TelemetryState>((set) => ({
  daemonOnline: true,
  memory: '32MB',
  throughput: '2.1 KB/s',
  cpu: '18.2%',
  setTelemetry: (t) => set(t),
}))

/** 批1先用定时器模拟遥测抖动；接后端后换 SSE（/api/v1/dashboard/telemetry/stream） */
export function useMockTelemetry() {
  const setTelemetry = useTelemetryStore((s) => s.setTelemetry)
  useEffect(() => {
    const id = setInterval(() => {
      const mem = 28 + Math.round(Math.random() * 10)
      const th = (1.6 + Math.random() * 1.2).toFixed(1)
      const cpu = (14 + Math.random() * 10).toFixed(1)
      setTelemetry({ memory: `${mem}MB`, throughput: `${th} KB/s`, cpu: `${cpu}%` })
    }, 3000)
    return () => clearInterval(id)
  }, [setTelemetry])
}

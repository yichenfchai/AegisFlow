import { useTelemetryStore } from '@/stores/telemetry'

export function StatusBar() {
  const { daemonOnline, memory, throughput, cpu } = useTelemetryStore()

  return (
    <footer className="fixed bottom-0 left-0 right-0 h-7 z-50 bg-surface border-t border-surface-container-highest flex items-center px-space-lg text-label-sm text-outline">
      <span className="flex items-center gap-space-sm">
        <span
          className={`w-2 h-2 rounded-full ${daemonOnline ? 'bg-secondary gp-breathe' : 'bg-error'}`}
        />
        {daemonOnline ? '守护进程运行中' : '守护进程离线'}
      </span>
      <span className="flex-1 text-center font-mono">solo-server</span>
      <span className="font-mono">
        内存 {memory} · 吞吐 {throughput} · CPU {cpu}
      </span>
    </footer>
  )
}

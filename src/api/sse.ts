/** SSE hook：mock 定时推送或真实 EventSource，接口一致 */
import { useEffect, useRef, useState } from 'react'

export interface SSEEvent {
  event: string
  data: unknown
}

export function useSSE(url: string | null, simulate?: (emit: (event: string, data: unknown) => void) => () => void) {
  const [events, setEvents] = useState<SSEEvent[]>([])
  const [connected, setConnected] = useState(false)
  const stopRef = useRef<(() => void) | null>(null)

  useEffect(() => {
    if (!url) return
    setConnected(true)

    if (simulate) {
      // mock 模式
      const stop = simulate((event, data) => {
        setEvents((prev) => [...prev.slice(-500), { event, data }])
      })
      stopRef.current = stop
      return () => {
        stopRef.current?.()
        setConnected(false)
      }
    }

    // 真实 SSE
    const es = new EventSource(url)
    es.onopen = () => setConnected(true)
    es.onerror = () => setConnected(false)
    ;['log', 'stage', 'status', 'ai', 'countdown', 'ping'].forEach((type) => {
      es.addEventListener(type, (e) => {
        try {
          const data = JSON.parse((e as MessageEvent).data)
          setEvents((prev) => [...prev.slice(-500), { event: type, data }])
        } catch {
          /* ignore */
        }
      })
    })
    return () => {
      es.close()
      setConnected(false)
    }
  }, [url])

  return { events, connected }
}

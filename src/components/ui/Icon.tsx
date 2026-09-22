import { useEffect, useState } from 'react'

let fontsReady = false
const listeners = new Set<() => void>()
if (typeof document !== 'undefined' && 'fonts' in document) {
  document.fonts.ready.then(() => {
    fontsReady = true
    listeners.forEach((fn) => fn())
  })
}

export function Icon({
  name,
  size = 18,
  fill = false,
  className = '',
}: {
  name: string
  size?: number
  fill?: boolean
  className?: string
}) {
  const [, force] = useState(0)
  useEffect(() => {
    if (fontsReady) return
    const fn = () => force((n) => n + 1)
    listeners.add(fn)
    return () => {
      listeners.delete(fn)
    }
  }, [])

  return (
    <span
      className={`material-symbols-outlined ${fontsReady ? 'msr-ready' : ''} ${className}`}
      style={{
        fontSize: size,
        fontVariationSettings: `'FILL' ${fill ? 1 : 0}, 'wght' 400, 'GRAD' 0, 'opsz' 20`,
        userSelect: 'none',
      }}
      aria-hidden
    >
      {name}
    </span>
  )
}

/** Accept same-origin paths only and prevent a return loop back to login. */
export function safeReturnTo(value: string | null): string {
  if (!value || !value.startsWith('/') || value.startsWith('//') || hasUnsafeCharacters(value)) return '/'
  try {
    const target = new URL(value, window.location.origin)
    const pathname = decodeURIComponent(target.pathname)
    if (target.origin !== window.location.origin || pathname.startsWith('//') || hasUnsafeCharacters(pathname) || /^\/login\/*$/i.test(pathname)) return '/'
    return `${target.pathname}${target.search}${target.hash}`
  } catch {
    return '/'
  }
}

function hasUnsafeCharacters(value: string): boolean {
  return Array.from(value).some((character) => {
    const code = character.charCodeAt(0)
    return character === '\\' || code <= 32 || code === 127
  })
}

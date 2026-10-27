/** Server-Sent Events framing, split from the authenticated client so it can be
 * tested without a session or a network. Chunks may split frames arbitrarily. */
export function createFrameParser(onFrame: (event: string, data: string) => void) {
  let buffer = ''
  return {
    push(chunk: string) {
      buffer = (buffer + chunk).replace(/\r\n/g, '\n')
      let index = buffer.indexOf('\n\n')
      while (index >= 0) {
        const frame = buffer.slice(0, index)
        buffer = buffer.slice(index + 2)
        let event = ''
        const data: string[] = []
        for (const line of frame.split('\n')) {
          if (line.startsWith(':')) continue
          if (line.startsWith('event:')) event = line.slice(6).trim()
          else if (line.startsWith('data:')) data.push(line.slice(5).replace(/^ /, ''))
        }
        if (data.length > 0) onFrame(event, data.join('\n'))
        index = buffer.indexOf('\n\n')
      }
    },
    get pending() {
      return buffer
    },
  }
}

import { describe, expect, it } from 'vitest'
import { createFrameParser } from './sseParser'

describe('SSE frame parser', () => {
  it('reassembles a frame split across chunks and ignores comments', () => {
    const frames: Array<[string, string]> = []
    const parser = createFrameParser((event, data) => frames.push([event, data]))
    parser.push('event: logs\ndata: [1,')
    parser.push('2]\n\n: ping\n\n')
    expect(frames).toEqual([['logs', '[1,2]']])
  })

  it('handles CRLF and multi-line data across several frames', () => {
    const frames: Array<[string, string]> = []
    const parser = createFrameParser((event, data) => frames.push([event, data]))
    parser.push('event: status\r\ndata: {"a":1}\r\n\r\nevent: x\ndata: l1\ndata: l2\n\n')
    expect(frames).toEqual([
      ['status', '{"a":1}'],
      ['x', 'l1\nl2'],
    ])
  })

  it('keeps an incomplete tail buffered until its terminator arrives', () => {
    const frames: Array<[string, string]> = []
    const parser = createFrameParser((event, data) => frames.push([event, data]))
    parser.push('data: tail')
    expect(frames).toEqual([])
    expect(parser.pending).toBe('data: tail')
    parser.push('\n\n')
    expect(frames).toEqual([['', 'tail']])
  })
})

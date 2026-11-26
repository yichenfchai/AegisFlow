import { describe, expect, it } from 'vitest'
import { renderMarkdown } from './markdown'

/**
 * These tests run in plain Node (no DOM), so they exercise the marked layer:
 * raw HTML escaping, the link protocol allow-list and image reduction. The
 * browser-only DOMPurify pass is covered by the E2E knowledge-base spec.
 */
describe('renderMarkdown', () => {
  it('renders Markdown emphasis and code as real markup', () => {
    expect(renderMarkdown('**粗体**')).toContain('<strong>粗体</strong>')
    expect(renderMarkdown('`const x = 1`')).toContain('<code>const x = 1</code>')
    expect(renderMarkdown('# 标题')).toContain('<h1>标题</h1>')
  })

  it('shows raw HTML payloads as literal text instead of markup', () => {
    const html = renderMarkdown('先看 <script>alert(1)</script> 再说')
    expect(html).not.toContain('<script')
    expect(html).toContain('&lt;script&gt;alert(1)&lt;/script&gt;')
    const image = renderMarkdown('<img src=x onerror="alert(1)">')
    expect(image).not.toContain('<img')
    expect(image).toContain('&lt;img')
  })

  it('allow-lists absolute http(s) links and hardens them', () => {
    const html = renderMarkdown('[文档](https://example.com/docs?q=1)')
    expect(html).toContain('href="https://example.com/docs?q=1"')
    expect(html).toContain('target="_blank"')
    expect(html).toContain('rel="noopener noreferrer"')
  })

  it('renders dangerous or relative link targets as plain text', () => {
    for (const target of ['javascript:alert(1)', 'data:text/html,<b>x</b>', '/relative', 'mailto:a@b.c']) {
      const html = renderMarkdown(`[点我](${target})`)
      expect(html, target).not.toContain('<a ')
      expect(html, target).toContain('点我')
    }
  })

  it('drops image sources and keeps only alt text', () => {
    const html = renderMarkdown('![截图](https://tracker.example.com/pixel.png)')
    expect(html).not.toContain('<img')
    expect(html).not.toContain('tracker.example.com')
    expect(html).toContain('截图')
  })
})

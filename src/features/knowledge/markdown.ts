/**
 * Safe Markdown rendering shared by the knowledge-base pages (P5 batch B).
 *
 * The server stores the body verbatim and never renders it, so rendering is a
 * client responsibility and must not become an XSS path:
 *
 * 1. Raw HTML is disabled at the parser level: every marked `html` token is
 *    re-emitted as escaped text, so `<script>alert(1)</script>` is displayed
 *    literally instead of becoming a script element. Image syntax is reduced to
 *    its alt text so a document cannot beacon out through external images.
 * 2. Links are allow-listed to absolute http(s) URLs and always carry
 *    `target="_blank" rel="noopener noreferrer"`; any other protocol (javascript:,
 *    data:, relative, mailto:) is rendered as plain text.
 * 3. As defense in depth the browser sanitizes the resulting HTML with
 *    DOMPurify against a small tag/attribute allow-list. Node unit tests import
 *    this module without a DOM, where DOMPurify is unsupported and the
 *    already-escaped parser output is returned unchanged.
 */
import { Marked, Renderer } from 'marked'
import DOMPurify from 'dompurify'

const escapeMap: Readonly<Record<string, string>> = {
  '&': '&amp;',
  '<': '&lt;',
  '>': '&gt;',
  '"': '&quot;',
  "'": '&#39;',
}

/** Escape text destined for HTML output (stored bodies may contain anything). */
function escapeHTML(value: string): string {
  return value.replace(/[&<>"']/g, (character) => escapeMap[character] ?? character)
}

/** Link protocol allow-list: absolute http(s) only. */
function isSafeHref(href: string): boolean {
  return /^https?:\/\//i.test(href.trim())
}

const renderer = new Renderer()

// Raw HTML (block and inline) is shown as literal text, never as markup.
renderer.html = ({ text }) => escapeHTML(text)

// Link destinations are validated against the protocol allow-list; a rejected
// link keeps its label as plain text so no anchor with a dangerous href exists.
// The `this: Renderer` annotation is required because marked invokes the merged
// renderer method with the renderer instance as `this` (its parser renders the
// nested inline tokens).
renderer.link = function (this: Renderer, { href, title, tokens }) {
  const inner = this.parser.parseInline(tokens)
  if (!isSafeHref(href)) return inner
  const titleAttribute = title ? ` title="${escapeHTML(title)}"` : ''
  return `<a href="${escapeHTML(href.trim())}"${titleAttribute} target="_blank" rel="noopener noreferrer">${inner}</a>`
}

// Images are reduced to their alt text: no external fetch from a document.
renderer.image = ({ text }) => escapeHTML(text)

const markdown = new Marked({ gfm: true, breaks: true, renderer })

/** Allow-list for the defense-in-depth DOMPurify pass. */
const SANITIZE_TAGS = [
  'p', 'br', 'hr', 'strong', 'em', 'del', 'code', 'pre', 'blockquote',
  'ul', 'ol', 'li', 'h1', 'h2', 'h3', 'h4', 'h5', 'h6',
  'table', 'thead', 'tbody', 'tr', 'th', 'td', 'a',
]
const SANITIZE_ATTR = ['href', 'title', 'target', 'rel']

function sanitize(html: string): string {
  // Without a DOM (Node unit tests) DOMPurify is unsupported; the parser output
  // is already escaped/allow-listed, so return it unchanged.
  if (typeof window === 'undefined' || typeof DOMPurify.sanitize !== 'function') return html
  return DOMPurify.sanitize(html, {
    ALLOWED_TAGS: SANITIZE_TAGS,
    ALLOWED_ATTR: SANITIZE_ATTR,
    ALLOW_DATA_ATTR: false,
    ALLOW_ARIA_ATTR: false,
  })
}

/** Render one stored Markdown body to sanitized HTML. */
export function renderMarkdown(source: string): string {
  return sanitize(markdown.parse(source, { async: false }))
}

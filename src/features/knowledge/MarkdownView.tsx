import { useMemo } from 'react'
import { renderMarkdown } from './markdown'

/**
 * Renders a stored Markdown body through the shared safe renderer (raw HTML
 * disabled, http(s)-only links). The renderer output is escaped/sanitized, so
 * `dangerouslySetInnerHTML` here is the deliberate single injection point of the
 * knowledge pages and must never receive unrendered user input.
 */
export function MarkdownView({ source }: { source: string }) {
  const html = useMemo(() => renderMarkdown(source), [source])
  return (
    <div
      className="text-body-md text-on-surface-variant leading-relaxed break-words
        [&_h1]:text-headline-lg [&_h1]:font-semibold [&_h1]:text-on-surface [&_h1]:mt-space-xl [&_h1]:mb-space-md
        [&_h2]:text-headline-md [&_h2]:font-semibold [&_h2]:text-on-surface [&_h2]:mt-space-lg [&_h2]:mb-space-md
        [&_h3]:text-title-lg [&_h3]:font-semibold [&_h3]:text-on-surface [&_h3]:mt-space-lg [&_h3]:mb-space-sm
        [&_p]:my-space-md [&_strong]:font-bold [&_strong]:text-on-surface
        [&_ul]:list-disc [&_ul]:pl-space-xl [&_ul]:my-space-md [&_ol]:list-decimal [&_ol]:pl-space-xl [&_ol]:my-space-md
        [&_li]:my-space-xs
        [&_a]:text-primary [&_a]:underline
        [&_code]:font-mono [&_code]:text-body-sm [&_code]:bg-surface-container-lowest [&_code]:px-1 [&_code]:py-0.5 [&_code]:rounded
        [&_pre]:bg-surface-container-lowest [&_pre]:border [&_pre]:border-surface-container-highest [&_pre]:rounded-lg [&_pre]:p-space-lg [&_pre]:overflow-x-auto [&_pre]:my-space-md
        [&_pre_code]:bg-transparent [&_pre_code]:p-0
        [&_blockquote]:border-l-2 [&_blockquote]:border-outline-variant [&_blockquote]:pl-space-lg [&_blockquote]:my-space-md
        [&_table]:border-collapse [&_table]:my-space-md [&_th]:border [&_th]:border-surface-container-highest [&_th]:px-space-md [&_th]:py-space-sm [&_td]:border [&_td]:border-surface-container-highest [&_td]:px-space-md [&_td]:py-space-sm
        [&_hr]:border-surface-container-highest [&_hr]:my-space-lg"
      // eslint-disable-next-line react/no-danger -- input is sanitized by renderMarkdown
      dangerouslySetInnerHTML={{ __html: html }}
    />
  )
}

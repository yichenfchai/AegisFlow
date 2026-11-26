import { useId, useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { Button, Field, Input, Textarea } from '@/components/ui/DataTable'
import { useDialogFocus } from '@/components/ui/useDialogFocus'
import { knowledgeAPI, type KnowledgeDocument } from '@/api/knowledge'
import { MarkdownView } from './MarkdownView'
import { knowledgeError, knowledgeValidation } from './knowledgeForm'

/**
 * 知识库文档新建/编辑对话框。复用现有模态语义（role=dialog + aria-modal、
 * aria-labelledby、焦点圈定与 Esc 关闭）；正文是原样存储的纯文本/Markdown，预览
 * 走与详情页相同的安全渲染器（禁用原始 HTML、链接协议白名单）。
 */
export function KnowledgeDocDialog({
  doc,
  onClose,
  onSaved,
}: {
  /** Present for edit, absent for create. */
  doc?: KnowledgeDocument
  onClose: () => void
  onSaved: (doc: KnowledgeDocument) => void
}) {
  const queryClient = useQueryClient()
  const dialogRef = useDialogFocus(true, onClose)
  const titleId = useId()
  const [title, setTitle] = useState(doc?.title ?? '')
  const [tags, setTags] = useState(doc?.tags ?? '')
  const [body, setBody] = useState(doc?.body ?? '')
  const [preview, setPreview] = useState(false)
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)

  const submit = async (event: React.FormEvent) => {
    event.preventDefault()
    if (busy) return
    setError('')
    const invalid = knowledgeValidation({ title, body, tags })
    if (invalid) {
      setError(invalid)
      return
    }
    const input = { title: title.trim(), body, tags: tags.trim() }
    setBusy(true)
    try {
      const saved = doc ? await knowledgeAPI.update(doc.id, input) : await knowledgeAPI.create(input)
      await queryClient.invalidateQueries({ queryKey: ['knowledge'] })
      onSaved(saved)
    } catch (cause) {
      setError(knowledgeError(cause))
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="fixed inset-0 z-modal bg-black/60 flex items-center justify-center" onClick={onClose}>
      <div
        ref={dialogRef}
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        tabIndex={-1}
        className="bg-surface-container rounded-lg border border-surface-container-highest w-[720px] max-h-[88vh] overflow-y-auto p-space-xl focus:outline-none"
        onClick={(event) => event.stopPropagation()}
      >
        <form onSubmit={submit} aria-busy={busy} className="space-y-space-lg">
          <div className="flex items-start justify-between gap-space-lg">
            <h3 id={titleId} className="text-headline-md font-semibold text-on-surface">
              {doc ? '编辑知识库文档' : '新建知识库文档'}
            </h3>
            <Button variant="secondary" size="sm" onClick={() => setPreview((value) => !value)}>
              {preview ? '返回编辑' : '预览'}
            </Button>
          </div>
          <p className="text-body-sm text-on-surface-variant">
            正文按原样存为纯文本 / Markdown；渲染时会禁用原始 HTML，并把链接限制为 http(s)。保存后可在详情页查看。
          </p>
          {preview ? (
            <div className="min-h-[240px] rounded border border-surface-container-highest bg-surface-container-lowest p-space-xl">
              {body.trim() === '' ? (
                <p className="text-body-md text-outline">正文为空，暂无可预览内容。</p>
              ) : (
                <MarkdownView source={body} />
              )}
            </div>
          ) : (
            <>
              <Field label="标题" hint="必填；单行，最多 200 字节" htmlFor="knowledge-title">
                <Input
                  id="knowledge-title"
                  value={title}
                  maxLength={200}
                  onChange={(event) => setTitle(event.target.value)}
                  disabled={busy}
                  placeholder="例如：生产发布与回滚手册"
                />
              </Field>
              <Field
                label="标签"
                hint="选填；英文逗号分隔、单行，最多 200 字节（空白会被清洗）"
                htmlFor="knowledge-tags"
              >
                <Input
                  id="knowledge-tags"
                  value={tags}
                  onChange={(event) => setTags(event.target.value)}
                  disabled={busy}
                  placeholder="ops,runbook"
                />
              </Field>
              <Field
                label="正文"
                hint="选填；最多 100 KiB。支持 Markdown：**粗体**、`代码`、# 标题等"
                htmlFor="knowledge-body"
              >
                <Textarea
                  id="knowledge-body"
                  mono
                  rows={12}
                  value={body}
                  onChange={(event) => setBody(event.target.value)}
                  disabled={busy}
                  placeholder={'# 发布手册\n\n1. 先备份\n2. 再发布'}
                />
              </Field>
            </>
          )}
          {error && (
            <p role="alert" className="text-body-sm text-error bg-error/10 rounded px-space-lg py-space-md">
              {error}
            </p>
          )}
          <div className="flex justify-end gap-space-md pt-space-sm">
            <Button variant="secondary" disabled={busy} onClick={onClose}>
              取消
            </Button>
            <Button type="submit" disabled={busy || title.trim() === ''}>
              {busy ? '保存中...' : doc ? '保存修改' : '创建文档'}
            </Button>
          </div>
        </form>
      </div>
    </div>
  )
}

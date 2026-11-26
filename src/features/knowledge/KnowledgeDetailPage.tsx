import { useState } from 'react'
import { Link, useNavigate, useParams } from 'react-router'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { Button, ConfirmDialog, PageHeader } from '@/components/ui/DataTable'
import { knowledgeAPI, knowledgeTags } from '@/api/knowledge'
import { ApiError } from '@/api/http'
import { errorText } from '@/api/errors'
import { fmtRel } from '@/api/format'
import { useSession } from '@/stores/session'
import { KnowledgeDocDialog } from './KnowledgeDocDialog'
import { MarkdownView } from './MarkdownView'
import { knowledgeError } from './knowledgeForm'

/**
 * 知识库文档详情（真实接口）：正文以纯文本 / Markdown 原样返回，这里用安全渲染器
 * 展示（原始 HTML 按文本显示，链接只允许 http(s) 且带 rel=noopener noreferrer）。
 * 管理员可编辑或删除；删除后回到列表。
 */
export function KnowledgeDetailPage() {
  const { id = '' } = useParams<{ id: string }>()
  const navigate = useNavigate()
  const queryClient = useQueryClient()
  const { user } = useSession()
  const isAdmin = user?.role === 'admin'
  const [editing, setEditing] = useState(false)
  const [deleting, setDeleting] = useState(false)
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)

  const docQuery = useQuery({
    queryKey: ['knowledge', 'detail', id],
    queryFn: () => knowledgeAPI.get(id),
    enabled: id !== '',
    retry: false,
  })
  const doc = docQuery.data

  const remove = async () => {
    if (!doc || busy) return
    setError('')
    setBusy(true)
    try {
      await knowledgeAPI.remove(doc.id)
      await queryClient.invalidateQueries({ queryKey: ['knowledge'] })
      navigate('/knowledge')
    } catch (cause) {
      setError(knowledgeError(cause))
    } finally {
      setBusy(false)
    }
  }

  if (docQuery.error && !doc) {
    const status = docQuery.error instanceof ApiError ? docQuery.error.status : 0
    return (
      <>
        <PageHeader title="知识库文档" />
        <p role="alert" className="text-body-md text-error py-space-lg">
          {status === 404 ? '未找到该知识库文档：它可能已被删除。' : errorText(docQuery.error, '加载知识库文档失败')}
        </p>
        <Link to="/knowledge" className="text-body-md text-primary hover:underline">
          返回知识库
        </Link>
      </>
    )
  }
  if (docQuery.isLoading || !doc) {
    return (
      <>
        <PageHeader title="知识库文档" subtitle="加载中..." />
        <p className="text-body-md text-outline py-space-lg">加载中...</p>
      </>
    )
  }

  const tags = knowledgeTags(doc.tags)
  return (
    <>
      <PageHeader
        title={doc.title}
        subtitle="纯文本 / Markdown 文档 · 安全渲染，原始 HTML 按文本显示"
        actions={
          <div className="flex items-center gap-space-md">
            <Link to="/knowledge" className="text-body-md text-primary hover:underline">
              返回知识库
            </Link>
            {isAdmin && (
              <>
                <Button variant="secondary" onClick={() => setEditing(true)}>
                  编辑
                </Button>
                <Button variant="danger" disabled={busy} onClick={() => { setError(''); setDeleting(true) }}>
                  删除
                </Button>
              </>
            )}
          </div>
        }
      />

      {error && (
        <p role="alert" className="mb-space-md text-body-sm text-error bg-error/10 rounded px-space-lg py-space-md">
          {error}
        </p>
      )}

      <div className="grid grid-cols-[300px_1fr] gap-gutter">
        <aside className="bg-surface-container rounded-lg border border-surface-container-highest p-space-xl space-y-space-lg self-start">
          <MetaRow label="创建人">{doc.createdByName || doc.createdBy.slice(0, 8)}</MetaRow>
          <MetaRow label="更新时间">
            <span title={new Date(doc.updatedAt * 1000).toLocaleString()}>{fmtRel(doc.updatedAt * 1000)}</span>
          </MetaRow>
          <MetaRow label="最近编辑">{doc.updatedByName || '—'}</MetaRow>
          <MetaRow label="创建时间">{new Date(doc.createdAt * 1000).toLocaleString()}</MetaRow>
          <div>
            <div className="mb-space-sm text-label-md text-outline">标签</div>
            {tags.length === 0 ? (
              <p className="text-body-sm text-outline">—</p>
            ) : (
              <div className="flex flex-wrap gap-space-sm">
                {tags.map((tag) => (
                  <span key={tag} className="rounded bg-surface-container-lowest px-space-sm py-0.5 font-mono text-label-sm text-on-surface-variant">
                    {tag}
                  </span>
                ))}
              </div>
            )}
          </div>
        </aside>

        <section className="bg-surface-container rounded-lg border border-surface-container-highest p-space-xl self-start">
          <h2 className="mb-space-lg text-headline-md font-semibold text-on-surface">正文</h2>
          {doc.body.trim() === '' ? (
            <p className="text-body-md text-outline">该文档暂无正文。</p>
          ) : (
            <MarkdownView source={doc.body} />
          )}
        </section>
      </div>

      {editing && (
        <KnowledgeDocDialog
          doc={doc}
          onClose={() => setEditing(false)}
          onSaved={() => setEditing(false)}
        />
      )}
      <ConfirmDialog
        open={deleting}
        title="删除知识库文档"
        danger
        confirmText="确认删除"
        onConfirm={() => void remove()}
        onCancel={() => setDeleting(false)}
      >
        <p>删除后文档正文、标签与历史记录将不可恢复；审计只保留文档 id 与标题。</p>
        {error && (
          <p role="alert" className="mt-space-md text-body-sm text-error">
            {error}
          </p>
        )}
      </ConfirmDialog>
    </>
  )
}

function MetaRow({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="flex items-start justify-between gap-space-lg text-body-sm">
      <span className="shrink-0 text-outline">{label}</span>
      <span className="min-w-0 text-right text-on-surface">{children}</span>
    </div>
  )
}

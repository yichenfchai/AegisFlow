import { useState } from 'react'
import { Link } from 'react-router'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import {
  Button,
  ConfirmDialog,
  DataTable,
  Field,
  Input,
  PageHeader,
  Pagination,
  type Column,
} from '@/components/ui/DataTable'
import { knowledgeAPI, knowledgeTags, type KnowledgeDoc, type KnowledgeDocument } from '@/api/knowledge'
import { errorText } from '@/api/errors'
import { fmtRel } from '@/api/format'
import { useSession } from '@/stores/session'
import { KnowledgeDocDialog } from './KnowledgeDocDialog'
import { knowledgeError } from './knowledgeForm'

const PAGE_SIZE = 12

/**
 * 知识库（真实接口）：所有已登录角色可读，管理员可新建/编辑/删除；列表按更新时间
 * 倒序、支持标题搜索和服务端分页，列表项不含正文。正文以纯文本 / Markdown 原样
 * 存储，服务端不做富文本渲染，详情页用安全渲染器展示。
 */
export function KnowledgeListPage() {
  const queryClient = useQueryClient()
  const { user } = useSession()
  const isAdmin = user?.role === 'admin'
  const [page, setPage] = useState(1)
  const [draftQuery, setDraftQuery] = useState('')
  const [query, setQuery] = useState('')
  const [creating, setCreating] = useState(false)
  const [editing, setEditing] = useState<KnowledgeDocument | null>(null)
  const [deleting, setDeleting] = useState<KnowledgeDoc | null>(null)
  const [notice, setNotice] = useState('')
  const [actionError, setActionError] = useState('')
  const [busy, setBusy] = useState(false)

  const { data, isPending, error } = useQuery({
    queryKey: ['knowledge', query, page],
    queryFn: () => knowledgeAPI.list({ q: query || undefined, limit: PAGE_SIZE, offset: (page - 1) * PAGE_SIZE }),
  })
  const docs = data?.items ?? []

  const refresh = () => queryClient.invalidateQueries({ queryKey: ['knowledge'] })

  const search = (event: React.FormEvent) => {
    event.preventDefault()
    setActionError('')
    setQuery(draftQuery.trim())
    setPage(1)
  }

  const openEdit = async (doc: KnowledgeDoc) => {
    setActionError('')
    setBusy(true)
    try {
      setEditing(await knowledgeAPI.get(doc.id))
    } catch (cause) {
      setActionError(knowledgeError(cause))
    } finally {
      setBusy(false)
    }
  }

  const remove = async () => {
    const target = deleting
    if (!target || busy) return
    setActionError('')
    setBusy(true)
    try {
      await knowledgeAPI.remove(target.id)
      setDeleting(null)
      setNotice(`已删除文档「${target.title}」`)
      await refresh()
    } catch (cause) {
      setActionError(knowledgeError(cause))
    } finally {
      setBusy(false)
    }
  }

  const columns: Column<KnowledgeDoc>[] = [
    {
      key: 'title',
      header: '标题',
      render: (doc) => (
        <Link to={`/knowledge/${doc.id}`} className="font-medium text-primary hover:underline">
          {doc.title}
        </Link>
      ),
    },
    {
      key: 'tags',
      header: '标签',
      render: (doc) => {
        const tags = knowledgeTags(doc.tags)
        return tags.length === 0 ? (
          <span className="text-outline">—</span>
        ) : (
          <div className="flex flex-wrap gap-space-xs">
            {tags.map((tag) => (
              <span key={tag} className="rounded bg-surface-container-lowest px-space-sm py-0.5 font-mono text-label-sm text-on-surface-variant">
                {tag}
              </span>
            ))}
          </div>
        )
      },
    },
    {
      key: 'createdByName',
      header: '创建人',
      render: (doc) => <span title={doc.createdBy}>{doc.createdByName || doc.createdBy.slice(0, 8)}</span>,
    },
    {
      key: 'updatedAt',
      header: '更新时间',
      render: (doc) => (
        <span title={new Date(doc.updatedAt * 1000).toLocaleString()}>
          {fmtRel(doc.updatedAt * 1000)}
          {doc.updatedByName && doc.updatedByName !== doc.createdByName ? ` · ${doc.updatedByName}` : ''}
        </span>
      ),
    },
  ]

  if (isAdmin) {
    columns.push({
      key: 'actions',
      header: '操作',
      align: 'right',
      render: (doc) => (
        <div className="flex justify-end gap-space-md">
          <Button variant="secondary" size="sm" disabled={busy} onClick={() => void openEdit(doc)}>
            编辑
          </Button>
          <Button variant="danger" size="sm" disabled={busy} onClick={() => { setActionError(''); setDeleting(doc) }}>
            删除
          </Button>
        </div>
      ),
    })
  }

  return (
    <>
      <PageHeader
        title="知识库"
        subtitle="团队文档以纯文本 / Markdown 存储；所有成员可读，管理员可新建、编辑与删除"
        actions={
          isAdmin ? (
            <Button icon="note_add" onClick={() => { setActionError(''); setCreating(true) }}>
              新建文档
            </Button>
          ) : undefined
        }
      />

      <form onSubmit={search} className="mb-space-lg flex flex-wrap items-end gap-space-md">
        <Field label="搜索标题" htmlFor="knowledge-search" className="w-[320px]">
          <Input
            id="knowledge-search"
            value={draftQuery}
            maxLength={200}
            placeholder="输入标题关键字，留空显示全部"
            onChange={(event) => setDraftQuery(event.target.value)}
          />
        </Field>
        <Button type="submit" variant="secondary" icon="search">
          搜索
        </Button>
        {(query !== '' || draftQuery !== '') && (
          <Button
            variant="ghost"
            onClick={() => {
              setDraftQuery('')
              setQuery('')
              setPage(1)
            }}
          >
            清空
          </Button>
        )}
      </form>

      {notice && (
        <p role="status" className="mb-space-md text-body-sm text-success bg-success/10 rounded px-space-lg py-space-md">
          {notice}
        </p>
      )}
      {actionError && (
        <p role="alert" className="mb-space-md text-body-sm text-error bg-error/10 rounded px-space-lg py-space-md">
          {actionError}
        </p>
      )}

      {error ? (
        <p role="alert" className="text-body-md text-error py-space-lg">
          {errorText(error, '加载知识库失败')}
        </p>
      ) : isPending ? (
        <p className="text-body-md text-outline py-space-lg">加载中...</p>
      ) : (
        <>
          <DataTable
            columns={columns}
            rows={docs}
            empty={query ? '没有标题匹配的知识库文档' : '知识库还没有文档；管理员可点击「新建文档」创建第一篇'}
          />
          <Pagination page={page} pageSize={PAGE_SIZE} total={data?.total ?? 0} onPage={setPage} />
        </>
      )}

      {creating && (
        <KnowledgeDocDialog
          onClose={() => setCreating(false)}
          onSaved={(saved) => {
            setCreating(false)
            setNotice(`已创建文档「${saved.title}」`)
          }}
        />
      )}
      {editing && (
        <KnowledgeDocDialog
          doc={editing}
          onClose={() => setEditing(null)}
          onSaved={(saved) => {
            setEditing(null)
            setNotice(`已保存文档「${saved.title}」`)
          }}
        />
      )}
      <ConfirmDialog
        open={deleting !== null}
        title="删除知识库文档"
        danger
        confirmText="确认删除"
        onConfirm={() => void remove()}
        onCancel={() => setDeleting(null)}
      >
        <p className="mb-space-md">删除后文档正文、标签与历史记录将不可恢复；审计只保留文档 id 与标题。</p>
        {deleting && <p className="text-body-sm text-on-surface">「{deleting.title}」</p>}
        {actionError && (
          <p role="alert" className="mt-space-md text-body-sm text-error">
            {actionError}
          </p>
        )}
      </ConfirmDialog>
    </>
  )
}

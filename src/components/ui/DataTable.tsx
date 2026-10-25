import { Icon } from './Icon'

export interface Column<T> {
  key: string
  header: React.ReactNode
  render: (row: T) => React.ReactNode
  width?: string
  align?: 'left' | 'right' | 'center'
}

export function DataTable<T extends { id: string }>({
  columns,
  rows,
  empty = '暂无数据',
  onRowClick,
}: {
  columns: Column<T>[]
  rows: T[]
  empty?: string
  onRowClick?: (row: T) => void
}) {
  return (
    <div className="bg-surface-container rounded-lg border border-surface-container-highest overflow-hidden">
      <table className="w-full text-left">
        <thead>
          <tr className="border-b border-surface-container-highest">
            {columns.map((col) => (
              <th
                key={col.key}
                style={{ width: col.width }}
                className={`px-space-xl py-space-md text-label-md text-outline font-normal ${
                  col.align === 'right' ? 'text-right' : col.align === 'center' ? 'text-center' : ''
                }`}
              >
                {col.header}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {rows.length === 0 && (
            <tr>
              <td colSpan={columns.length} className="px-space-xl py-10 text-center text-body-md text-outline">
                {empty}
              </td>
            </tr>
          )}
          {rows.map((row) => (
            <tr
              key={row.id}
              onClick={onRowClick ? () => onRowClick(row) : undefined}
              className={`border-b border-surface-container-highest/50 last:border-0 hover:bg-surface-container-low transition-colors ${
                onRowClick ? 'cursor-pointer' : ''
              }`}
            >
              {columns.map((col) => (
                <td
                  key={col.key}
                  className={`px-space-xl py-space-lg text-body-md text-on-surface ${
                    col.align === 'right' ? 'text-right' : col.align === 'center' ? 'text-center' : ''
                  }`}
                >
                  {col.render(row)}
                </td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

/** 表底分页 */
export function Pagination({
  page,
  pageSize,
  total,
  onPage,
}: {
  page: number
  pageSize: number
  total: number
  onPage: (p: number) => void
}) {
  const totalPages = Math.max(1, Math.ceil(total / pageSize))
  return (
    <div className="flex items-center justify-between mt-space-lg">
      <span className="text-label-md text-outline">
        显示 {(page - 1) * pageSize + 1}-{Math.min(page * pageSize, total)} · 共 {total} 条
      </span>
      <div className="flex items-center gap-space-sm">
        <button
          disabled={page <= 1}
          onClick={() => onPage(page - 1)}
          className="px-space-lg py-1 rounded border border-surface-container-highest text-body-sm text-on-surface-variant hover:text-on-surface disabled:opacity-40 disabled:cursor-not-allowed"
        >
          上一页
        </button>
        <span className="font-mono text-label-md text-outline px-space-sm">
          {page} / {totalPages}
        </span>
        <button
          disabled={page >= totalPages}
          onClick={() => onPage(page + 1)}
          className="px-space-lg py-1 rounded border border-surface-container-highest text-body-sm text-on-surface-variant hover:text-on-surface disabled:opacity-40 disabled:cursor-not-allowed"
        >
          下一页
        </button>
      </div>
    </div>
  )
}

/** 页头：标题 + 副标题 + 右侧动作 */
export function PageHeader({
  title,
  subtitle,
  actions,
}: {
  title: string
  subtitle?: string
  actions?: React.ReactNode
}) {
  return (
    <div className="flex items-start justify-between mb-space-xl">
      <div>
        <h1 className="text-headline-lg font-semibold text-on-surface">{title}</h1>
        {subtitle && <p className="text-body-md text-outline mt-space-xs">{subtitle}</p>}
      </div>
      {actions && <div className="flex items-center gap-space-md">{actions}</div>}
    </div>
  )
}

/** 按钮组 */
export function Button({
  variant = 'primary',
  size = 'md',
  icon,
  children,
  onClick,
  disabled,
  className = '',
  type = 'button',
}: {
  variant?: 'primary' | 'secondary' | 'danger' | 'ghost'
  size?: 'sm' | 'md'
  icon?: string
  children?: React.ReactNode
  onClick?: () => void
  disabled?: boolean
  className?: string
  type?: 'button' | 'submit'
}) {
  const variants = {
    primary: 'bg-primary text-on-primary hover:brightness-110 font-medium',
    secondary:
      'bg-transparent border border-outline-variant text-on-surface-variant hover:text-on-surface hover:border-outline',
    danger: 'bg-transparent border border-error/60 text-error hover:bg-error/10',
    ghost: 'text-on-surface-variant hover:text-on-surface hover:bg-surface-container',
  }
  return (
    <button
      type={type}
      onClick={onClick}
      disabled={disabled}
      className={`inline-flex items-center gap-space-sm rounded transition-all disabled:opacity-40 disabled:cursor-not-allowed ${
        size === 'sm' ? 'px-space-lg py-1 text-body-sm' : 'px-space-xl py-1.5 text-body-md'
      } ${variants[variant]} ${className}`}
    >
      {icon && <Icon name={icon} size={size === 'sm' ? 14 : 16} />}
      {children}
    </button>
  )
}

/** 表单字段壳：label + 说明 + 控件（htmlFor 需与控件 id 配对） */
export function Field({
  label,
  hint,
  children,
  className = '',
  htmlFor,
}: {
  label: string
  hint?: string
  children: React.ReactNode
  className?: string
  htmlFor?: string
}) {
  return (
    <div className={className}>
      <label htmlFor={htmlFor} className="block text-label-md text-on-surface-variant mb-space-sm">
        {label}
      </label>
      {children}
      {hint && <p className="text-label-sm text-outline mt-space-xs">{hint}</p>}
    </div>
  )
}

/** 输入框（含 mono 变体） */
export function Input({
  mono,
  className = '',
  ...props
}: React.InputHTMLAttributes<HTMLInputElement> & { mono?: boolean }) {
  return (
    <input
      {...props}
      className={`w-full bg-surface-container-lowest border border-surface-container-highest rounded px-space-lg py-space-md text-body-md text-on-surface placeholder:text-text-3 focus:outline-none focus:border-primary/60 transition-colors ${
        mono ? 'font-mono text-body-sm' : ''
      } ${className}`}
    />
  )
}

/** 多行文本 */
export function Textarea({
  mono,
  className = '',
  ...props
}: React.TextareaHTMLAttributes<HTMLTextAreaElement> & { mono?: boolean }) {
  return (
    <textarea
      {...props}
      className={`w-full bg-surface-container-lowest border border-surface-container-highest rounded px-space-lg py-space-md text-body-md text-on-surface placeholder:text-text-3 focus:outline-none focus:border-primary/60 transition-colors resize-y ${
        mono ? 'font-mono text-body-sm leading-relaxed' : ''
      } ${className}`}
    />
  )
}

/** 下拉框 */
export function Select({
  className = '',
  children,
  ...props
}: React.SelectHTMLAttributes<HTMLSelectElement>) {
  return (
    <select
      {...props}
      className={`bg-surface-container-lowest border border-surface-container-highest rounded px-space-lg py-space-md text-body-md text-on-surface focus:outline-none focus:border-primary/60 cursor-pointer ${className}`}
    >
      {children}
    </select>
  )
}

/** 数字步进器 */
export function Stepper({
  value,
  onChange,
  min = 1,
  max = 99,
}: {
  value: number
  onChange: (v: number) => void
  min?: number
  max?: number
}) {
  return (
    <div className="inline-flex items-center border border-surface-container-highest rounded overflow-hidden">
      <button
        onClick={() => onChange(Math.max(min, value - 1))}
        className="px-space-lg py-space-sm text-on-surface-variant hover:bg-surface-container"
      >
        <Icon name="remove" size={14} />
      </button>
      <span className="font-mono text-body-md px-space-lg min-w-10 text-center">{value}</span>
      <button
        onClick={() => onChange(Math.min(max, value + 1))}
        className="px-space-lg py-space-sm text-on-surface-variant hover:bg-surface-container"
      >
        <Icon name="add" size={14} />
      </button>
    </div>
  )
}

/** 空状态 */
export function EmptyState({ icon = 'inbox', text }: { icon?: string; text: string }) {
  return (
    <div className="flex flex-col items-center py-16 gap-space-md">
      <Icon name={icon} size={32} className="text-outline" />
      <span className="text-body-md text-outline">{text}</span>
    </div>
  )
}

/** 确认弹窗 */
export function ConfirmDialog({
  open,
  title,
  children,
  confirmText = '确认',
  danger,
  onConfirm,
  onCancel,
}: {
  open: boolean
  title: string
  children: React.ReactNode
  confirmText?: string
  danger?: boolean
  onConfirm: () => void
  onCancel: () => void
}) {
  if (!open) return null
  return (
    <div className="fixed inset-0 z-modal bg-black/60 flex items-center justify-center" onClick={onCancel}>
      <div
        className="bg-surface-container rounded-lg border border-surface-container-highest w-[420px] p-space-xl"
        onClick={(e) => e.stopPropagation()}
      >
        <h3 className={`text-headline-md font-semibold mb-space-md ${danger ? 'text-error' : 'text-on-surface'}`}>
          {title}
        </h3>
        <div className="text-body-md text-on-surface-variant mb-space-xl">{children}</div>
        <div className="flex justify-end gap-space-md">
          <Button variant="secondary" onClick={onCancel}>
            取消
          </Button>
          <Button variant={danger ? 'danger' : 'primary'} onClick={onConfirm}>
            {confirmText}
          </Button>
        </div>
      </div>
    </div>
  )
}

import { useMemo, useRef, useState } from 'react'
import type { DragEvent } from 'react'
import { Button, Input, Modal, Popconfirm, Space, Table, Tooltip, Typography, message } from 'antd'
import type { TableColumnsType } from 'antd'
import {
  ArrowUpOutlined,
  CopyOutlined,
  DeleteOutlined,
  DownloadOutlined,
  EditOutlined,
  FileOutlined,
  FolderAddOutlined,
  FolderOutlined,
  ReloadOutlined,
  ScissorOutlined,
  SnippetsOutlined,
  UploadOutlined
} from '@ant-design/icons'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import { translateError } from '../../api/errors'
import { useApi } from '../../api/provider'
import type { ParentFileEntryDTO } from '../../api/types'
import { CodeEditor, type CodeEditorLanguage } from '../../components/CodeEditor'
import { ErrorNotice } from '../../components/ErrorNotice'
import { useI18n } from '../../i18n'
import { fontSize, spacing } from '../../tokens/palette'
import { formatBytes, formatTime } from '../../utils/format'

/**
 * 网页编辑器单文件上限：与服务端 `parentTextLimit` 保持一致（20 MiB）。
 * 超过它的文件不提供「编辑」，避免把浏览器/编辑器撑爆。
 */
const PARENT_EDIT_MAX_BYTES = 20 * 1024 * 1024

/** 列表行：真实的母盘条目，或首行的「上一级目录」快捷行。 */
type FileRow = ParentFileEntryDTO & { parentLink?: boolean }

/**
 * 维护态下的母盘文件管理。
 *
 * 母盘由**服务端本地挂载**后经文件 API 读写，不再通过 iSCSI 挂到客户端 —— 链路短，
 * 也不受客户端 iSCSI 环境影响（这正是之前"挂载母盘"反复卡住的根源）。
 *
 * 交互：
 *   - 目录：单击进入；首行固定「上一级目录」，也可把条目拖到它上面移回上级；
 *   - 文件：单击**不**打开编辑（大文件会拖垮页面），只能点「编辑」按钮（>20 MiB 禁用）；
 *   - 行可拖拽：拖到目录行/面包屑上即移动进去；
 *   - 剪切 / 复制 / 粘贴：剪切=移动，复制=服务端递归拷贝（同名自动生成 (copy)）。
 */
export function RepoParentFiles({ repoId }: { repoId: string }): JSX.Element {
  const api = useApi()
  const { t } = useI18n()
  const queryClient = useQueryClient()
  const fileInput = useRef<HTMLInputElement>(null)

  const [cwd, setCwd] = useState('/')
  const [editor, setEditor] = useState<{ path: string; content: string } | null>(null)
  const [rename, setRename] = useState<{ path: string; name: string } | null>(null)
  const [mkdirOpen, setMkdirOpen] = useState(false)
  const [mkdirName, setMkdirName] = useState('')
  const [uploading, setUploading] = useState(false)
  // 拖拽移动：dragPath 是被拖的行，dragOverPath 是高亮的投放目标（目录/面包屑）。
  const [dragPath, setDragPath] = useState<string | null>(null)
  const [dragOverPath, setDragOverPath] = useState<string | null>(null)
  // 剪切/复制剪贴板：只记录一条（资源管理器式）。
  const [clipboard, setClipboard] = useState<{ mode: 'cut' | 'copy'; path: string; name: string } | null>(null)

  const queryKey = useMemo(() => ['parent-files', repoId, cwd], [repoId, cwd])
  const listingQuery = useQuery({
    queryKey,
    queryFn: () => api.listParentFiles(repoId, cwd)
  })
  const refresh = (): void => {
    void queryClient.invalidateQueries({ queryKey: ['parent-files', repoId] })
  }

  const deleteMutation = useMutation({
    mutationFn: (path: string) => api.deleteParentEntry(repoId, path),
    onSuccess: () => {
      message.success(t('repo.parentFiles.deleted'))
      refresh()
    },
    onError: (err) => message.error(translateError(err) || t('repo.parentFiles.actionFailed'))
  })

  const mkdirMutation = useMutation({
    mutationFn: (path: string) => api.mkdirParent(repoId, path),
    onSuccess: () => {
      setMkdirOpen(false)
      setMkdirName('')
      refresh()
    },
    onError: (err) => message.error(translateError(err) || t('repo.parentFiles.actionFailed'))
  })

  // 重命名与拖拽移动共用：都是「把 from 移到 to」。
  const moveMutation = useMutation({
    mutationFn: (input: { from: string; to: string }) => api.moveParentEntry(repoId, input.from, input.to),
    onSuccess: () => {
      setRename(null)
      setDragPath(null)
      setDragOverPath(null)
      refresh()
    },
    onError: (err) => message.error(translateError(err) || t('repo.parentFiles.actionFailed'))
  })

  // 粘贴：剪切走 move（成功后清空剪贴板），复制走 copy（保留剪贴板，可连续粘贴）。
  const pasteMutation = useMutation({
    mutationFn: (input: { mode: 'cut' | 'copy'; from: string; to: string }) =>
      input.mode === 'cut'
        ? api.moveParentEntry(repoId, input.from, input.to)
        : api.copyParentEntry(repoId, input.from, input.to),
    onSuccess: (_res, input) => {
      message.success(t('repo.parentFiles.pasted'))
      if (input.mode === 'cut') setClipboard(null)
      refresh()
    },
    onError: (err) => message.error(translateError(err) || t('repo.parentFiles.actionFailed'))
  })

  const saveTextMutation = useMutation({
    mutationFn: (input: { path: string; content: string }) => api.writeParentText(repoId, input.path, input.content),
    onSuccess: () => {
      message.success(t('repo.parentFiles.saved'))
      refresh()
    },
    onError: (err) => message.error(translateError(err) || t('repo.parentFiles.actionFailed'))
  })

  const uploadFiles = async (files: FileList | null): Promise<void> => {
    if (!files || files.length === 0) return
    setUploading(true)
    try {
      for (const file of Array.from(files)) {
        await api.uploadParentFile(repoId, joinParentPath(cwd, file.name), file)
      }
      message.success(t('repo.parentFiles.uploaded'))
      refresh()
    } catch (err) {
      message.error(translateError(err) || t('repo.parentFiles.actionFailed'))
    } finally {
      setUploading(false)
      if (fileInput.current) fileInput.current.value = ''
    }
  }

  const download = async (entry: ParentFileEntryDTO): Promise<void> => {
    try {
      const blob = await api.downloadParentFile(repoId, entry.path)
      const url = URL.createObjectURL(blob)
      const a = document.createElement('a')
      a.href = url
      a.download = entry.name
      a.click()
      URL.revokeObjectURL(url)
    } catch (err) {
      message.error(translateError(err) || t('repo.parentFiles.actionFailed'))
    }
  }

  const openEditor = async (entry: ParentFileEntryDTO): Promise<void> => {
    try {
      const res = await api.readParentText(repoId, entry.path)
      setEditor({ path: entry.path, content: res.content })
    } catch (err) {
      message.error(translateError(err) || t('repo.parentFiles.actionFailed'))
    }
  }

  const pasteHere = (): void => {
    if (!clipboard) return
    const to = joinParentPath(cwd, clipboard.name)
    if (clipboard.mode === 'cut' && to === clipboard.path) return
    pasteMutation.mutate({ mode: clipboard.mode, from: clipboard.path, to })
  }

  /** 拖拽目标是否可用：不能拖进自己/自己的子目录，也不能原地移动。 */
  const canDropInto = (targetDir: string): boolean => {
    if (!dragPath) return false
    if (targetDir === dragPath || targetDir.startsWith(`${dragPath}/`)) return false
    return joinParentPath(targetDir, baseName(dragPath)) !== dragPath
  }

  const dropInto = (targetDir: string): void => {
    if (!dragPath || !canDropInto(targetDir)) return
    moveMutation.mutate({ from: dragPath, to: joinParentPath(targetDir, baseName(dragPath)) })
  }

  const columns: TableColumnsType<FileRow> = [
    {
      title: t('repo.parentFiles.name'),
      dataIndex: 'name',
      key: 'name',
      render: (_, entry) => (
        <Space
          size={spacing.xs}
          style={{ cursor: entry.is_dir ? 'pointer' : 'default' }}
          onClick={() => {
            // 只有目录单击进入；文件单击不触发编辑（大文件会拖垮页面，编辑必须点按钮）。
            if (entry.is_dir) setCwd(entry.path)
          }}
        >
          {entry.parentLink ? <ArrowUpOutlined /> : entry.is_dir ? <FolderOutlined /> : <FileOutlined />}
          <Typography.Text>{entry.parentLink ? t('repo.parentFiles.parentDir') : entry.name}</Typography.Text>
        </Space>
      )
    },
    {
      title: t('repo.parentFiles.size'),
      dataIndex: 'size',
      key: 'size',
      width: 120,
      render: (_, entry) => (entry.parentLink || entry.is_dir ? '-' : formatBytes(entry.size))
    },
    {
      title: t('repo.parentFiles.modifiedAt'),
      dataIndex: 'modified_at',
      key: 'modified_at',
      width: 180,
      render: (value: number, entry) => (entry.parentLink ? '-' : formatTime(value))
    },
    {
      title: t('common.actions'),
      key: 'actions',
      width: 280,
      render: (_, entry) => {
        if (entry.parentLink) return null
        const tooLarge = !entry.is_dir && entry.size > PARENT_EDIT_MAX_BYTES
        return (
          <Space size={spacing.xs}>
            {entry.is_dir ? null : (
              <>
                <Tooltip title={tooLarge ? t('repo.parentFiles.tooLargeToEdit') : t('repo.parentFiles.edit')}>
                  <Button
                    type="text"
                    size="small"
                    icon={<EditOutlined />}
                    disabled={tooLarge}
                    onClick={() => void openEditor(entry)}
                  />
                </Tooltip>
                <Tooltip title={t('repo.parentFiles.download')}>
                  <Button type="text" size="small" icon={<DownloadOutlined />} onClick={() => void download(entry)} />
                </Tooltip>
              </>
            )}
            <Tooltip title={t('repo.parentFiles.cut')}>
              <Button
                type="text"
                size="small"
                icon={<ScissorOutlined />}
                onClick={() => {
                  setClipboard({ mode: 'cut', path: entry.path, name: entry.name })
                  message.info(t('repo.parentFiles.cutReady', { name: entry.name }))
                }}
              />
            </Tooltip>
            <Tooltip title={t('repo.parentFiles.copy')}>
              <Button
                type="text"
                size="small"
                icon={<CopyOutlined />}
                onClick={() => {
                  setClipboard({ mode: 'copy', path: entry.path, name: entry.name })
                  message.info(t('repo.parentFiles.copied', { name: entry.name }))
                }}
              />
            </Tooltip>
            <Tooltip title={t('repo.parentFiles.rename')}>
              <Button
                type="text"
                size="small"
                icon={<FolderAddOutlined rotate={90} />}
                onClick={() => setRename({ path: entry.path, name: entry.name })}
              />
            </Tooltip>
            <Popconfirm
              title={t('repo.parentFiles.deleteConfirm')}
              okText={t('common.confirm')}
              cancelText={t('common.cancel')}
              onConfirm={() => deleteMutation.mutate(entry.path)}
            >
              <Button type="text" size="small" danger icon={<DeleteOutlined />} />
            </Popconfirm>
          </Space>
        )
      }
    }
  ]

  const crumbs = breadcrumbOf(cwd)
  // 首行固定「上一级目录」：既提供点击返回，也是**拖拽回上级**的投放目标。
  const rows: FileRow[] = useMemo(() => {
    const items = listingQuery.data?.items ?? []
    if (cwd === '/') return items
    return [
      { name: '..', path: parentOf(cwd), is_dir: true, size: 0, modified_at: 0, parentLink: true },
      ...items
    ]
  }, [listingQuery.data, cwd])

  return (
    <Space direction="vertical" size={spacing.md} style={{ width: '100%' }}>
      <Space wrap>
        <Button size="small" icon={<UploadOutlined />} loading={uploading} onClick={() => fileInput.current?.click()}>
          {t('repo.parentFiles.upload')}
        </Button>
        <input
          ref={fileInput}
          type="file"
          multiple
          style={{ display: 'none' }}
          onChange={(event) => void uploadFiles(event.target.files)}
        />
        <Button size="small" icon={<FolderAddOutlined />} onClick={() => setMkdirOpen(true)}>
          {t('repo.parentFiles.newFolder')}
        </Button>
        <Tooltip title={clipboard ? t('repo.parentFiles.clipboard', { name: clipboard.name }) : undefined}>
          <Button
            size="small"
            icon={<SnippetsOutlined />}
            disabled={!clipboard || pasteMutation.isPending}
            loading={pasteMutation.isPending}
            onClick={pasteHere}
          >
            {t('repo.parentFiles.paste')}
          </Button>
        </Tooltip>
        <Button size="small" icon={<ReloadOutlined />} onClick={refresh}>
          {t('common.refresh')}
        </Button>
        <Typography.Text type="secondary" style={{ fontSize: fontSize.xs }}>
          {t('repo.parentFiles.dragHint')}
        </Typography.Text>
      </Space>

      <Space size={4} wrap>
        {crumbs.map((crumb) => (
          <Typography.Link
            key={crumb.path}
            onClick={() => setCwd(crumb.path)}
            onDragOver={(event) => {
              if (!canDropInto(crumb.path)) return
              event.preventDefault()
              event.dataTransfer.dropEffect = 'move'
              setDragOverPath(crumb.path)
            }}
            onDragLeave={() => setDragOverPath((prev) => (prev === crumb.path ? null : prev))}
            onDrop={(event) => {
              event.preventDefault()
              dropInto(crumb.path)
            }}
            style={{
              padding: '0 4px',
              borderRadius: 4,
              background: dragOverPath === crumb.path ? 'rgba(22, 119, 255, 0.14)' : undefined
            }}
          >
            {crumb.name}
            {crumb.path === '/' ? '' : ' /'}
          </Typography.Link>
        ))}
      </Space>

      {listingQuery.error ? <ErrorNotice error={listingQuery.error} /> : null}

      <Table<FileRow>
        size="small"
        rowKey={(entry) => entry.path}
        columns={columns}
        dataSource={rows}
        loading={listingQuery.isLoading}
        pagination={false}
        // 列表占满窗口剩余高度：扣掉顶部导航、维护横幅、Tabs、工具条与面包屑；
        // 小窗口下保底 300px，避免被压成一条缝。
        scroll={{ y: 'max(300px, calc(100vh - 380px))' }}
        onRow={(entry) => ({
          draggable: !entry.parentLink,
          style: dragOverPath === entry.path ? { background: 'rgba(22, 119, 255, 0.12)' } : undefined,
          onDragStart: (event) => {
            if (entry.parentLink) return
            setDragPath(entry.path)
            event.dataTransfer.effectAllowed = 'move'
            event.dataTransfer.setData('text/plain', entry.path)
          },
          onDragEnd: () => {
            setDragPath(null)
            setDragOverPath(null)
          },
          ...(entry.is_dir
            ? {
                onDragOver: (event: DragEvent<HTMLElement>) => {
                  if (!canDropInto(entry.path)) return
                  event.preventDefault()
                  event.dataTransfer.dropEffect = 'move'
                  setDragOverPath(entry.path)
                },
                onDragLeave: () => setDragOverPath((prev) => (prev === entry.path ? null : prev)),
                onDrop: (event: DragEvent<HTMLElement>) => {
                  event.preventDefault()
                  dropInto(entry.path)
                }
              }
            : {})
        })}
      />

      {/* 内嵌代码编辑器（CodeMirror）：只在点「编辑」按钮后打开。 */}
      <Modal
        open={editor !== null}
        title={editor?.path}
        width={1200}
        centered
        destroyOnClose
        okText={t('common.save')}
        cancelText={t('common.cancel')}
        confirmLoading={saveTextMutation.isPending}
        onCancel={() => setEditor(null)}
        onOk={() => {
          if (!editor) return
          saveTextMutation.mutate({ path: editor.path, content: editor.content })
        }}
        styles={{ body: { padding: 0 } }}
      >
        <CodeEditor
          value={editor?.content ?? ''}
          language={editorLanguage(editor?.path ?? '')}
          onChange={(content) => setEditor((prev) => (prev ? { ...prev, content } : prev))}
          onSave={() => {
            if (editor) saveTextMutation.mutate({ path: editor.path, content: editor.content })
          }}
        />
      </Modal>

      <Modal
        open={mkdirOpen}
        title={t('repo.parentFiles.newFolder')}
        okText={t('common.confirm')}
        cancelText={t('common.cancel')}
        confirmLoading={mkdirMutation.isPending}
        onCancel={() => setMkdirOpen(false)}
        onOk={() => {
          if (!mkdirName.trim()) return
          mkdirMutation.mutate(joinParentPath(cwd, mkdirName.trim()))
        }}
      >
        <Input
          value={mkdirName}
          placeholder={t('repo.parentFiles.folderName')}
          onChange={(event) => setMkdirName(event.target.value)}
          onPressEnter={() => {
            if (!mkdirName.trim()) return
            mkdirMutation.mutate(joinParentPath(cwd, mkdirName.trim()))
          }}
        />
      </Modal>

      <Modal
        open={rename !== null}
        title={t('repo.parentFiles.rename')}
        okText={t('common.confirm')}
        cancelText={t('common.cancel')}
        confirmLoading={moveMutation.isPending}
        onCancel={() => setRename(null)}
        onOk={() => {
          if (!rename || !rename.name.trim()) return
          moveMutation.mutate({
            from: rename.path,
            to: joinParentPath(parentOf(rename.path), rename.name.trim())
          })
        }}
      >
        <Input
          value={rename?.name ?? ''}
          onChange={(event) => setRename((prev) => (prev ? { ...prev, name: event.target.value } : prev))}
          onPressEnter={() => {
            if (!rename || !rename.name.trim()) return
            moveMutation.mutate({
              from: rename.path,
              to: joinParentPath(parentOf(rename.path), rename.name.trim())
            })
          }}
        />
      </Modal>
    </Space>
  )
}

/** 按扩展名选择语法高亮；其它文件按纯文本。 */
function editorLanguage(path: string): CodeEditorLanguage {
  const lower = path.toLowerCase()
  if (lower.endsWith('.json')) return 'json'
  if (/\.(js|mjs|cjs|ts|tsx|jsx)$/.test(lower)) return 'javascript'
  return 'text'
}

/** 拼接母盘内路径（统一 "/"，根为 "/"）。 */
function joinParentPath(dir: string, name: string): string {
  const base = dir === '/' ? '' : dir.replace(/\/+$/, '')
  return `${base}/${name.replace(/^\/+/, '')}`
}

/** 取父目录（根的子项父目录为 "/"）。 */
function parentOf(path: string): string {
  const idx = path.lastIndexOf('/')
  return idx <= 0 ? '/' : path.slice(0, idx)
}

/** 取路径末段（文件名/目录名）。 */
function baseName(path: string): string {
  const idx = path.lastIndexOf('/')
  return idx < 0 ? path : path.slice(idx + 1)
}

/** 面包屑：根叫「/」，逐级展开。 */
function breadcrumbOf(cwd: string): { name: string; path: string }[] {
  const out = [{ name: '/', path: '/' }]
  let acc = ''
  for (const seg of cwd.split('/').filter(Boolean)) {
    acc += `/${seg}`
    out.push({ name: seg, path: acc })
  }
  return out
}

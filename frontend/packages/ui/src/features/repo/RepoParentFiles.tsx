import { useMemo, useRef, useState } from 'react'
import {
  Button,
  Input,
  Modal,
  Popconfirm,
  Space,
  Table,
  Tooltip,
  Typography,
  message
} from 'antd'
import type { TableColumnsType } from 'antd'
import {
  DeleteOutlined,
  DownloadOutlined,
  EditOutlined,
  FileOutlined,
  FolderAddOutlined,
  FolderOutlined,
  ReloadOutlined,
  UploadOutlined
} from '@ant-design/icons'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import { translateError } from '../../api/errors'
import { useApi } from '../../api/provider'
import type { ParentFileEntryDTO } from '../../api/types'
import { ErrorNotice } from '../../components/ErrorNotice'
import { useI18n } from '../../i18n'
import { fontSize, spacing } from '../../tokens/palette'
import { formatBytes, formatTime } from '../../utils/format'

/**
 * 维护态下的母盘文件管理。
 *
 * 母盘由**服务端本地挂载**后经文件 API 读写，不再通过 iSCSI 挂到客户端 —— 链路短，
 * 也不受客户端 iSCSI 环境影响（这正是之前"挂载母盘"反复卡住的根源）。
 *
 * 页面能力：浏览/上传/下载/新建目录/重命名/删除，以及内嵌文本编辑器（Ctrl/Cmd+S 保存）。
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

  const renameMutation = useMutation({
    mutationFn: (input: { from: string; to: string }) => api.moveParentEntry(repoId, input.from, input.to),
    onSuccess: () => {
      setRename(null)
      refresh()
    },
    onError: (err) => message.error(translateError(err) || t('repo.parentFiles.actionFailed'))
  })

  const saveTextMutation = useMutation({
    mutationFn: (input: { path: string; content: string }) =>
      api.writeParentText(repoId, input.path, input.content),
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

  const columns: TableColumnsType<ParentFileEntryDTO> = [
    {
      title: t('repo.parentFiles.name'),
      dataIndex: 'name',
      key: 'name',
      render: (_, entry) => (
        <Space size={spacing.xs} style={{ cursor: 'pointer' }} onClick={() => void openEntry(entry, setCwd, openEditor)}>
          {entry.is_dir ? <FolderOutlined /> : <FileOutlined />}
          <Typography.Text>{entry.name}</Typography.Text>
        </Space>
      )
    },
    {
      title: t('repo.parentFiles.size'),
      dataIndex: 'size',
      key: 'size',
      width: 120,
      render: (_, entry) => (entry.is_dir ? '-' : formatBytes(entry.size))
    },
    {
      title: t('repo.parentFiles.modifiedAt'),
      dataIndex: 'modified_at',
      key: 'modified_at',
      width: 180,
      render: (value: number) => formatTime(value)
    },
    {
      title: t('common.actions'),
      key: 'actions',
      width: 170,
      render: (_, entry) => (
        <Space size={spacing.xs}>
          {entry.is_dir ? null : (
            <>
              <Tooltip title={t('repo.parentFiles.edit')}>
                <Button type="text" size="small" icon={<EditOutlined />} onClick={() => void openEditor(entry)} />
              </Tooltip>
              <Tooltip title={t('repo.parentFiles.download')}>
                <Button type="text" size="small" icon={<DownloadOutlined />} onClick={() => void download(entry)} />
              </Tooltip>
            </>
          )}
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
  ]

  const crumbs = breadcrumbOf(cwd)

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
        <Button size="small" icon={<ReloadOutlined />} onClick={refresh}>
          {t('common.refresh')}
        </Button>
      </Space>

      <Space size={4} wrap>
        {crumbs.map((crumb) => (
          <Typography.Link key={crumb.path} onClick={() => setCwd(crumb.path)}>
            {crumb.name}
            {crumb.path === '/' ? '' : ' /'}
          </Typography.Link>
        ))}
      </Space>

      {listingQuery.error ? <ErrorNotice error={listingQuery.error} /> : null}

      <Table<ParentFileEntryDTO>
        size="small"
        rowKey={(entry) => entry.path}
        columns={columns}
        dataSource={listingQuery.data?.items ?? []}
        loading={listingQuery.isLoading}
        pagination={false}
        scroll={{ y: 420 }}
      />

      {/* 内嵌编辑器：等宽字体 + Ctrl/Cmd+S 保存。文件大于 2MiB 时服务端会要求下载后本地编辑。 */}
      <Modal
        open={editor !== null}
        title={editor?.path}
        width={960}
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
      >
        <Input.TextArea
          value={editor?.content ?? ''}
          onChange={(event) => setEditor((prev) => (prev ? { ...prev, content: event.target.value } : prev))}
          onKeyDown={(event) => {
            if ((event.ctrlKey || event.metaKey) && event.key.toLowerCase() === 's') {
              event.preventDefault()
              if (editor) saveTextMutation.mutate({ path: editor.path, content: editor.content })
            }
          }}
          spellCheck={false}
          autoSize={{ minRows: 20, maxRows: 28 }}
          style={{ fontFamily: 'Consolas, "Cascadia Code", monospace', fontSize: fontSize.sm, lineHeight: 1.5 }}
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
        confirmLoading={renameMutation.isPending}
        onCancel={() => setRename(null)}
        onOk={() => {
          if (!rename || !rename.name.trim()) return
          renameMutation.mutate({
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
            renameMutation.mutate({
              from: rename.path,
              to: joinParentPath(parentOf(rename.path), rename.name.trim())
            })
          }}
        />
      </Modal>
    </Space>
  )
}

/** 点击条目：目录进入、文件打开编辑器。 */
async function openEntry(
  entry: ParentFileEntryDTO,
  setCwd: (path: string) => void,
  openEditor: (entry: ParentFileEntryDTO) => Promise<void>
): Promise<void> {
  if (entry.is_dir) {
    setCwd(entry.path)
    return
  }
  await openEditor(entry)
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

/** 面包屑：根叫「母盘」，逐级展开。 */
function breadcrumbOf(cwd: string): { name: string; path: string }[] {
  const out = [{ name: '/', path: '/' }]
  let acc = ''
  for (const seg of cwd.split('/').filter(Boolean)) {
    acc += `/${seg}`
    out.push({ name: seg, path: acc })
  }
  return out
}

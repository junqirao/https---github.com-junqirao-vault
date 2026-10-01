import { useEffect, useMemo, useState } from 'react'
import { Breadcrumb, Button, Input, List, Modal, Segmented, Select, Space, Spin, Typography } from 'antd'
import { useQuery } from '@tanstack/react-query'

import { ErrorNotice } from '../../components/ErrorNotice'
import { agentApi, selectDirectory, supportsDirectoryPicker } from '../../api/agentClient'
import type { LocalScanResult } from '../../api/agentTypes'
import { useApi } from '../../api/provider'
import { useI18n } from '../../i18n'
import { formatBytes } from '../../utils/format'
import { spacing } from '../../tokens/palette'

/** 源目录位置：服务端目录 / 客户端本地目录。 */
export type SourceDirKind = 'server' | 'local'

export interface SourceDirValue {
  kind: SourceDirKind
  path: string
}

export interface SourceDirPickerProps {
  value?: SourceDirValue
  onChange?: (value: SourceDirValue | undefined) => void
  /** 本地目录扫描不通过（空目录 / 扫描失败）时为 false，供表单阻止提交。 */
  onValidityChange?: (valid: boolean) => void
}

/**
 * 源目录选择器（受控组件）：
 *  - 服务端目录：只读路径 + 浏览弹窗（走 /v1/fs/*）；
 *  - 客户端本地目录：系统目录选择（Electron）+ 本地代理扫描（/agent/fs/scan）；
 *    Electron 不可用时降级为手动输入路径。
 */
export function SourceDirPicker({ value, onChange, onValidityChange }: SourceDirPickerProps): JSX.Element {
  const { t } = useI18n()
  const [kind, setKind] = useState<SourceDirKind>(value?.kind ?? 'server')
  const [serverPath, setServerPath] = useState<string>(value?.kind === 'server' ? value.path : '')
  const [localPath, setLocalPath] = useState<string>(value?.kind === 'local' ? value.path : '')
  const [browserOpen, setBrowserOpen] = useState(false)
  const [scanning, setScanning] = useState(false)
  const [scan, setScan] = useState<LocalScanResult | null>(null)
  const [scanError, setScanError] = useState<unknown>(null)
  const [pickerError, setPickerError] = useState<unknown>(null)
  // 环境不提供系统选择器（或 IPC 调用失败）时，才降级为可编辑输入；按钮始终可见。
  const [manualFallback, setManualFallback] = useState(false)
  const pickerAvailable = useMemo(() => supportsDirectoryPicker(), [])

  // 表单（外部）回填时同步内部状态。
  useEffect(() => {
    if (!value) return
    setKind(value.kind)
    if (value.kind === 'server') setServerPath(value.path)
    else setLocalPath(value.path)
  }, [value])

  const localValid = scan !== null && scan.file_count > 0
  // 扫描结果的 warnings 统一兜底成数组：接口在无告警时可能给出 null（Go 的 nil 切片），
  // 直接读 .length 会在渲染期抛异常（表现为整页白屏）。
  const scanWarnings = Array.isArray(scan?.warnings) ? scan.warnings : []
  useEffect(() => {
    onValidityChange?.(kind !== 'local' || localValid)
  }, [kind, localValid, onValidityChange])

  const switchKind = (next: SourceDirKind): void => {
    setKind(next)
    onChange?.({ kind: next, path: next === 'server' ? serverPath : localPath })
  }

  const pickServer = (path: string): void => {
    setServerPath(path)
    setBrowserOpen(false)
    onChange?.({ kind: 'server', path })
  }

  const pickLocal = (path: string): void => {
    setLocalPath(path)
    onChange?.({ kind: 'local', path })
  }

  const runScan = async (path: string): Promise<void> => {
    const target = path.trim()
    if (!target) return
    setScanning(true)
    setScanError(null)
    setScan(null)
    try {
      setScan(await agentApi.scanLocalDir(target))
    } catch (error) {
      setScanError(error)
    } finally {
      setScanning(false)
    }
  }

  const handleSelectLocal = async (): Promise<void> => {
    if (!pickerAvailable) {
      // 无系统选择器：给出提示并降级为手动输入（按钮仍在，便于用户知晓原因）。
      setManualFallback(true)
      return
    }
    setPickerError(null)
    try {
      const selection = await selectDirectory()
      if (!selection || selection.canceled || !selection.path) return
      pickLocal(selection.path)
      await runScan(selection.path)
    } catch (error) {
      // IPC 失败（例如主进程未注册该 handler）：不抛未捕获异常，提示后可手填路径。
      setPickerError(error)
      setManualFallback(true)
    }
  }

  return (
    <Space direction="vertical" size={spacing.xs} style={{ width: '100%' }}>
      <Segmented
        value={kind}
        onChange={(next) => switchKind(next as SourceDirKind)}
        options={[
          { label: t('sourceDir.server'), value: 'server' },
          { label: t('sourceDir.local'), value: 'local' }
        ]}
      />
      {kind === 'server' ? (
        <Space.Compact style={{ width: '100%' }}>
          <Input readOnly value={serverPath} placeholder={t('common.optional')} />
          <Button onClick={() => setBrowserOpen(true)}>{t('sourceDir.browse')}</Button>
        </Space.Compact>
      ) : (
        <Space direction="vertical" size={spacing.xxs} style={{ width: '100%' }}>
          <Space.Compact style={{ width: '100%' }}>
            <Input
              readOnly={!manualFallback}
              value={localPath}
              spellCheck={false}
              placeholder={manualFallback ? t('field.path') : t('common.optional')}
              onChange={(event) => {
                if (manualFallback) pickLocal(event.target.value)
              }}
              onBlur={(event) => {
                if (manualFallback) void runScan(event.currentTarget.value)
              }}
              onPressEnter={(event) => {
                if (manualFallback) void runScan(event.currentTarget.value)
              }}
            />
            <Button onClick={() => void handleSelectLocal()} loading={scanning}>
              {t('sourceDir.selectDir')}
            </Button>
          </Space.Compact>
          {manualFallback ? (
            <Typography.Text type="warning">{t('sourceDir.pickerUnavailable')}</Typography.Text>
          ) : null}
          {pickerError ? <ErrorNotice error={pickerError} /> : null}
          {scanning ? (
            <Space size={spacing.xxs}>
              <Spin size="small" />
              <Typography.Text type="secondary">{t('common.loading')}</Typography.Text>
            </Space>
          ) : null}
          {scanError ? <ErrorNotice error={scanError} /> : null}
          {!scanning && scan ? (
            scan.file_count === 0 ? (
              <Typography.Text type="warning">{t('upload.emptyDir')}</Typography.Text>
            ) : (
              <Space size={spacing.xs} wrap>
                <Typography.Text type="secondary">
                  {`${t('sourceDir.fileCount')} ${scan.file_count}`}
                </Typography.Text>
                <Typography.Text type="secondary">
                  {`${t('sourceDir.totalSize')} ${formatBytes(scan.total_bytes)}`}
                </Typography.Text>
                {scanWarnings.length > 0 ? (
                  <Typography.Text type="warning">
                    {t('sourceDir.scanSkipped', { count: scanWarnings.length })}
                  </Typography.Text>
                ) : null}
              </Space>
            )
          ) : null}
        </Space>
      )}
      <ServerDirBrowser
        open={browserOpen}
        initialPath={serverPath}
        onCancel={() => setBrowserOpen(false)}
        onConfirm={pickServer}
      />
    </Space>
  )
}

interface ServerDirBrowserProps {
  open: boolean
  initialPath?: string
  onCancel: () => void
  onConfirm: (path: string) => void
}

/** 服务端目录浏览弹窗：根列表 + 面包屑 + 子目录列表 + 目录统计。 */
function ServerDirBrowser({ open, initialPath, onCancel, onConfirm }: ServerDirBrowserProps): JSX.Element {
  const api = useApi()
  const { t } = useI18n()
  const [current, setCurrent] = useState<string>('')

  const rootsQuery = useQuery({
    queryKey: ['fs-roots'],
    queryFn: () => api.listFsRoots(),
    enabled: open
  })

  useEffect(() => {
    if (!open) return
    setCurrent(initialPath && initialPath.trim() ? initialPath : '')
  }, [open, initialPath])

  // 未指定初始路径时默认落在第一个可浏览根。
  useEffect(() => {
    if (!open || current) return
    const first = rootsQuery.data?.items[0]
    if (first) setCurrent(first.path)
  }, [open, current, rootsQuery.data])

  const browseQuery = useQuery({
    queryKey: ['fs-browse', current],
    queryFn: () => api.browseFs(current),
    enabled: open && current !== ''
  })

  const statQuery = useQuery({
    queryKey: ['fs-stat', current],
    queryFn: () => api.statFs(current),
    enabled: open && current !== ''
  })

  const crumbs = useMemo(() => pathSegments(current), [current])
  const parent = browseQuery.data?.parent
  // 目录不存在（首次建盘前尚未创建）是正常状态，不渲染错误；此时不能从该目录建库。
  const notExists = browseQuery.data?.exists === false

  return (
    <Modal
      open={open}
      title={t('sourceDir.server')}
      width={640}
      centered
      okText={t('sourceDir.selectThis')}
      cancelText={t('common.cancel')}
      okButtonProps={{ disabled: current === '' || notExists }}
      onCancel={onCancel}
      onOk={() => current && onConfirm(current)}
    >
      <Space direction="vertical" size={spacing.sm} style={{ width: '100%' }}>
        <Space size={spacing.xs} wrap>
          <Typography.Text type="secondary">{t('sourceDir.roots')}</Typography.Text>
          <Select
            style={{ minWidth: 240 }}
            loading={rootsQuery.isLoading}
            value={browseQuery.data?.root}
            options={(rootsQuery.data?.items ?? []).map((root) => ({
              value: root.path,
              label: root.exists ? root.name : `${root.name} · ${t('sourceDir.rootMissing')}`
            }))}
            onChange={(next) => setCurrent(next)}
          />
        </Space>
        {rootsQuery.error ? <ErrorNotice error={rootsQuery.error} /> : null}
        {current ? (
          <Breadcrumb
            items={crumbs.map((crumb, index) => ({
              title:
                index === crumbs.length - 1 ? (
                  crumb.label
                ) : (
                  <Typography.Link onClick={() => setCurrent(crumb.path)}>{crumb.label}</Typography.Link>
                )
            }))}
          />
        ) : null}
        {browseQuery.error ? <ErrorNotice error={browseQuery.error} /> : null}
        {notExists ? (
          <Typography.Text type="warning">{t('sourceDir.notExist')}</Typography.Text>
        ) : null}
        {browseQuery.isLoading ? (
          <Spin size="small" />
        ) : notExists ? null : (
          <List
            size="small"
            dataSource={browseQuery.data?.items ?? []}
            locale={{ emptyText: t('sourceDir.empty') }}
            renderItem={(item) => (
              <List.Item
                onClick={() => setCurrent(item.path)}
                style={{ cursor: 'pointer' }}
                extra={item.has_children ? <Typography.Text type="secondary">›</Typography.Text> : null}
              >
                {item.name}
              </List.Item>
            )}
          />
        )}
        {statQuery.error ? <ErrorNotice error={statQuery.error} /> : null}
        <Space size={spacing.sm} wrap>
          <Button size="small" disabled={!parent} onClick={() => parent && setCurrent(parent)}>
            {t('sourceDir.up')}
          </Button>
          {statQuery.isLoading ? (
            <Typography.Text type="secondary">{t('common.loading')}</Typography.Text>
          ) : null}
          {statQuery.data?.exists ? (
            <>
              <Typography.Text type="secondary">
                {`${t('sourceDir.fileCount')} ${statQuery.data.file_count}`}
              </Typography.Text>
              <Typography.Text type="secondary">
                {`${t('sourceDir.totalSize')} ${formatBytes(statQuery.data.total_bytes)}`}
              </Typography.Text>
              {statQuery.data.truncated ? (
                <Typography.Text type="warning">{t('sourceDir.statTruncated')}</Typography.Text>
              ) : null}
            </>
          ) : null}
        </Space>
      </Space>
    </Modal>
  )
}

/** 把绝对路径拆成可点击的面包屑层级（兼容 Windows 反斜杠与 POSIX 斜杠）。 */
function pathSegments(full: string): { label: string; path: string }[] {
  if (!full) return []
  const isWindows = /^[a-zA-Z]:/.test(full)
  const separator = isWindows ? '\\' : '/'
  const parts = full.split(/[\\/]/).filter((part) => part !== '')
  const crumbs: { label: string; path: string }[] = []
  let accumulated = ''
  for (let index = 0; index < parts.length; index += 1) {
    const part = parts[index] ?? ''
    if (index === 0) {
      accumulated = isWindows ? `${part}${separator}` : `${separator}${part}`
    } else {
      accumulated = accumulated.endsWith(separator)
        ? `${accumulated}${part}`
        : `${accumulated}${separator}${part}`
    }
    crumbs.push({ label: part, path: accumulated })
  }
  return crumbs
}

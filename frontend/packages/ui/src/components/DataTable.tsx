import type { ReactNode } from 'react'
import { Table } from 'antd'
import type { TableColumnsType, TableProps } from 'antd'

import { useLayoutMode } from './PageShell'

export interface DataTableProps<T> {
  columns: TableColumnsType<T>
  rows: T[]
  rowKey: (row: T) => string
  loading?: boolean
  empty?: ReactNode
  pagination?: TableProps<T>['pagination']
  size?: TableProps<T>['size']
  scroll?: TableProps<T>['scroll']
}

/** 表格封装：统一分页默认值、行键与空态文案；管理端外壳下默认更紧凑。 */
export function DataTable<T extends object>({
  columns,
  rows,
  rowKey,
  loading,
  empty,
  pagination,
  size,
  scroll
}: DataTableProps<T>): JSX.Element {
  const defaultSize: TableProps<T>['size'] = useLayoutMode() === 'admin' ? 'small' : 'middle'
  return (
    <Table<T>
      rowKey={(row) => rowKey(row)}
      columns={columns}
      dataSource={rows}
      loading={loading}
      size={size ?? defaultSize}
      scroll={scroll}
      pagination={
        pagination === undefined
          ? { pageSize: 20, hideOnSinglePage: true, showSizeChanger: false }
          : pagination
      }
      locale={{ emptyText: empty }}
    />
  )
}

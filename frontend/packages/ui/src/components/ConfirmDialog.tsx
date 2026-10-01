import { Modal } from 'antd'
import type { ReactNode } from 'react'

import { useI18n } from '../i18n'

export interface ConfirmDialogProps {
  open: boolean
  title: ReactNode
  content?: ReactNode
  danger?: boolean
  loading?: boolean
  onConfirm: () => void
  onCancel: () => void
}

/** 确认对话框：用于删除、释放、强制下线等破坏性操作。 */
export function ConfirmDialog({
  open,
  title,
  content,
  danger,
  loading,
  onConfirm,
  onCancel
}: ConfirmDialogProps): JSX.Element {
  const { t } = useI18n()
  return (
    <Modal
      open={open}
      title={title}
      okText={t('common.confirm')}
      cancelText={t('common.cancel')}
      okButtonProps={{ danger, loading }}
      onOk={onConfirm}
      onCancel={onCancel}
      destroyOnClose
      maskClosable={false}
    >
      {content}
    </Modal>
  )
}

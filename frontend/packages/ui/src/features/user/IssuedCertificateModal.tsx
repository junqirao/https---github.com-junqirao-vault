import { Alert, Button, Descriptions, Modal, Space } from 'antd'

import { CopyableText } from '../../components/CopyableText'
import { SectionCard } from '../../components/SectionCard'
import type { IssuedCertificateDTO } from '../../api/types'
import { useI18n } from '../../i18n'
import { spacing } from '../../tokens/palette'
import { formatTime } from '../../utils/format'

export interface IssuedCertificateModalProps {
  open: boolean
  certificate: IssuedCertificateDTO | null
  onClose: () => void
}

/** 证书签发结果弹窗：一次性展示，关闭后不再可见。 */
export function IssuedCertificateModal({ open, certificate, onClose }: IssuedCertificateModalProps): JSX.Element {
  const { t } = useI18n()

  const download = (): void => {
    if (!certificate) return
    const blob = new Blob([buildBundle(certificate)], { type: 'application/x-pem-file' })
    const url = URL.createObjectURL(blob)
    const anchor = document.createElement('a')
    anchor.href = url
    anchor.download = `vault-client-${certificate.serial}.pem`
    anchor.click()
    URL.revokeObjectURL(url)
  }

  return (
    <Modal
      open={open}
      title={t('user.cert.issued')}
      width={640}
      onCancel={onClose}
      footer={
        <Space>
          <Button onClick={onClose}>{t('common.close')}</Button>
          <Button type="primary" disabled={!certificate} onClick={download}>
            {t('user.cert.download')}
          </Button>
        </Space>
      }
      destroyOnClose
      maskClosable={false}
    >
      {certificate ? (
        <Space direction="vertical" size={spacing.md} style={{ width: '100%' }}>
          <Alert type="warning" showIcon message={t('user.cert.privateKeyOnce')} />
          <SectionCard>
            <Descriptions size="small" column={1} bordered>
              <Descriptions.Item label={t('user.cert.serial')}>{certificate.serial}</Descriptions.Item>
              <Descriptions.Item label={t('user.cert.fingerprint')}>
                <CopyableText value={certificate.fingerprint_sha256} monospace />
              </Descriptions.Item>
              <Descriptions.Item label={t('user.cert.validity')}>
                {`${formatTime(certificate.not_before)} ~ ${formatTime(certificate.not_after)}`}
              </Descriptions.Item>
            </Descriptions>
          </SectionCard>
        </Space>
      ) : null}
    </Modal>
  )
}

/** 把证书、私钥与 CA 组装为单个 PEM 文本（段前注释、段间空行）。 */
function buildBundle(certificate: IssuedCertificateDTO): string {
  const section = (comment: string, pem: string): string => `# ${comment}\n${pem.trimEnd()}`
  const bundle = [
    section('Vault client certificate', certificate.cert_pem),
    section('Vault client private key (keep secret)', certificate.key_pem),
    section('Vault CA certificate', certificate.ca_pem)
  ].join('\n\n')
  return `${bundle}\n`
}

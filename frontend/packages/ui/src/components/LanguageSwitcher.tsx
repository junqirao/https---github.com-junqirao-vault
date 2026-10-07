import { GlobalOutlined } from '@ant-design/icons'
import { Button, Dropdown } from 'antd'
import type { MenuProps } from 'antd'

import { LANGUAGE_LABELS, LANGUAGES, type Language } from '../i18n'

export interface LanguageSwitcherProps {
  value: Language
  onChange: (language: Language) => void
  compact?: boolean
}

/**
 * 语言切换器（受控；持久化由宿主应用负责）。
 *
 * 外观是「地球图标 + 当前语言名」，**不带下拉箭头**：它读起来是一个"当前语言"标签，
 * 点击才展开可选语言。原先用的是带箭头的 Select，看起来像表单里的一个输入框。
 */
export function LanguageSwitcher({ value, onChange, compact }: LanguageSwitcherProps): JSX.Element {
  const items: MenuProps['items'] = LANGUAGES.map((language) => ({
    key: language,
    label: LANGUAGE_LABELS[language]
  }))

  return (
    <Dropdown
      menu={{
        items,
        selectable: true,
        selectedKeys: [value],
        onClick: ({ key }) => onChange(key as Language)
      }}
      trigger={['click']}
    >
      <Button type="text" size={compact ? 'small' : 'middle'} icon={<GlobalOutlined />}>
        {LANGUAGE_LABELS[value]}
      </Button>
    </Dropdown>
  )
}

import { Select } from 'antd'

import { LANGUAGE_LABELS, LANGUAGES, type Language } from '../i18n'

export interface LanguageSwitcherProps {
  value: Language
  onChange: (language: Language) => void
  compact?: boolean
}

/** 语言切换器（受控；持久化由宿主应用负责）。 */
export function LanguageSwitcher({ value, onChange, compact }: LanguageSwitcherProps): JSX.Element {
  return (
    <Select<Language>
      value={value}
      onChange={onChange}
      variant="borderless"
      style={{ width: compact ? 120 : 160 }}
      options={LANGUAGES.map((language) => ({ value: language, label: LANGUAGE_LABELS[language] }))}
    />
  )
}

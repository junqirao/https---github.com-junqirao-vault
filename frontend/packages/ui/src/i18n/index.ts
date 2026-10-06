import { useSyncExternalStore } from 'react'

/*
  注册 dayjs 语言包：antd 的日期组件（DatePicker/RangePicker）按 antd locale 里的
  lang.locale 去取 dayjs 的月份/星期名（见 rc-picker 的 getShortMonths），
  语言包没注册时面板会退回英文。这里只"注册"不切换全局 locale，
  因此不会影响任何按固定格式（YYYY-MM-DD 等）格式化出来的字符串。
*/
import 'dayjs/locale/ja'
import 'dayjs/locale/ko'
import 'dayjs/locale/zh-cn'

import enUS from './en-US.json'
import jaJP from './ja-JP.json'
import koKR from './ko-KR.json'
import zhCN from './zh-CN.json'

/** 支持的语言。 */
export type Language = 'zh-CN' | 'en-US' | 'ja-JP' | 'ko-KR'

export const LANGUAGES: readonly Language[] = ['zh-CN', 'en-US', 'ja-JP', 'ko-KR']

/** 各语言的自称，供语言切换器展示。 */
export const LANGUAGE_LABELS: Record<Language, string> = {
  'zh-CN': '简体中文',
  'en-US': 'English',
  'ja-JP': '日本語',
  'ko-KR': '한국어'
}

/** antd 组件库语言包名（由宿主应用映射到具体 locale 对象）。 */
export const ANTD_LOCALE_NAME: Record<Language, 'zh_CN' | 'en_US' | 'ja_JP' | 'ko_KR'> = {
  'zh-CN': 'zh_CN',
  'en-US': 'en_US',
  'ja-JP': 'ja_JP',
  'ko-KR': 'ko_KR'
}

const dictionaries: Record<Language, Record<string, string>> = {
  'zh-CN': zhCN as Record<string, string>,
  'en-US': enUS as Record<string, string>,
  'ja-JP': jaJP as Record<string, string>,
  'ko-KR': koKR as Record<string, string>
}

const FALLBACK_LANGUAGE: Language = 'zh-CN'

let currentLanguage: Language = FALLBACK_LANGUAGE
const listeners = new Set<() => void>()

const warnedKeys = new Set<string>()

/** 是否为开发模式（缺失 key 时打印告警）。 */
function isDev(): boolean {
  try {
    return Boolean(import.meta.env?.DEV)
  } catch {
    return false
  }
}

export function isLanguage(value: unknown): value is Language {
  return typeof value === 'string' && (LANGUAGES as readonly string[]).includes(value)
}

export function getLanguage(): Language {
  return currentLanguage
}

export function setLanguage(language: Language): void {
  if (language === currentLanguage) return
  currentLanguage = language
  for (const listener of listeners) listener()
}

/** 订阅语言变化（供 useSyncExternalStore 使用）。 */
export function subscribeLanguage(listener: () => void): () => void {
  listeners.add(listener)
  return () => listeners.delete(listener)
}

function interpolate(template: string, args?: Record<string, unknown>): string {
  if (!args) return template
  return template.replace(/\{(\w+)\}/g, (match, name: string) => {
    const value = args[name]
    if (value === undefined || value === null) return match
    return String(value)
  })
}

/** 翻译：缺失 key 时回退为 key 本身，开发模式下打印一次告警。 */
export function t(key: string, args?: Record<string, unknown>): string {
  const dict = dictionaries[currentLanguage] ?? dictionaries[FALLBACK_LANGUAGE]
  const template = dict[key]
  if (template === undefined) {
    if (isDev() && !warnedKeys.has(key)) {
      warnedKeys.add(key)
      console.warn(`[i18n] missing key: ${key} (${currentLanguage})`)
    }
    return interpolate(key, args)
  }
  return interpolate(template, args)
}

/** 当前语言下是否存在该 key（用于错误码映射的降级判断）。 */
export function hasKey(key: string): boolean {
  const dict = dictionaries[currentLanguage] ?? dictionaries[FALLBACK_LANGUAGE]
  return dict[key] !== undefined
}

/** React 侧使用入口。 */
export interface I18nApi {
  language: Language
  setLanguage: (language: Language) => void
  t: typeof t
}

/** 语言变化即时生效：组件通过 useSyncExternalStore 订阅模块级语言状态。 */
export function useI18n(): I18nApi {
  const language = useSyncExternalStore(subscribeLanguage, getLanguage, getLanguage)
  return { language, setLanguage, t }
}

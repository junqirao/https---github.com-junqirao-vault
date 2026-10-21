import { useEffect, useRef } from 'react'
import { EditorState } from '@codemirror/state'
import {
  EditorView,
  crosshairCursor,
  drawSelection,
  dropCursor,
  highlightActiveLine,
  highlightActiveLineGutter,
  highlightSpecialChars,
  keymap,
  lineNumbers,
  rectangularSelection
} from '@codemirror/view'
import { defaultKeymap, history, historyKeymap, indentWithTab } from '@codemirror/commands'
import {
  bracketMatching,
  defaultHighlightStyle,
  foldGutter,
  indentOnInput,
  syntaxHighlighting
} from '@codemirror/language'
import { oneDark } from '@codemirror/theme-one-dark'
import { json } from '@codemirror/lang-json'
import { javascript } from '@codemirror/lang-javascript'

export type CodeEditorLanguage = 'json' | 'javascript' | 'text'

export interface CodeEditorProps {
  /** 当前文本；变化时同步进编辑器（外部切换文件）。 */
  value: string
  language?: CodeEditorLanguage
  /** 用户输入时回调。 */
  onChange: (value: string) => void
  /** Ctrl/Cmd+S 回调（浏览器默认保存被拦截）。 */
  onSave?: () => void
  /** 编辑器高度（CSS 值）。 */
  height?: string
}

/**
 * 内嵌代码编辑器（CodeMirror 6）。
 *
 * 为什么不用 textarea：母盘里多为脚本/配置文件，行号、括号匹配、折叠、撤销与选中高亮是
 * 编辑的基本诉求；CodeMirror 纯 JS、无 worker/网络依赖，可直接打进 Electron/Web 包。
 */
export function CodeEditor({
  value,
  language = 'text',
  onChange,
  onSave,
  height = '60vh'
}: CodeEditorProps): JSX.Element {
  const host = useRef<HTMLDivElement | null>(null)
  const viewRef = useRef<EditorView | null>(null)
  // 用 ref 保证编辑器只创建一次，但始终调用最新的回调。
  const onChangeRef = useRef(onChange)
  const onSaveRef = useRef(onSave)
  onChangeRef.current = onChange
  onSaveRef.current = onSave

  useEffect(() => {
    if (!host.current) return
    const languageExt =
      language === 'json' ? json() : language === 'javascript' ? javascript({ typescript: true }) : []
    const view = new EditorView({
      parent: host.current,
      state: EditorState.create({
        doc: value,
        extensions: [
          lineNumbers(),
          highlightActiveLineGutter(),
          highlightSpecialChars(),
          history(),
          foldGutter(),
          drawSelection(),
          dropCursor(),
          EditorState.allowMultipleSelections.of(true),
          indentOnInput(),
          bracketMatching(),
          rectangularSelection(),
          crosshairCursor(),
          highlightActiveLine(),
          syntaxHighlighting(defaultHighlightStyle, { fallback: true }),
          keymap.of([
            {
              key: 'Mod-s',
              run: () => {
                onSaveRef.current?.()
                return true
              }
            },
            ...defaultKeymap,
            ...historyKeymap,
            indentWithTab
          ]),
          EditorView.updateListener.of((update) => {
            if (update.docChanged) onChangeRef.current(update.state.doc.toString())
          }),
          EditorView.theme({
            '&': { height, fontSize: '13px' },
            '.cm-scroller': { overflow: 'auto', fontFamily: 'Consolas, "Cascadia Code", monospace' }
          }),
          oneDark,
          languageExt
        ]
      })
    })
    viewRef.current = view
    return () => {
      view.destroy()
      viewRef.current = null
    }
    // 只在挂载/语言变化时重建；value 的后续变化由下面的同步 effect 处理。
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [language, height])

  // 外部切换文件时同步内容；用户输入已经与 value 一致，不会触发替换（避免光标跳动）。
  useEffect(() => {
    const view = viewRef.current
    if (!view) return
    const current = view.state.doc.toString()
    if (current === value) return
    view.dispatch({ changes: { from: 0, to: current.length, insert: value } })
  }, [value])

  return <div ref={host} style={{ height, overflow: 'hidden', borderRadius: 6, border: '1px solid #30363d' }} />
}

/**
 * 最小 ImportMeta 声明（不依赖 vite/client 类型）。
 *
 * UI 包以源码方式被 Vite 应用引用，构建期会注入 `import.meta.env`。
 */
interface ImportMeta {
  readonly env?: {
    readonly DEV?: boolean
    readonly PROD?: boolean
    readonly MODE?: string
    /** 浏览器 dev 模式下本地代理地址（无 Electron 时的回退）。 */
    readonly VITE_AGENT_URL?: string
    /** 浏览器 dev 模式下本地代理一次性令牌。 */
    readonly VITE_AGENT_TOKEN?: string
  }
}

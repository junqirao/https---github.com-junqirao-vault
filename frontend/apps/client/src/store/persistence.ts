/**
 * 本地配置持久化。
 *
 * Electron 下走 preload 暴露的 IPC 写 `%APPDATA%/Vault/config.json`；
 * 浏览器 dev 模式（无 preload）退回 localStorage。
 */

export interface PersistenceAdapter {
  load: () => Promise<unknown>
  save: (value: unknown) => Promise<void>
}

const LOCAL_STORAGE_KEY = 'vault.client.config'

const localStorageAdapter: PersistenceAdapter = {
  load: async () => {
    const raw = window.localStorage.getItem(LOCAL_STORAGE_KEY)
    if (!raw) return null
    try {
      return JSON.parse(raw) as unknown
    } catch {
      return null
    }
  },
  save: async (value) => {
    window.localStorage.setItem(LOCAL_STORAGE_KEY, JSON.stringify(value))
  }
}

const bridgeAdapter: PersistenceAdapter = {
  load: async () => {
    const bridge = window.vault
    if (!bridge) return null
    return bridge.getConfig()
  },
  save: async (value) => {
    const bridge = window.vault
    if (!bridge) return
    await bridge.setConfig(value)
  }
}

export function createPersistence(): PersistenceAdapter {
  return window.vault ? bridgeAdapter : localStorageAdapter
}

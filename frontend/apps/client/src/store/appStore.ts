import { create } from 'zustand'

import { getLanguage, isLanguage, setLanguage as applyLanguage, type Language, type ServerEntry } from '@vault/ui'

import { createPersistence } from './persistence'

const CONFIG_VERSION = 1

/** 持久化结构（写入 %APPDATA%/Vault/config.json）。 */
interface PersistedConfig {
  version: number
  language: Language
  clientId: string
  mountMode: string
  autoMount: boolean
  servers: ServerEntry[]
  activeKey: string | null
}

export interface AppState extends PersistedConfig {
  hydrated: boolean
  hydrate: () => Promise<void>
  setLanguage: (language: Language) => void
  setMountMode: (mode: string) => void
  setAutoMount: (value: boolean) => void
  setActive: (key: string) => void
  upsertServer: (entry: ServerEntry) => void
  removeServer: (key: string) => void
  updateActiveServer: (patch: Partial<ServerEntry>) => void
  /** 用代理自动续期得到的新令牌更新活动服务端会话（仅更新令牌与过期时间）。 */
  updateActiveToken: (token: string, expiresAt: number) => void
}

const persistence = createPersistence()

function newClientId(): string {
  try {
    return `client-${crypto.randomUUID()}`
  } catch {
    return `client-${Math.random().toString(36).slice(2)}${Date.now().toString(36)}`
  }
}

function isServerEntry(value: unknown): value is ServerEntry {
  if (typeof value !== 'object' || value === null) return false
  const entry = value as Partial<ServerEntry>
  return typeof entry.key === 'string' && typeof entry.baseUrl === 'string'
}

/** 规范化地址（用于"是否同一服务端"的比较）：去空白、去尾部斜杠、主机名大小写不敏感。 */
function normalizeUrl(url: string): string {
  return url.trim().replace(/\/+$/, '').toLowerCase()
}

/**
 * 判断两个条目是否指向**同一个服务端**。
 *
 * 判定依据（任一成立即视为同一个）：
 *  1. 双方都有 `instanceId` 且相等（服务端重装换地址也算同一台）；
 *  2. 规范化后的地址相同（同一台服务端在地址没变时重复添加）。
 *
 * 为什么必须去重：同一台服务端一旦出现多条，用户会在顶栏看到两个一模一样的条目，
 * 会话/证书指纹也会分散在不同条目上（例如早期条目以地址为 key、后来以实例 ID 为 key）。
 */
function isSameServer(a: ServerEntry, b: ServerEntry): boolean {
  if (a.instanceId && b.instanceId && a.instanceId === b.instanceId) return true
  return normalizeUrl(a.baseUrl) === normalizeUrl(b.baseUrl) && normalizeUrl(a.baseUrl) !== ''
}

/** 去掉指向同一服务端的重复条目（保留最后一条，即最新的那条）。 */
function dedupeServers(servers: ServerEntry[]): ServerEntry[] {
  const out: ServerEntry[] = []
  for (const entry of servers) {
    for (let i = out.length - 1; i >= 0; i -= 1) {
      if (isSameServer(out[i], entry)) out.splice(i, 1)
    }
    out.push(entry)
  }
  return out
}

function parsePersisted(raw: unknown): Partial<PersistedConfig> {
  if (typeof raw !== 'object' || raw === null) return {}
  const value = raw as Record<string, unknown>
  const out: Partial<PersistedConfig> = {}
  if (isLanguage(value.language)) out.language = value.language
  if (typeof value.clientId === 'string' && value.clientId) out.clientId = value.clientId
  if (typeof value.mountMode === 'string' && value.mountMode) out.mountMode = value.mountMode
  if (typeof value.autoMount === 'boolean') out.autoMount = value.autoMount
  if (Array.isArray(value.servers)) out.servers = dedupeServers(value.servers.filter(isServerEntry))
  if (typeof value.activeKey === 'string' || value.activeKey === null) {
    const activeKey = value.activeKey as string | null
    // 去重后 activeKey 可能指向已被合并掉的条目：回退到现存条目之一，避免"没有活动服务端"。
    const exists = out.servers?.some((entry) => entry.key === activeKey) ?? false
    out.activeKey = exists ? activeKey : (out.servers?.[out.servers.length - 1]?.key ?? null)
  }
  return out
}

function snapshot(state: AppState): PersistedConfig {
  return {
    version: CONFIG_VERSION,
    language: state.language,
    clientId: state.clientId,
    mountMode: state.mountMode,
    autoMount: state.autoMount,
    servers: state.servers,
    activeKey: state.activeKey
  }
}

function persist(state: AppState): void {
  void persistence.save(snapshot(state))
}

export const useAppStore = create<AppState>()((set, get) => {
  /** 修改状态并落盘。 */
  const update = (patch: Partial<AppState>): void => {
    set(patch as AppState)
    persist(get())
  }

  return {
    hydrated: false,
    version: CONFIG_VERSION,
    language: getLanguage(),
    clientId: newClientId(),
    mountMode: 'letter',
    autoMount: false,
    servers: [],
    activeKey: null,

    hydrate: async () => {
      const raw = await persistence.load()
      const parsed = parsePersisted(raw)
      const language = parsed.language ?? getLanguage()
      applyLanguage(language)
      set({
        ...parsed,
        language,
        hydrated: true
      })
    },

    setLanguage: (language) => {
      applyLanguage(language)
      update({ language })
    },

    setMountMode: (mountMode) => update({ mountMode }),

    setAutoMount: (autoMount) => update({ autoMount }),

    setActive: (key) => update({ activeKey: key }),

    upsertServer: (entry) => {
      // 同一服务端只保留一条：先取出指向同一服务端的旧条目，把其中的会话/展示字段继承过来
      // （避免重新配置地址时丢掉登录态），再从列表里移除它们，最后加入合并结果。
      const previous = get().servers.filter((item) => isSameServer(item, entry))
      const inherited = previous.reduce<Partial<ServerEntry>>((acc, item) => ({ ...acc, ...item }), {})
      const next: ServerEntry = { ...inherited, ...entry }
      const servers = get().servers.filter((item) => !isSameServer(item, entry))
      servers.push(next)
      update({ servers, activeKey: next.key })
    },

    removeServer: (key) => {
      const servers = get().servers.filter((item) => item.key !== key)
      const activeKey = get().activeKey === key ? (servers[0]?.key ?? null) : get().activeKey
      update({ servers, activeKey })
    },

    updateActiveServer: (patch) => {
      const activeKey = get().activeKey
      if (!activeKey) return
      const servers = get().servers.map((item) => (item.key === activeKey ? { ...item, ...patch } : item))
      update({ servers })
    },

    updateActiveToken: (token, expiresAt) => {
      const activeKey = get().activeKey
      if (!activeKey) return
      const servers = get().servers.map((item) =>
        item.key === activeKey ? { ...item, token, tokenExpiresAt: expiresAt } : item
      )
      update({ servers })
    }
  }
})

/** 当前活动服务端（派生读取）。 */
export function selectActiveServer(state: AppState): ServerEntry | null {
  if (!state.activeKey) return null
  return state.servers.find((item) => item.key === state.activeKey) ?? null
}

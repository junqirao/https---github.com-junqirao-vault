import crypto from 'node:crypto'
import { promises as fs } from 'node:fs'
import path from 'node:path'
import { BrowserWindow, Menu, Tray, app, dialog, ipcMain, nativeImage, shell } from 'electron'

import { AgentManager, listProcesses, runPowerShell, type AgentRequestOptions, type NamedProcess } from './agent'
import { resolveBundledVersion, resolveWebappEntry } from './webapp'

/**
 * Electron 主进程。
 *
 * 职责（严格最小化）：
 *  - 单实例锁；
 *  - 创建窗口（记忆窗口尺寸）；
 *  - 系统托盘（显示/退出）；
 *  - 通过 IPC 读写本地配置与本地代理状态；
 *  - 拉起并守护本地代理 Vault-Agent.exe（找不到时降级，不影响应用可用性）。
 *
 * 安全约束：不开启 nodeIntegration，开启 contextIsolation，preload 只暴露最小 API。
 */

/** 配置目录固定为 %APPDATA%/Vault（与需求一致）。 */
const CONFIG_DIR = path.join(app.getPath('appData'), 'Vault')
const CONFIG_FILE = path.join(CONFIG_DIR, 'config.json')
const WINDOW_STATE_FILE = path.join(CONFIG_DIR, 'window-state.json')
/** 记录"上次清理渲染缓存时的应用版本"（与用户配置同目录，不受缓存清理影响）。 */
const CACHE_VERSION_FILE = path.join(CONFIG_DIR, 'renderer-cache-version.json')

const DEV_SERVER_URL = process.env.VITE_DEV_SERVER_URL

let mainWindow: BrowserWindow | null = null
let tray: Tray | null = null
let quitting = false

/**
 * 内置渲染层（随 exe 打包）的版本号：createWindow 时解析一次，之后用于热更层判定
 * （热更层必须比它新才会被加载，见 resolveWebappEntry）。
 */
let bundledVersion = ''
/**
 * 当前已加载的渲染层入口文件（绝对路径）。
 *
 * 用途：静默热更在代理激活新资源后要求切换渲染层（vault:apply-web-layer），
 * 这里用"解析出的文件 == 已加载文件"来判定无需切换，从而**杜绝重载循环**。
 */
let loadedRendererFile = ''

/** 本地代理守护器（单例）。 */
const agent = new AgentManager()

/**
 * 版本变化时清理渲染进程缓存。
 *
 * 目的：避免 Chromium 磁盘缓存让界面加载到旧资源（表现为"客户端一直访问旧内容"）。
 *
 * 取舍：另一种做法是把 userData 路径改为包含应用版本（app.setPath('userData', ...)），
 * 使不同版本各自使用独立的缓存目录。但用户配置与窗口状态本就存放在
 * %APPDATA%/Vault（CONFIG_DIR，与 userData 无关），改 userData 虽不会真正丢配置，
 * 却会让缓存目录随版本无限增长，且更容易被误解为"配置丢失"。
 * 因此这里采用副作用更小、更稳妥的方式：仅在**应用版本变化**时删除
 * Cache / Code Cache / GPUCache 三个缓存目录，用户配置完整保留。
 *
 * 在模块加载阶段即启动（早于窗口创建），确保 Chromium 尚未锁定缓存文件。
 */
async function pruneRendererCacheOnVersionChange(): Promise<void> {
  const version = app.getVersion()
  const record = await readJson<{ version?: string }>(CACHE_VERSION_FILE)
  if (record?.version === version) return
  const userData = app.getPath('userData')
  for (const name of ['Cache', 'Code Cache', 'GPUCache']) {
    try {
      await fs.rm(path.join(userData, name), { recursive: true, force: true })
    } catch {
      // 缓存被占用（Chromium 已启动）时忽略：下次版本变化或手动清理即可，不影响启动。
    }
  }
  await writeJson(CACHE_VERSION_FILE, { version })
}

/** 尽早启动缓存清理；主窗口创建前会 await 它。 */
const rendererCachePruned = pruneRendererCacheOnVersionChange()

/**
 * 已固化的服务端证书指纹（TOFU 首次信任），见 certificate-error 的处理。
 *
 * 与用户配置同目录，按"站点 → 允许的证书指纹集合"存储。
 */
const TRUSTED_CERTS_FILE = path.join(CONFIG_DIR, 'trusted-certs.json')

/** 站点 → 允许的证书指纹集合（小写十六进制）。 */
const trustedCertificates = new Map<string, Set<string>>()

/** 载入已固化的证书指纹；主窗口创建前会 await 它（首个请求发出前必须就绪）。 */
const trustedCertificatesLoaded = (async (): Promise<void> => {
  const record = await readJson<Record<string, unknown>>(TRUSTED_CERTS_FILE)
  if (!record) return
  for (const [origin, value] of Object.entries(record)) {
    if (!Array.isArray(value)) continue
    const set = new Set(value.filter((item): item is string => typeof item === 'string'))
    if (set.size > 0) trustedCertificates.set(origin, set)
  }
})()

/** 把内存中的证书指纹写入磁盘。 */
async function persistTrustedCertificates(): Promise<void> {
  const record: Record<string, string[]> = {}
  for (const [origin, set] of trustedCertificates) record[origin] = [...set]
  await writeJson(TRUSTED_CERTS_FILE, record)
}

/** 规范化站点标识（协议 + 主机 + 端口）；无法解析时返回空串。 */
function originOf(url: string): string {
  try {
    const parsed = new URL(url)
    return `${parsed.protocol}//${parsed.host}`
  } catch {
    return ''
  }
}

/** 计算证书 DER 的 SHA-256（小写十六进制）；算法与 Go 侧 certFingerprint 一致。 */
function certificateSha256(pem: string): string {
  const base64 = pem
    .replace(/-----BEGIN CERTIFICATE-----/g, '')
    .replace(/-----END CERTIFICATE-----/g, '')
    .replace(/\s+/g, '')
  return crypto.createHash('sha256').update(Buffer.from(base64, 'base64')).digest('hex')
}

/**
 * 自签证书放行策略（TOFU 首次信任）。
 *
 * 服务端使用自建 CA，Chromium 会以"证书链不可信"为由拒绝它的**所有**请求
 * （等价于 curl 的 SEC_E_UNTRUSTED_ROOT），表现就是"测试连接成功、保存后却什么也做不了"。
 *
 * 这里**不做无差别放行**：只有实际证书的 SHA-256 与已登记指纹完全一致才放行。
 * 指纹来自本地代理的连通性探测（该探测有意跳过链校验，只回传指纹供人工核对），
 * 用户核对服务端名称/指纹后点击"测试连接/保存"即视为完成首次信任。
 */
app.on('certificate-error', (event, _webContents, url, _error, certificate, callback) => {
  void (async (): Promise<void> => {
    await trustedCertificatesLoaded
    const origin = originOf(url)
    const allowed = origin ? trustedCertificates.get(origin) : undefined
    if (allowed && allowed.has(certificateSha256(certificate.data))) {
      event.preventDefault()
      callback(true)
      return
    }
    callback(false)
  })()
})

interface WindowState {
  width: number
  height: number
  x?: number
  y?: number
  /** 写入时的布局版本；与当前版本不一致时忽略已保存的尺寸（见 loadWindowState）。 */
  layoutVersion?: number
}

/** 默认窗口尺寸：以宽屏为主（内容区需要横向排布表格 + 表单）。 */
const DEFAULT_WINDOW: WindowState = { width: 1440, height: 900 }

/** 窗口最小尺寸。 */
const MIN_WINDOW = { width: 1180, height: 720 }

/**
 * 窗口布局版本。
 *
 * 递增它会让**已保存的窗口尺寸失效**（下次启动回到新的默认尺寸），但保留窗口位置。
 * 用途：调整默认尺寸时让存量用户也能生效——否则"没手动调过窗口"的用户永远沿用旧默认值。
 * 用户自己拖过窗口后写入的是当前版本号，之后再改不会覆盖他的选择。
 */
const WINDOW_LAYOUT_VERSION = 2

async function readJson<T>(file: string): Promise<T | null> {
  try {
    const raw = await fs.readFile(file, 'utf8')
    return JSON.parse(raw) as T
  } catch {
    return null
  }
}

async function writeJson(file: string, value: unknown): Promise<void> {
  await fs.mkdir(path.dirname(file), { recursive: true })
  await fs.writeFile(file, JSON.stringify(value, null, 2), 'utf8')
}

async function loadWindowState(): Promise<WindowState> {
  const state = await readJson<WindowState>(WINDOW_STATE_FILE)
  if (!state || typeof state.width !== 'number' || typeof state.height !== 'number') {
    return { ...DEFAULT_WINDOW }
  }
  if (state.layoutVersion !== WINDOW_LAYOUT_VERSION) {
    return { ...DEFAULT_WINDOW, x: state.x, y: state.y }
  }
  return {
    ...state,
    width: Math.max(state.width, MIN_WINDOW.width),
    height: Math.max(state.height, MIN_WINDOW.height)
  }
}

/**
 * 下发给渲染进程的代理状态：**去掉令牌**。
 *
 * 令牌只留在主进程，渲染进程的所有代理调用都经 vault:agent-request 转发（见 registerIpc）。
 */
function publicAgentInfo(): Omit<ReturnType<AgentManager['getInfo']>, 'token'> {
  const { token: _token, ...rest } = agent.getInfo()
  return rest
}

/** 每个渲染进程各持有一条代理事件流（SSE）；key 为 webContents.id。 */
const agentStreams = new Map<number, () => void>()

function closeAgentStream(id: number): void {
  const close = agentStreams.get(id)
  if (!close) return
  agentStreams.delete(id)
  close()
}

/** 只允许渲染进程访问代理的 /agent/ 前缀路径，避免借主进程之手访问任意地址。 */
function parseAgentRequest(value: unknown): AgentRequestOptions | null {
  if (typeof value !== 'object' || value === null) return null
  const input = value as { method?: unknown; path?: unknown; body?: unknown; timeoutMs?: unknown }
  const method = typeof input.method === 'string' ? input.method.toUpperCase() : ''
  if (!['GET', 'POST', 'PATCH', 'DELETE'].includes(method)) return null
  const requestPath = typeof input.path === 'string' ? input.path : ''
  // 必须是以 /agent/ 开头的站内路径：不允许协议、主机或 .. 逃逸。
  if (!requestPath.startsWith('/agent/') || requestPath.startsWith('//') || requestPath.includes('..')) {
    return null
  }
  const timeoutMs =
    typeof input.timeoutMs === 'number' && Number.isFinite(input.timeoutMs) && input.timeoutMs > 0
      ? Math.min(input.timeoutMs, 300000)
      : undefined
  return { method, path: requestPath, body: input.body, timeoutMs }
}

function registerIpc(): void {
  ipcMain.handle('vault:get-config', async () => readJson<unknown>(CONFIG_FILE))
  ipcMain.handle('vault:set-config', async (_event, value: unknown) => {
    await writeJson(CONFIG_FILE, value)
    return true
  })
  ipcMain.handle('vault:config-path', () => CONFIG_DIR)
  ipcMain.handle('vault:agent-info', () => publicAgentInfo())
  ipcMain.handle('vault:agent-request', async (_event, options: unknown) => {
    const parsed = parseAgentRequest(options)
    if (!parsed) return { status: 0, error: 'network', message: 'invalid_request' }
    return agent.request(parsed)
  })
  ipcMain.handle('vault:agent-events-start', (event) => {
    const id = event.sender.id
    closeAgentStream(id)
    const close = agent.openEventStream((message) => {
      if (event.sender.isDestroyed()) return
      event.sender.send('vault:agent-event', message)
    })
    agentStreams.set(id, close)
    // 渲染进程销毁（关闭窗口/刷新）时释放长连接，避免连接泄漏。
    event.sender.once('destroyed', () => closeAgentStream(id))
    return true
  })
  ipcMain.handle('vault:agent-events-stop', (event) => {
    closeAgentStream(event.sender.id)
    return true
  })
  // 登记服务端证书指纹（TOFU 首次信任）；只有指纹与登记值一致的自签证书才会被放行。
  ipcMain.handle('vault:trust-certificate', async (_event, payload: unknown) => {
    if (typeof payload !== 'object' || payload === null) return false
    const input = payload as { serverUrl?: unknown; sha256?: unknown }
    const serverUrl = typeof input.serverUrl === 'string' ? input.serverUrl : ''
    const sha256 = typeof input.sha256 === 'string' ? input.sha256.trim().toLowerCase() : ''
    const origin = originOf(serverUrl)
    if (!origin || !/^[0-9a-f]{64}$/.test(sha256)) return false
    // 必须先载入历史指纹，否则会把已有记录覆盖掉。
    await trustedCertificatesLoaded
    const set = trustedCertificates.get(origin) ?? new Set<string>()
    set.add(sha256)
    trustedCertificates.set(origin, set)
    await persistTrustedCertificates()
    return true
  })
  ipcMain.handle('vault:open-path', async (_event, target: unknown) => {
    if (typeof target !== 'string' || !target) return 'invalid_path'
    return shell.openPath(target)
  })
  // 选择源目录（单选目录）；用户取消时返回 canceled=true。
  ipcMain.handle('vault:select-directory', async () => {
    const result = await dialog.showOpenDialog({ properties: ['openDirectory'] })
    if (result.canceled || result.filePaths.length === 0) return { canceled: true }
    return { canceled: false, path: result.filePaths[0] }
  })
  ipcMain.handle('vault:show-item', async (_event, target: unknown) => {
    if (typeof target === 'string' && target) shell.showItemInFolder(target)
    return true
  })
  ipcMain.handle('vault:quit', async () => {
    await agent.shutdown()
    quitting = true
    app.quit()
  })
  // 立即重启客户端，让已激活的渲染层热更资源生效（见 resolveWebappEntry）。
  ipcMain.handle('vault:relaunch', async () => {
    await agent.shutdown()
    quitting = true
    app.relaunch()
    app.exit(0)
  })
  // 让"刚刚激活的热更渲染层"立刻生效：重新解析热更目录并加载，无需重启整个客户端。
  //
  // 由渲染层在收到代理的 web_update(activated) 事件后调用（静默热更，见
  // internal/agent 的 onSessionEstablished）。返回 applied=false 表示"当前已是最新层"
  // （或没有可用的热更层）——调用方据此无需做任何事，也不会形成重载循环。
  ipcMain.handle('vault:apply-web-layer', async () => {
    if (!mainWindow) return { applied: false, reason: 'no_window' }
    const entry = await resolveWebappEntry(bundledVersion)
    if (!entry.ok) return { applied: false, reason: entry.reason }
    if (entry.file === loadedRendererFile) return { applied: false, reason: 'already_loaded' }
    try {
      await mainWindow.loadFile(entry.file)
      loadedRendererFile = entry.file
      console.info(`[vault] 已自动切换到新渲染层：${entry.dir}`)
      return { applied: true, version: entry.dir }
    } catch (error) {
      const reason = error instanceof Error ? error.message : String(error)
      console.warn(`[vault] 切换渲染层失败（保持当前界面）：${reason}`)
      return { applied: false, reason }
    }
  })
}

/** 代理状态变化（含重启换端口）时广播给所有渲染进程。 */
function broadcastAgentInfo(): void {
  const info = publicAgentInfo()
  for (const window of BrowserWindow.getAllWindows()) {
    if (!window.isDestroyed()) window.webContents.send('vault:agent-changed', info)
  }
}

/** 关闭窗口：仍有活跃挂载时先确认；选择"仅关闭窗口"则最小化到托盘。 */
async function handleCloseRequest(window: BrowserWindow): Promise<void> {
  const count = await agent.activeMountCount()
  if (count <= 0) {
    window.hide()
    return
  }
  const { response } = await dialog.showMessageBox(window, {
    type: 'warning',
    title: 'Vault',
    message: `仍有 ${count} 个存储库处于挂载状态，确定退出吗？`,
    buttons: ['退出', '仅关闭窗口', '取消'],
    defaultId: 1,
    cancelId: 2,
    noLink: true
  })
  if (response === 0) {
    await agent.shutdown()
    quitting = true
    app.quit()
    return
  }
  if (response === 1) window.hide()
}

async function createWindow(): Promise<void> {
  const state = await loadWindowState()
  mainWindow = new BrowserWindow({
    width: state.width,
    height: state.height,
    x: state.x,
    y: state.y,
    minWidth: MIN_WINDOW.width,
    minHeight: MIN_WINDOW.height,
    show: false,
    backgroundColor: '#F5F5F7',
    title: 'Vault',
    webPreferences: {
      preload: path.join(__dirname, 'preload.js'),
      nodeIntegration: false,
      contextIsolation: true,
      sandbox: true
    }
  })

  mainWindow.once('ready-to-show', () => mainWindow?.show())

  const persistState = async (): Promise<void> => {
    if (!mainWindow || mainWindow.isDestroyed()) return
    const bounds = mainWindow.getNormalBounds()
    await writeJson(WINDOW_STATE_FILE, {
      width: bounds.width,
      height: bounds.height,
      x: bounds.x,
      y: bounds.y,
      layoutVersion: WINDOW_LAYOUT_VERSION
    })
  }

  mainWindow.on('resized', () => void persistState())
  mainWindow.on('moved', () => void persistState())
  mainWindow.on('closed', () => {
    mainWindow = null
  })

  // 关闭窗口不直接退出：有活跃挂载时先确认，否则最小化到托盘。
  mainWindow.on('close', (event) => {
    if (quitting || !mainWindow) return
    event.preventDefault()
    void handleCloseRequest(mainWindow)
  })

  // 外部链接一律用系统浏览器打开，避免在应用内导航。
  mainWindow.webContents.setWindowOpenHandler(({ url }) => {
    void shell.openExternal(url)
    return { action: 'deny' }
  })

  if (DEV_SERVER_URL) {
    await mainWindow.loadURL(DEV_SERVER_URL)
    return
  }
  // 生产环境：解析内置层版本后加载渲染层（详见 loadRendererLayer）。
  bundledVersion = await resolveBundledVersion(app.getAppPath(), app.getVersion())
  await loadRendererLayer()
}

/**
 * 加载渲染层：优先热更资源（%ProgramData%\Vault\webapp\<version>），但**只在热更层比
 * 内置层新时**才采用（比较见 resolveWebappEntry 的说明：否则旧的热更资源会把界面永久
 * 钉在老版本上）。任何一步失败都回退打包内置的 dist/index.html，绝不让界面打不开。
 *
 * 记录实际加载的文件路径，供 applyWebLayer 判断"要不要切换"（同路径即视为已切换，
 * 从而杜绝重载循环）。
 */
async function loadRendererLayer(): Promise<void> {
  if (!mainWindow) return
  const entry = await resolveWebappEntry(bundledVersion)
  if (entry.ok) {
    try {
      await mainWindow.loadFile(entry.file)
      loadedRendererFile = entry.file
      console.info(`[vault] 已加载热更渲染层：${entry.dir}（内置版本 ${bundledVersion}）`)
      return
    } catch (error) {
      const reason = error instanceof Error ? error.message : String(error)
      console.warn(`[vault] 热更渲染层加载失败，回退内置资源：${reason}`)
    }
  } else {
    console.warn(`[vault] 未启用热更渲染层（${entry.reason}），使用内置资源`)
  }
  const bundled = path.join(__dirname, '../dist/index.html')
  await mainWindow.loadFile(bundled)
  loadedRendererFile = bundled
  console.info(`[vault] 已加载内置渲染层（版本 ${bundledVersion}）`)
}

/**
 * 托盘图标：内嵌 32×32 PNG（Vault 标记：圆角方形底 + 白色 V），
 * 内嵌而非读文件，避免打包后找不到资源；32px 在 100%/200% 缩放下都是整数倍缩放，最清晰。
 */
function createTrayIcon(): Electron.NativeImage {
  const png =
    'iVBORw0KGgoAAAANSUhEUgAAACAAAAAgCAYAAABzenr0AAAAAXNSR0IArs4c6QAAAARnQU1BAACxjwv8YQUAAAAJcEhZcwAADsMAAA7DAcdvqGQAAAICSURBVFhH7ZchTMNAFIbn2BUSCAocOHCTkziQOHBDIXaFgYAQBFlCCAgMwYFAIMBgERMIEIAiCEKCgQQBhgCGJYh75Gfp1nuv67p2TRD8yWe6//r+vrvrrpnMv0LUW6K+bk1jSlM5LhjvFGmA3ztUPS6NKNfcOC5Rp1CueXGKZoLXEnJcmnFcU+U36BToCq9Zl1OkXJrF6zTrhNLmXphTANORKVDWKt5VoiFuTBM1Z/JWAOXSJDelidJmwQ6gqcxNHsOrRLl1eT0MjBldk9c9xGJsFmD5hOqaP5a/BzG1R/TxVRtzeCV/B5EDPL01AlS/ifJb0uNncIno9bMxBsI17oscoHJn3+z6UXr87J7ZfjwA94DIAfDEXLOH0teuN3IAwJ8KLQ5q6+2z7QvrVlsBguZ1/8L2YIH61Wq9tBUAoJVcXgFsOW/Ve0LX+D0SBQDnD3YRtBzXsdX8Qrf6F+X4xAHwxGitX7w4hPcAH8uJFQBsV3g5W9i2fEwQsQOgtXxBekJ3or6yYwcAaHGQNk6ltxmJAgD+hsQbr9XC85M4AP7pvK2H1o/vSE8YIkDtLCiNYWD/Fw6izztjxQ7wex4UptTAAcgKAClt3rkxHUw18FshzjTEQcy/X45rjviAjqLNpTgRc2GBpPF9oLTZbFncE47pWChoV1Ky2kzjc4/X+BP6AYrvFkLf9xIUAAAAAElFTkSuQmCC'
  return nativeImage.createFromDataURL(`data:image/png;base64,${png}`)
}

function createTray(): void {
  try {
    tray = new Tray(createTrayIcon())
    tray.setToolTip('Vault')
    tray.setContextMenu(
      Menu.buildFromTemplate([
        { label: '显示窗口', click: () => showWindow() },
        { type: 'separator' },
        {
          label: '退出',
          click: () => {
            void agent.shutdown().finally(() => {
              quitting = true
              app.quit()
            })
          }
        }
      ])
    )
    tray.on('click', () => showWindow())
  } catch {
    // 无图形环境（如 CI）时静默跳过托盘，不影响窗口本身。
    tray = null
  }
}

function showWindow(): void {
  if (!mainWindow) {
    void createWindow()
    return
  }
  if (mainWindow.isMinimized()) mainWindow.restore()
  mainWindow.show()
  mainWindow.focus()
}

/** 结束旧实例后等待其退出（Electron 的单实例锁由系统在进程结束后释放）的最长时间。 */
const TAKEOVER_WAIT_MS = 3000
const TAKEOVER_POLL_MS = 150
/** 重启自身时写入的标记：确保重启后的进程不会再尝试接管（防极端情况下反复重启）。 */
const TAKEOVER_DONE_ENV = 'VAULT_TAKEOVER_DONE'
/**
 * 接管时经环境变量把"旧安装目录"传给重启后的进程。
 *
 * 为什么需要传递：旧的 Vault-Agent.exe 位于**旧安装目录**下，默认不在客户端的
 * "可清理目录"白名单里，会一直占着 Global\VaultAgent 锁导致新代理起不来。
 * 而接管动作发生在即将退出的进程里，白名单必须交给重启后的进程才能真正生效。
 */
const TAKEOVER_ROOTS_ENV = 'VAULT_TAKEOVER_STALE_ROOTS'

/** 应用接管时继承下来的"可清理代理目录"，并清除环境变量（避免再传给子进程）。 */
function applyInheritedStaleRoots(): void {
  const raw = process.env[TAKEOVER_ROOTS_ENV]
  delete process.env[TAKEOVER_ROOTS_ENV]
  if (!raw) return
  for (const dir of raw.split(path.delimiter)) {
    const trimmed = dir.trim()
    if (trimmed) agent.allowStaleRoot(trimmed)
  }
}

type TakeoverOutcome =
  /** 已有实例（同一安装目录的重复启动）：保持原行为，让位给它。 */
  | { kind: 'same' }
  /** 已结束旧实例：重启自身以干净地取得单实例锁。 */
  | { kind: 'takeover' }
  /** 运行中的实例版本更高或无法结束：提示用户先退出它。 */
  | { kind: 'blocked'; path: string }

/** 解析 "0.1.9" / "0.1.9.0" 形式的版本号，便于逐段比较。 */
function parseVersion(value: string): number[] {
  return value.split('.').map((part) => {
    const parsed = Number.parseInt(part.replace(/[^0-9].*$/, ''), 10)
    return Number.isFinite(parsed) ? parsed : 0
  })
}

/** a 是否比 b 新（逐段比较，长度不同按 0 补齐）。 */
function isNewerVersion(a: string, b: string): boolean {
  const left = parseVersion(a)
  const right = parseVersion(b)
  const length = Math.max(left.length, right.length)
  for (let i = 0; i < length; i += 1) {
    const x = left[i] ?? 0
    const y = right[i] ?? 0
    if (x !== y) return x > y
  }
  return false
}

/** 一次 PowerShell 调用读取多个可执行文件的 FileVersion（由 electron-builder 写入）。 */
async function readFileVersions(paths: string[]): Promise<Map<string, string>> {
  const versions = new Map<string, string>()
  if (paths.length === 0) return versions
  const quoted = paths.map((item) => `'${item.replace(/'/g, "''")}'`).join(',')
  const script =
    `@(${quoted}) | ForEach-Object { ` +
    `[pscustomobject]@{ Path = $_; Version = (Get-Item -LiteralPath $_).VersionInfo.FileVersion } } | ` +
    `ConvertTo-Json -Compress`
  try {
    const stdout = (await runPowerShell(script)).trim()
    if (!stdout) return versions
    const parsed = JSON.parse(stdout) as unknown
    for (const row of Array.isArray(parsed) ? parsed : [parsed]) {
      if (typeof row !== 'object' || row === null) continue
      const value = row as { Path?: unknown; Version?: unknown }
      if (typeof value.Path === 'string' && typeof value.Version === 'string') {
        versions.set(value.Path, value.Version)
      }
    }
  } catch {
    // 读不到版本：留空，调用方按"无法确认"处理（保守起见不接管）。
  }
  return versions
}

/**
 * 判断"拿不到单实例锁"时该怎么办。
 *
 * 背景：Electron 的单实例锁按 userData（%APPDATA%/Vault）判定，**与安装目录无关**。
 * 因此当本机还跑着另一个安装目录下的旧客户端时，新客户端会拿不到锁、静默退出，旧进程
 * 把窗口顶到前台——用户看到的永远是旧界面（即"客户端一直访问旧内容"）。
 *
 * 处置规则：
 *   1. 运行中的实例与本进程同目录 → 这是同一份安装的重复启动，让位（原行为）；
 *   2. 运行中的实例来自其它目录且版本更低 → 结束它并回收其代理，重启自身；
 *   3. 运行中的实例版本更高 → 提示用户先退出它，不做任何强杀。
 */
async function takeoverOlderInstance(): Promise<TakeoverOutcome> {
  // 已经接管过一次（本进程由接管后的 relaunch 启动）：不再重复，直接让位。
  if (process.env[TAKEOVER_DONE_ENV] === '1') return { kind: 'same' }

  const ownDir = path.dirname(app.getPath('exe'))
  let processes: NamedProcess[]
  try {
    processes = await listProcesses('Vault.exe')
  } catch {
    return { kind: 'same' } // 枚举失败：保持原行为，不冒险
  }

  const others = processes.filter((proc) => proc.pid !== process.pid && proc.exePath !== '')
  if (others.length === 0) return { kind: 'same' }

  const ownResolved = path.resolve(ownDir).toLowerCase()
  const sameInstall = others.some(
    (proc) => path.resolve(path.dirname(proc.exePath)).toLowerCase() === ownResolved
  )
  if (sameInstall) return { kind: 'same' }

  const ownVersion = app.getVersion()
  const versions = await readFileVersions(others.map((proc) => proc.exePath))
  // 只有当"每一个运行中的实例版本都比本进程低"时才接管；读不到版本同样视为不能接管。
  const blocker = others.find((proc) => {
    const other = versions.get(proc.exePath)
    return !other || !isNewerVersion(ownVersion, other)
  })
  if (blocker) return { kind: 'blocked', path: blocker.exePath }

  // 本进程版本更高：结束旧实例，并把它们的目录记入"可清理代理"白名单。
  const staleRoots = new Set<string>()
  for (const proc of others) {
    const dir = path.dirname(proc.exePath)
    staleRoots.add(dir)
    agent.allowStaleRoot(dir)
    try {
      process.kill(proc.pid, 'SIGKILL')
    } catch {
      // 结束失败（权限不足/已自行退出）：下面的存活检查会再次判定。
    }
  }
  // 白名单要交给重启后的进程才能真正生效（本进程即将退出）。
  process.env[TAKEOVER_ROOTS_ENV] = [...staleRoots].join(path.delimiter)

  const deadline = Date.now() + TAKEOVER_WAIT_MS
  for (;;) {
    const alive = (await listProcesses('Vault.exe').catch(() => [])).filter(
      (proc) => proc.pid !== process.pid && proc.exePath !== ''
    )
    if (alive.length === 0) return { kind: 'takeover' }
    if (Date.now() >= deadline) return { kind: 'blocked', path: alive[0]?.exePath ?? others[0].exePath }
    await new Promise((resolve) => setTimeout(resolve, TAKEOVER_POLL_MS))
  }
}

/** 作为主实例运行（唯一实例，正常路径）。 */
function startPrimaryInstance(): void {
  app.on('second-instance', () => showWindow())

  app.whenReady().then(async () => {
    // 应用自带完整 UI，不需要 Electron 默认的 File/Edit/View/Window/Help 菜单栏。
    // 托盘菜单（createTray）是独立对象，不受影响。
    Menu.setApplicationMenu(null)
    // 主窗口创建前完成渲染缓存清理（版本变化时），避免加载到旧资源。
    await rendererCachePruned
    // 已固化的服务端证书指纹必须在任何请求发出前就绪（否则首个自签证书请求会被拒）。
    await trustedCertificatesLoaded
    // 若本进程是"接管旧实例"后重启的，先继承旧安装目录，供代理清理陈旧进程。
    applyInheritedStaleRoots()
    registerIpc()
    agent.onChanged(broadcastAgentInfo)
    await createWindow()
    createTray()
    // 代理在后台异步拉起；找不到可执行文件时进入降级模式，不影响窗口。
    agent.start()
    app.on('activate', () => {
      if (BrowserWindow.getAllWindows().length === 0) void createWindow()
      else showWindow()
    })
  })

  // 关闭窗口时保留托盘（真正退出走托盘菜单或系统退出）。
  app.on('window-all-closed', () => {
    if (process.platform !== 'darwin' && quitting) app.quit()
  })

  // 应用退出时停止守护，避免代理成为孤儿进程。
  app.on('will-quit', () => agent.stop())
}

const gotLock = app.requestSingleInstanceLock()
if (gotLock) {
  startPrimaryInstance()
} else {
  void (async () => {
    const outcome = await takeoverOlderInstance()
    if (outcome.kind === 'takeover') {
      // 旧实例已结束：重启自身一次，让新进程干净地取得单实例锁。
      // 本进程已提权（requireAdministrator），创建同样要求的子进程不会再弹 UAC。
      process.env[TAKEOVER_DONE_ENV] = '1'
      app.relaunch()
      app.exit(0)
      return
    }
    if (outcome.kind === 'blocked') {
      await app.whenReady()
      dialog.showMessageBoxSync({
        type: 'warning',
        title: 'Vault',
        message: '已有一个 Vault 客户端在运行',
        detail:
          '无法自动接管运行中的实例（它的版本更高，或读不到版本信息）。\n\n' +
          `路径：${outcome.path}\n\n` +
          '请先在任务管理器里结束这些 Vault 进程，再启动本客户端。',
        buttons: ['确定'],
        noLink: true
      })
    }
    app.quit()
  })()
}

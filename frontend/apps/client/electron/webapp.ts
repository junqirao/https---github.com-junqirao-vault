import { promises as fs } from 'node:fs'
import os from 'node:os'
import path from 'node:path'

/**
 * 客户端渲染层资源热更的加载目录解析。
 *
 * 落盘布局（与 Go 侧 internal/agent 的**冻结契约**）：
 *   <webapp>/<version>/index.html + assets/**
 *   <webapp>/current.json = {"version","dir","activated_at"}
 *
 * 解析失败（未热更、指针损坏、目录可疑、index.html 缺失）一律返回失败原因，
 * 由调用方回退到打包内置的 dist/index.html —— 绝不因热更资源损坏而打不开界面。
 */

/** 代理数据目录：与 Go 侧 agent.DataDir() 保持一致（%ProgramData%\Vault，缺失时回退临时目录）。 */
export function agentDataDir(): string {
  const programData = (process.env.ProgramData ?? '').trim()
  if (programData !== '') return path.join(programData, 'Vault')
  return path.join(os.tmpdir(), 'Vault')
}

/** 版本目录名只允许安全字符（显式拒绝 ..、/、\、: 与任何路径分隔符，防路径穿越）。 */
const SAFE_DIR_NAME = /^[A-Za-z0-9._-]+$/

/**
 * 内置渲染层（随 exe 打包进 app.asar 的那一份）的版本号。
 *
 * 刻意**不直接用 `app.getVersion()`**：该 API 只在"应用 package.json 有 version 字段"时
 * 返回应用版本，否则会回退成 **Electron 自身的大版本号**（如 33.x）。一旦回退发生，
 * 热更层（0.1.x）就会被判为"不比内置层新"而**永远不被加载** —— 正是最难排查的那类故障。
 * 因此这里直接读打包目录里的 package.json，读不到时才退回 `app.getVersion()`。
 */
export async function resolveBundledVersion(appPath: string, fallback: string): Promise<string> {
  try {
    const raw = await fs.readFile(path.join(appPath, 'package.json'), 'utf8')
    const parsed: unknown = JSON.parse(raw)
    if (typeof parsed === 'object' && parsed !== null) {
      const version = (parsed as { version?: unknown }).version
      if (typeof version === 'string' && version.trim() !== '') return version.trim()
    }
  } catch {
    // 读不到就用调用方给的兜底值（app.getVersion()）。
  }
  return fallback
}

/** 激活指针（current.json）中我们关心的字段。 */
interface CurrentPointer {
  dir?: unknown
  version?: unknown
}

export type WebappEntryResult =
  | { ok: true; dir: string; file: string }
  | { ok: false; reason: string }

/** 把版本号解析为数字段；含非数字段（如 1.2.x）时返回 null。 */
function parseVersion(value: string): number[] | null {
  const cleaned = value.trim().replace(/^v/i, '').split(/[-+]/)[0]
  if (cleaned === '') return null
  const parts = cleaned.split('.')
  const nums: number[] = []
  for (const part of parts) {
    if (!/^\d+$/.test(part)) return null
    nums.push(Number(part))
  }
  return nums.length > 0 ? nums : null
}

/**
 * 版本比较：a 比 b 新返回正数，旧返回负数，无法比较（含相等）返回 0。
 *
 * 只比较数字段，缺位补 0（0.1 == 0.1.0）；预发布后缀（-beta 等）忽略，
 * 因此 `0.1.38-beta` > `0.1.37` ✓，而 `0.1.37-beta` 与 `0.1.37` 视为相等（不采用）。
 */
export function compareVersions(a: string, b: string): number {
  const left = parseVersion(a)
  const right = parseVersion(b)
  if (!left || !right) return 0
  const len = Math.max(left.length, right.length)
  for (let i = 0; i < len; i += 1) {
    const x = left[i] ?? 0
    const y = right[i] ?? 0
    if (x !== y) return x > y ? 1 : -1
  }
  return 0
}

/**
 * 解析要加载的热更渲染层目录。
 *
 * ok:true 时 `file` 为可直接 `loadFile` 的 index.html 绝对路径；
 * 否则 `reason` 说明失败原因（供调用方打印 WARN 并回退内置资源）。
 *
 * `bundledVersion` 是**随 exe 打包进 app.asar 的渲染层版本**（app.getVersion()）：
 * 热更层只有在**比它新**时才会被采用。
 *
 * ⚠️ 这条比较是必须的，缺了会造成最难排查的一类问题：热更目录（%ProgramData%\Vault\webapp）
 * 里留着旧版本的界面资源时，客户端**永远加载那一份**——换了 exe、重新打了包，
 * 界面/版本号/样式却纹丝不动（真实工单：客户端一直显示 v0.1.28，改的前端样式全不见效）。
 * 版本缺失或不可解析时同样退回内置资源：内置层与 exe 天然同源，永远自洽。
 */
export async function resolveWebappEntry(bundledVersion: string): Promise<WebappEntryResult> {
  const webappDir = path.resolve(path.join(agentDataDir(), 'webapp'))

  let raw: string
  try {
    raw = await fs.readFile(path.join(webappDir, 'current.json'), 'utf8')
  } catch {
    return { ok: false, reason: 'current_json_missing' }
  }

  let pointer: CurrentPointer
  try {
    const parsed: unknown = JSON.parse(raw)
    if (typeof parsed !== 'object' || parsed === null) return { ok: false, reason: 'current_json_invalid' }
    pointer = parsed as CurrentPointer
  } catch {
    return { ok: false, reason: 'current_json_invalid' }
  }

  const hotVersion = typeof pointer.version === 'string' ? pointer.version.trim() : ''
  if (hotVersion === '') return { ok: false, reason: 'hotupdate_version_missing' }
  if (compareVersions(hotVersion, bundledVersion) <= 0) {
    return { ok: false, reason: `hotupdate_not_newer(${hotVersion}<=${bundledVersion})` }
  }

  const dir = typeof pointer.dir === 'string' ? pointer.dir.trim() : ''
  if (dir === '') return { ok: false, reason: 'dir_missing' }
  if (!SAFE_DIR_NAME.test(dir) || dir === '.' || dir.includes('..')) {
    return { ok: false, reason: 'dir_unsafe' }
  }

  const dirPath = path.resolve(webappDir, dir)
  // 双保险：解析后的目录必须仍在 webapp 根目录的下一层（越界即回退）。
  if (path.dirname(dirPath) !== webappDir) return { ok: false, reason: 'dir_escaped' }

  const file = path.join(dirPath, 'index.html')
  try {
    const stat = await fs.stat(file)
    if (!stat.isFile()) return { ok: false, reason: 'index_not_file' }
  } catch {
    return { ok: false, reason: 'index_missing' }
  }

  return { ok: true, dir: dirPath, file }
}

/**
 * 目录模式挂载路径的**展示**计算。
 *
 * 与代理侧 internal/agent/mount.go 的 resolveMountDir / mountDirLeaf / sanitizePathSegment
 * 保持同一套规则：界面告诉用户"实际挂载到哪"，就必须真的挂到那 —— 两边一旦漂移，
 * 用户会按界面提示去别的目录找文件（真实反馈："目录下默认多一级要提前说清楚"）。
 */

/** 清洗一段路径名：非法字符换成短横线，去掉首尾的点与空格（对齐 Go 侧同名函数）。 */
export function sanitizePathSegment(name: string): string {
  const cleaned = (name ?? '').replace(/[\u0000-\u001f\\/:*?"<>|]/g, '-')
  return cleaned.replace(/^[ .]+/, '').replace(/[ .]+$/, '')
}

/** 目录模式下自动生成的那一级目录名：`<服务端名称>_<存储库名称>`。 */
export function mountDirLeaf(serverAlias: string, repoName: string): string {
  const server = sanitizePathSegment(serverAlias) || 'vault'
  const repo = sanitizePathSegment(repoName)
  return repo === '' ? server : `${server}_${repo}`
}

/** 取路径最后一段（兼容 Windows 反斜杠与 POSIX 斜杠）。 */
function baseName(path: string): string {
  const index = Math.max(path.lastIndexOf('\\'), path.lastIndexOf('/'))
  return index >= 0 ? path.slice(index + 1) : path
}

/** 去掉路径末尾的分隔符（对齐 Go 的 filepath.Clean 在展示层面的效果）。 */
function trimTrailingSeparators(path: string): string {
  const trimmed = path.trim()
  const stripped = trimmed.replace(/[\\/]+$/, '')
  if (stripped === '') return trimmed
  // 盘符根（C:\）是合法父目录：不能剥成 "C:"（那是驱动器的相对路径）。
  return /^[A-Za-z]:$/.test(stripped) ? `${stripped}\\` : stripped
}

/**
 * 目录模式的最终挂载点：**父目录** + 一层 `<服务端名称>_<存储库名称>`。
 *
 * 父目录末段已经是那一层时原样返回（与代理侧同为幂等）：重挂/恢复传回来的是上次的最终
 * 挂载点，界面在这里不能把它再拼一次，否则预览与实际挂载点会不一致。
 */
export function effectiveMountDir(parentDir: string, serverAlias: string, repoName: string): string {
  const leaf = mountDirLeaf(serverAlias, repoName)
  const root = trimTrailingSeparators(parentDir)
  if (root === '') return ''
  if (baseName(root).toLowerCase() === leaf.toLowerCase()) return root
  return `${root}\\${leaf}`
}

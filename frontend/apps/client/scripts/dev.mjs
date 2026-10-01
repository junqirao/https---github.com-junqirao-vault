/**
 * 开发模式启动脚本：先编译 Electron 主进程，再启动 Vite dev server 并拉起 Electron。
 *
 * 避免引入 concurrently / wait-on 等额外依赖。
 */
import { spawn } from 'node:child_process'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { createRequire } from 'node:module'
import { createServer } from 'vite'

const require = createRequire(import.meta.url)
const appDir = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')

function run(command, args) {
  return new Promise((resolve, reject) => {
    // windowsHide：避免在 Windows 上为 cmd 垫片弹出控制台窗口。
    const child = spawn(command, args, {
      cwd: appDir,
      stdio: 'inherit',
      shell: process.platform === 'win32',
      windowsHide: true
    })
    child.on('exit', (code) => (code === 0 ? resolve() : reject(new Error(`${command} exited with ${code}`))))
  })
}

async function main() {
  await run(process.execPath, [require.resolve('typescript/bin/tsc'), '-p', 'tsconfig.electron.json'])

  const server = await createServer({ configFile: path.join(appDir, 'vite.config.mts') })
  await server.listen()
  const address = server.httpServer?.address()
  const port = typeof address === 'object' && address ? address.port : 5173
  const url = `http://localhost:${port}`

  const electronPath = require('electron')
  const child = spawn(electronPath, ['.'], {
    cwd: appDir,
    stdio: 'inherit',
    env: { ...process.env, VITE_DEV_SERVER_URL: url }
  })

  child.on('exit', async () => {
    await server.close()
    process.exit(0)
  })
}

main().catch((error) => {
  console.error(error)
  process.exit(1)
})

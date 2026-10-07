'use strict'

/**
 * electron-builder 的 afterPack 钩子：把"许可"从随包分发改成外链分发。
 *
 * 背景：Electron 分发包自带两个许可文件——
 *   · LICENSE.electron.txt        Electron 本体许可（MIT），约 1 KB；
 *   · LICENSES.chromium.html      Chromium 及其第三方组件的许可全文，约 9 MB。
 * 后者占客户端整体体积的 4% 左右，而客户端里没有任何入口会去读它，属于纯粹的体积负担。
 *
 * 做法（只动许可文件，不动任何运行时文件）：
 *   · 保留 LICENSE.electron.txt —— MIT 明确要求"版权声明与许可声明随所有副本分发"，成本也
 *     只有 1 KB，没有删的理由；
 *   · 删除 LICENSES.chromium.html，改写一份 1 KB 左右的 THIRD-PARTY-NOTICES.txt，把许可
 *     全文的官方在线地址写在里面；
 *   · 客户端托盘菜单的「开源许可」（见 electron/main.ts）直接打开这些地址。
 *
 * 合规依据：许可义务的核心是"让接收方能够获得许可与版权声明"，不是"必须把某个 9 MB 文件
 * 原样放进包里"。改外链后，接收方仍能从随包的 Electron MIT 全文 + NOTICE 里的官方地址
 * 拿到 Chromium 及第三方组件的完整许可，义务不被规避。
 *
 * 如果发布方希望提供一个和旧包完全一致的完整清单（例如放到官网/更新服务上自托管），
 * 构建时设置环境变量 VAULT_THIRD_PARTY_LICENSES_URL=<地址>：该地址会被写进 NOTICE 并成为
 * 首选入口。要上传的原始文件，就是打包前产物目录 win-unpacked 下的 LICENSES.chromium.html
 * （备用输出目录 release-alt / release-build-N 下同样有一份）。
 */

const fs = require('node:fs')
const path = require('node:path')

/** Electron 分发包自带的 Chromium 许可清单（要被外链化掉的那个大文件）。 */
const CHROMIUM_LICENSES = 'LICENSES.chromium.html'
/** Electron 本体许可（MIT）：保留，不参与精简。 */
const ELECTRON_LICENSE = 'LICENSE.electron.txt'
/** 外链化后随包提供的说明文件。 */
const NOTICE_FILE = 'THIRD-PARTY-NOTICES.txt'

/** Electron 官方许可地址（MIT 全文）。 */
const ELECTRON_LICENSE_URL = 'https://github.com/electron/electron/blob/main/LICENSE'
/** Chromium 官方许可地址（BSD 3-Clause 全文）。 */
const CHROMIUM_LICENSE_URL =
  'https://chromium.googlesource.com/chromium/src/+/refs/heads/main/LICENSE'

/** 自托管的完整清单地址（可选，由构建环境变量提供）。 */
const SELF_HOSTED_URL = (process.env.VAULT_THIRD_PARTY_LICENSES_URL || '').trim()

/** 递归统计目录体积（只在收尾阶段跑一次，产物只有几十个文件）。 */
function dirSize(dir) {
  let total = 0
  for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
    const full = path.join(dir, entry.name)
    try {
      if (entry.isDirectory()) total += dirSize(full)
      else total += fs.statSync(full).size
    } catch {
      // 统计失败不影响打包，忽略。
    }
  }
  return total
}

function formatSize(bytes) {
  return `${(bytes / 1024 / 1024).toFixed(2)} MB`
}

/**
 * 定位 Chromium 许可清单。
 *
 * Windows / Linux 上它位于产物目录根（与 Vault.exe 同级），macOS 上在 .app 的
 * Contents/Resources 里，所以两处都找一遍，找不到就跳过（例如将来 Electron 改了分发包
 * 结构，也只是没东西可精简，不应让构建失败）。
 */
function findChromiumLicenses(packager, appOutDir) {
  const candidates = [path.join(appOutDir, CHROMIUM_LICENSES)]
  try {
    const resources = packager.getResourcesDir(appOutDir)
    candidates.push(path.join(resources, CHROMIUM_LICENSES))
    candidates.push(path.join(resources, '..', CHROMIUM_LICENSES))
  } catch {
    // getResourcesDir 在个别平台/目标下可能不可用，忽略附加候选。
  }
  for (const candidate of candidates) {
    const resolved = path.resolve(candidate)
    if (fs.existsSync(resolved)) return resolved
  }
  return null
}

function buildNotice() {
  const lines = [
    'Vault 客户端 第三方许可说明（THIRD-PARTY NOTICES）',
    '='.repeat(64),
    '',
    '本程序基于 Electron 构建，包含 Chromium 及其第三方组件。',
    '',
    '-'.repeat(64),
    '一、Electron（MIT）',
    '-'.repeat(64),
    `许可全文随本程序分发，见同目录下的 ${ELECTRON_LICENSE}。`,
    `在线地址：${ELECTRON_LICENSE_URL}`,
    '',
    '-'.repeat(64),
    '二、Chromium 及其第三方组件',
    '-'.repeat(64),
    'Chromium 及其第三方组件的完整许可清单约 9 MB。为控制分发包体积，该清单未随',
    '本程序分发；许可全文可通过下列地址获取：',
    ''
  ]
  if (SELF_HOSTED_URL) {
    lines.push(`  · 完整许可清单（发布方提供）：${SELF_HOSTED_URL}`)
  } else {
    lines.push('  · 完整许可清单：请联系软件发布方获取（可自托管为在线页面）')
  }
  lines.push(
    `  · Chromium 许可全文（BSD 3-Clause）：${CHROMIUM_LICENSE_URL}`,
    `  · Electron 许可全文（MIT）：${ELECTRON_LICENSE_URL}`,
    '',
    '客户端内「开源许可」入口（系统托盘右键菜单 → 开源许可）可直接打开上述在线地址。',
    ''
  )
  return lines.join('\r\n')
}

/**
 * @param {import('electron-builder').AfterPackContext} context
 */
module.exports = async function afterPack(context) {
  const { appOutDir, packager } = context
  if (!appOutDir || !fs.existsSync(appOutDir)) return

  const sizeBefore = dirSize(appOutDir)

  const licenses = findChromiumLicenses(packager, appOutDir)
  if (licenses) {
    const saved = fs.statSync(licenses).size
    fs.rmSync(licenses, { force: true })
    const noticePath = path.join(path.dirname(licenses), NOTICE_FILE)
    // 带 BOM 写：这是给 Windows 用户直接打开看的说明文件，与 build.py 的 _write_text 保持一致
    // （无 BOM 的 UTF-8 在记事本/PowerShell 5.1 下会显示成乱码）。
    fs.writeFileSync(noticePath, `\ufeff${buildNotice()}`, 'utf8')
    console.log(
      `  • 许可外链化: 删除 ${path.basename(licenses)}（${formatSize(saved)}）` +
        `，写入 ${NOTICE_FILE}`
    )
  } else {
    console.log(`  • 许可外链化: 未找到 ${CHROMIUM_LICENSES}，跳过（不影响打包）`)
  }

  const sizeAfter = dirSize(appOutDir)
  console.log(
    `  • 产物体积: ${formatSize(sizeBefore)} → ${formatSize(sizeAfter)}` +
      `（本次收尾减少 ${formatSize(sizeBefore - sizeAfter)}）`
  )
}

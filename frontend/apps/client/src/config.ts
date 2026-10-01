/**
 * 客户端版本。
 *
 * 真源是 `package.json` 的 `version`：由 vite 的 define 在构建期注入（见 vite.config.mts），
 * 因此界面展示、上报服务端做兼容性判定、发布包版本三者永远一致，无需手工同步。
 */
export const CLIENT_VERSION = __APP_VERSION__

/** 本地配置文件名（用于界面提示，不含敏感内容）。 */
export const CONFIG_FILE_HINT = '%APPDATA%/Vault/config.json'

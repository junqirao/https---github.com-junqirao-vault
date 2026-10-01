/** @vault/ui —— 唯一 UI 真源（管理端与用户端共用）。 */

// 设计 token
export * from './tokens'

// i18n
export {
  ANTD_LOCALE_NAME,
  LANGUAGES,
  LANGUAGE_LABELS,
  getLanguage,
  hasKey,
  isLanguage,
  setLanguage,
  t,
  useI18n,
  type I18nApi,
  type Language
} from './i18n'

// API 客户端
export { ApiClient, buildQuery, type ApiClientConfig, type ApiClientOptions, type QueryValue } from './api/client'
export { VaultApi } from './api/endpoints'
export {
  ApiError,
  businessErrorFrom,
  errorCodeOf,
  errorMessageOf,
  isApiError,
  translateError,
  type ApiErrorKind
} from './api/errors'
export { ApiProvider, useApi } from './api/provider'
export * from './api/types'
export * from './api/agentTypes'
export {
  agentApi,
  AgentUnavailableError,
  applyWebLayer,
  getAgentVersionInfo,
  identityMatchesServer,
  isAgentUnavailableError,
  onAgentEndpointChanged,
  parseAgentEvent,
  relaunchClient,
  selectDirectory,
  subscribeAgentVersion,
  supportsDirectoryPicker,
  trustServerCertificate,
  type AgentVersionInfo,
  type DirectorySelection,
  type VaultAgentInfo,
  type VaultBridge
} from './api/agentClient'

// hooks
export {
  subscribeAgentSession,
  useAgent,
  useAgentEventNotifier,
  useAgentVersion,
  type AgentMountInput,
  type AgentUnmountInput,
  type UseAgentResult
} from './hooks/useAgent'
export { useErrorNotifier } from './hooks/useErrorNotifier'
export { AuthProvider, useAuth, usePermission, type AuthContextValue, type SessionUser } from './hooks/useAuth'
export {
  ServerConfigProvider,
  useServerConfig,
  type ServerConfigValue,
  type ServerEntry
} from './hooks/useServerConfig'

// 通用组件
export { ConfirmDialog, type ConfirmDialogProps } from './components/ConfirmDialog'
export { CopyableText, type CopyableTextProps } from './components/CopyableText'
export { DataTable, type DataTableProps } from './components/DataTable'
export { EmptyState, type EmptyStateProps } from './components/EmptyState'
export { ErrorBoundary } from './components/ErrorBoundary'
export { ErrorNotice, type ErrorNoticeProps } from './components/ErrorNotice'
export { LanguageSwitcher, type LanguageSwitcherProps } from './components/LanguageSwitcher'
export { LoadingState, type LoadingStateProps } from './components/LoadingState'
export {
  PageShell,
  LayoutModeProvider,
  useLayoutMode,
  type PageShellProps,
  type LayoutMode
} from './components/PageShell'
export { PasswordStrength, type PasswordStrengthProps } from './components/PasswordStrength'
export { SectionCard, type SectionCardProps } from './components/SectionCard'
export { StatusTag, type StatusTagProps } from './components/StatusTag'

// 业务视图
export { LoginForm, type LoginFormProps, type LoginFormValues } from './features/auth/LoginForm'
export {
  BlockedNotice,
  CompatNotice,
  OfflineNotice,
  type BlockedNoticeProps,
  type CompatNoticeProps,
  type OfflineNoticeProps
} from './features/bootstrap/StartupNotice'
export {
  BootstrapWizard,
  type BootstrapInputValues,
  type BootstrapWizardProps
} from './features/bootstrap/BootstrapWizard'
export { ServerEndpointForm, type ServerEndpointFormProps } from './features/server/ServerEndpointForm'
export { MyRepos, type MyReposProps } from './features/repo/MyRepos'
export { RepoList, type RepoListProps } from './features/repo/RepoList'
export { RepoCreateForm, type RepoCreateFormProps } from './features/repo/RepoCreateForm'
export {
  SourceDirPicker,
  type SourceDirKind,
  type SourceDirPickerProps,
  type SourceDirValue
} from './features/repo/SourceDirPicker'
export { RepoDetail, type RepoDetailProps } from './features/repo/RepoDetail'
export { LogViewer } from './features/logs/LogViewer'
export { UserList } from './features/user/UserList'
export { UserForm, type UserFormProps } from './features/user/UserForm'
export {
  IssuedCertificateModal,
  type IssuedCertificateModalProps
} from './features/user/IssuedCertificateModal'
export { LeaseList, type LeaseListProps } from './features/lease/LeaseList'
export { JobList } from './features/job/JobList'
export { AuditList } from './features/audit/AuditList'
export { StorageList } from './features/storage/StorageList'
export { OrphanDisks } from './features/system/OrphanDisks'
export { SystemSettings, type SystemSettingsProps } from './features/system/SystemSettings'
export { ClientSettings, type ClientSettingsProps } from './features/settings/ClientSettings'

// 工具
export { formatBytes, formatTime, maskSecret, remainingSeconds, shortId } from './utils/format'
export {
  allocationStateLabel,
  auditResultLabel,
  diskKindLabel,
  diskStateLabel,
  jobStateLabel,
  jobTypeLabel,
  leaseStateLabel,
  parentActionLabel,
  parentConditionLabel,
  permissionLabel,
  repoModeLabel,
  repoStateLabel,
  roleLabel
} from './utils/labels'
export {
  CLIENT_API_VERSION,
  DEFAULT_SERVER_URL,
  PASSWORD_MAX,
  PASSWORD_MIN,
  USERNAME_MAX,
  USERNAME_MIN,
  hasLetterAndDigit,
  isValidHttpUrl,
  isValidPassword,
  isValidUsername,
  normalizeBaseUrl
} from './utils/validation'

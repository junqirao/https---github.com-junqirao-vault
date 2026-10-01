import { Navigate, Route, Routes, useNavigate, useParams } from 'react-router-dom'

import {
  AuditList,
  ClientSettings,
  EmptyState,
  JobList,
  LeaseList,
  LogViewer,
  MountList,
  MyRepos,
  PageShell,
  RepoCreateForm,
  RepoDetail,
  RepoList,
  StorageList,
  SystemSettings,
  UserList,
  useAuth,
  useI18n,
  type Language
} from '@vault/ui'

import { AppLayout } from '../layouts/AppLayout'
import { useAppStore } from '../store/appStore'
import type { AutoMountProgress } from '../flow/useStartupFlow'

export interface AppRoutesProps {
  language: Language
  onLanguageChange: (language: Language) => void
  onManageServers: () => void
  /** 自动挂载进度（由本地代理执行，前端只展示）。 */
  autoMount: AutoMountProgress | null
}

/** 阻止非超级管理员访问管理模块（整棵 /admin/* 子树统一包裹）。 */
function RequireSuperAdmin({ children }: { children: JSX.Element }): JSX.Element {
  const { isSuperAdmin } = useAuth()
  const { t } = useI18n()
  if (!isSuperAdmin) {
    return (
      <PageShell title={t('page.forbidden.title')}>
        <EmptyState title={t('page.forbidden.title')} description={t('page.forbidden.desc')} />
      </PageShell>
    )
  }
  return children
}

function RepoDetailRoute(): JSX.Element {
  const { repoId } = useParams<{ repoId: string }>()
  const navigate = useNavigate()
  const { isSuperAdmin, user } = useAuth()
  const clientId = useAppStore((state) => state.clientId)
  if (!repoId) return <Navigate to="/my-repos" replace />
  return (
    <RepoDetail
      repoId={repoId}
      isSuperAdmin={isSuperAdmin}
      currentUserId={user?.id ?? ''}
      clientId={clientId}
      onBack={() => navigate(-1)}
    />
  )
}

function ClientSettingsRoute(): JSX.Element {
  const setLanguage = useAppStore((state) => state.setLanguage)
  const language = useAppStore((state) => state.language)
  return <ClientSettings language={language} onLanguageChange={setLanguage} />
}

function NotFoundRoute(): JSX.Element {
  const { t } = useI18n()
  return (
    <PageShell title={t('page.notFound.title')}>
      <EmptyState title={t('page.notFound.title')} />
    </PageShell>
  )
}

/** 路由表：应用端与管理端各自独立的外壳与菜单，互不混用。 */
export function AppRoutes({ language, onLanguageChange, onManageServers, autoMount }: AppRoutesProps): JSX.Element {
  const navigate = useNavigate()
  const { isSuperAdmin } = useAuth()

  return (
    <Routes>
      {/* 应用外壳：面向普通用户的存储库 / 挂载 / 设置 */}
      <Route
        element={
          <AppLayout
            mode="app"
            language={language}
            onLanguageChange={onLanguageChange}
            onManageServers={onManageServers}
            autoMount={autoMount}
          />
        }
      >
        <Route index element={<Navigate to="/my-repos" replace />} />
        <Route
          path="my-repos"
          element={
            <MyRepos
              onCreate={() => navigate('/my-repos/new')}
              onOpen={(repoId) => navigate(`/repos/${repoId}`)}
            />
          }
        />
        <Route
          path="my-repos/new"
          element={
            <RepoCreateForm
              isSuperAdmin={false}
              onCreated={(repo) => navigate(`/repos/${repo.id}`)}
              onOpenRepo={(repoId) => navigate(`/repos/${repoId}`)}
              onCancel={() => navigate('/my-repos')}
            />
          }
        />
        <Route path="mounts" element={<MountList />} />
        <Route path="logs" element={<LogViewer />} />
        <Route path="repos/:repoId" element={<RepoDetailRoute />} />
        <Route path="settings" element={<ClientSettingsRoute />} />
        <Route path="*" element={<NotFoundRoute />} />
      </Route>

      {/* 管理外壳：仅超级管理员可达，整棵子树共用一层守卫 */}
      <Route
        path="admin"
        element={
          <RequireSuperAdmin>
            <AppLayout
              mode="admin"
              language={language}
              onLanguageChange={onLanguageChange}
              onManageServers={onManageServers}
              autoMount={autoMount}
            />
          </RequireSuperAdmin>
        }
      >
        <Route index element={<Navigate to="/admin/users" replace />} />
        <Route path="users" element={<UserList />} />
        <Route
          path="repos"
          element={
            <RepoList
              isSuperAdmin
              onCreate={() => navigate('/admin/repos/new')}
              onOpen={(repoId) => navigate(`/repos/${repoId}`)}
            />
          }
        />
        <Route
          path="repos/new"
          element={
            <RepoCreateForm
              isSuperAdmin
              onCreated={(repo) => navigate(`/repos/${repo.id}`)}
              onOpenRepo={(repoId) => navigate(`/repos/${repoId}`)}
              onCancel={() => navigate('/admin/repos')}
            />
          }
        />
        <Route path="leases" element={<LeaseList canRevoke />} />
        <Route path="storages" element={<StorageList />} />
        <Route path="jobs" element={<JobList />} />
        <Route path="audit" element={<AuditList />} />
        <Route path="logs" element={<LogViewer />} />
        <Route path="system" element={<SystemSettings isSuperAdmin={isSuperAdmin} />} />
      </Route>
    </Routes>
  )
}

import { Navigate, Route, Routes, useNavigate, useParams } from 'react-router-dom'

import {
  AboutPage,
  AuditList,
  ClientSettings,
  EmptyState,
  JobList,
  LeaseList,
  LogViewer,
  MyRepos,
  OrphanDisks,
  PageShell,
  RepoCreateForm,
  RepoDetail,
  RepoList,
  StorageCreateForm,
  StorageList,
  SystemSettings,
  UserList,
  useAuth,
  useI18n,
  type Language
} from '@vault/ui'

import { AppLayout } from '../layouts/AppLayout'
// 客户端版本由宿主注入（真源是 apps/client/package.json，构建期 define 进 CLIENT_VERSION）。
import { CLIENT_VERSION } from '../config'
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
  if (!repoId) return <Navigate to="/my-repos" replace />
  return (
    <RepoDetail
      repoId={repoId}
      isSuperAdmin={isSuperAdmin}
      currentUserId={user?.id ?? ''}
      currentUserName={user?.username ?? ''}
      // 详情的「返回」一律回到存储库列表，不做 navigate(-1)：从新建页跳进详情时，
      // 历史里那张已经提交过的表单还在，后退会把用户送回去（表单已重置，看着像白填了）。
      onBack={() => navigate(isSuperAdmin ? '/admin/repos' : '/my-repos', { replace: true })}
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
              // 建库成功即离开新建页：用 replace 把新建页从历史里顶掉，
              // 否则以后在任何地方按浏览器后退都会回到那张已提交过的空表单。
              onCreated={(repo) => navigate(`/repos/${repo.id}`, { replace: true })}
              onOpenRepo={(repoId) => navigate(`/repos/${repoId}`, { replace: true })}
              onCancel={() => navigate('/my-repos')}
            />
          }
        />
        <Route path="logs" element={<LogViewer />} />
        <Route path="repos/:repoId" element={<RepoDetailRoute />} />
        <Route path="settings" element={<ClientSettingsRoute />} />
        {/* 「关于」是页面而不是弹窗：链接能分享、刷新能回来、前进后退也对得上
            （弹窗关掉就等于没去过，被问"你那边什么版本"时没法把它留在那儿对着念）。 */}
        <Route path="about" element={<AboutPage clientVersion={CLIENT_VERSION} />} />
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
              // 同应用端：建库成功后新建页不再留在历史里，详情页返回只回存储库列表。
              onCreated={(repo) => navigate(`/repos/${repo.id}`, { replace: true })}
              onOpenRepo={(repoId) => navigate(`/repos/${repoId}`, { replace: true })}
              onCancel={() => navigate('/admin/repos')}
            />
          }
        />
        <Route path="leases" element={<LeaseList canRevoke />} />
        <Route path="storages" element={<StorageList onCreate={() => navigate('/admin/storages/new')} />} />
        <Route
          path="storages/new"
          element={
            <StorageCreateForm
              // 建存储成功即离开新建页：用 replace 顶掉新建页，
              // 否则以后在别处按浏览器后退会回到那张已提交过的空表单。
              onCreated={() => navigate('/admin/storages', { replace: true })}
              onCancel={() => navigate('/admin/storages')}
            />
          }
        />
        <Route path="orphans" element={<OrphanDisks />} />
        <Route path="jobs" element={<JobList />} />
        <Route path="audit" element={<AuditList />} />
        {/* 管理端看的是**服务端**日志（GET /v1/system/logs，仅超管）；
            客户端自己的代理日志在应用端 /logs，两者不是一回事。 */}
        <Route path="logs" element={<LogViewer source="server" />} />
        <Route path="system" element={<SystemSettings isSuperAdmin={isSuperAdmin} />} />
        {/* 管理端同一份内容、同一个页面组件：版本/许可与登录角色无关（见 AppLayout 管理端菜单）。 */}
        <Route path="about" element={<AboutPage clientVersion={CLIENT_VERSION} />} />
      </Route>
    </Routes>
  )
}

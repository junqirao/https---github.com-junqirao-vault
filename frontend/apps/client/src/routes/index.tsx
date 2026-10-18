import { Navigate, Route, Routes, useLocation, useNavigate, useParams } from 'react-router-dom'

import {
  AboutPage,
  AuditList,
  ClientSettings,
  DiskImport,
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
  UploadTaskPage,
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

/** 详情页的来源（写进 `location.state.from`），决定「返回」回到哪一份存储库列表。 */
interface RepoDetailState {
  from?: string
}

function RepoDetailRoute(): JSX.Element {
  const { repoId } = useParams<{ repoId: string }>()
  const navigate = useNavigate()
  const location = useLocation()
  const { isSuperAdmin, user } = useAuth()
  if (!repoId) return <Navigate to="/my-repos" replace />
  // 详情是两条外壳**共用**的一个页面（/repos/:repoId 只挂在应用外壳下），所以"返回去哪"不能按
  // 角色猜：超管从用户端「我的存储库」点进来，角色上仍是超管，猜出来的管理端列表是错的。
  // 来源在跳转时写进 location.state（见下面各处的 onOpen），直接打开/刷新链接时没有来源，
  // 才退回按角色判断。
  const backTo = (location.state as RepoDetailState | null)?.from ?? (isSuperAdmin ? '/admin/repos' : '/my-repos')
  return (
    <RepoDetail
      repoId={repoId}
      isSuperAdmin={isSuperAdmin}
      currentUserId={user?.id ?? ''}
      currentUserName={user?.username ?? ''}
      // 详情的「返回」回到进来时的那份列表，不做 navigate(-1)：从新建页跳进详情时，
      // 历史里那张已经提交过的表单还在，后退会把用户送回去（表单已重置，看着像白填了）。
      onBack={() => navigate(backTo, { replace: true })}
    />
  )
}

/**
 * 上传任务页的来源（写进 `location.state.from`），决定「返回」回到哪。
 *
 * 上传进度只存在**发起它的那台机器**的代理内存里，所以入口目前只有应用端「我的存储库」；
 * 这里仍沿用详情页那套 from 约定，是为了将来从别处（比如任务中心）跳进来时不用再改。
 */
function UploadTaskRoute(): JSX.Element {
  const { uploadId } = useParams<{ uploadId: string }>()
  const navigate = useNavigate()
  const location = useLocation()
  if (!uploadId) return <Navigate to="/my-repos" replace />
  const backTo = (location.state as RepoDetailState | null)?.from ?? '/my-repos'
  return (
    <UploadTaskPage
      uploadId={uploadId}
      onBack={() => navigate(backTo, { replace: true })}
      // 完成后进详情：用 replace 把上传页从历史里顶掉，否则后退会回到那张已经结束的进度页。
      onOpenRepo={(repoId) => navigate(`/repos/${repoId}`, { replace: true, state: { from: backTo } })}
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
              // 详情「返回」靠这个来源回到应用端列表（见 RepoDetailRoute）。
              onOpen={(repoId) => navigate(`/repos/${repoId}`, { state: { from: '/my-repos' } })}
              // 上传中的库点进「上传任务」页：进度只在本机代理里，所以只有应用端有这个入口。
              onOpenUpload={(uploadId) =>
                navigate(`/uploads/${uploadId}`, { state: { from: '/my-repos' } })
              }
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
              onCreated={(repo) => navigate(`/repos/${repo.id}`, { replace: true, state: { from: '/my-repos' } })}
              onOpenRepo={(repoId) => navigate(`/repos/${repoId}`, { replace: true, state: { from: '/my-repos' } })}
              onCancel={() => navigate('/my-repos')}
            />
          }
        />
        <Route path="logs" element={<LogViewer />} />
        <Route path="repos/:repoId" element={<RepoDetailRoute />} />
        {/* 上传任务页独立于存储库详情：上传中的库在服务端可能还不存在（压根没有 repoId 可用）。 */}
        <Route path="uploads/:uploadId" element={<UploadTaskRoute />} />
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
              // 详情「返回」靠这个来源回到管理端列表（见 RepoDetailRoute）。
              onOpen={(repoId) => navigate(`/repos/${repoId}`, { state: { from: '/admin/repos' } })}
              // 迁移入口：把存储根里已有的 VHDX 原地接管成存储库（见 DiskImport）。
              onImport={() => navigate('/admin/repos/import')}
            />
          }
        />
        {/* 导入是独立一页而非弹窗：要选的项、要核对的容量、要看的失败明细都比较多，
            弹窗里做这些事只会让人反复取消重来。 */}
        <Route path="repos/import" element={<DiskImport onBack={() => navigate('/admin/repos')} />} />
        <Route
          path="repos/new"
          element={
            <RepoCreateForm
              isSuperAdmin
              // 同应用端：建库成功后新建页不再留在历史里，详情页返回只回存储库列表。
              onCreated={(repo) => navigate(`/repos/${repo.id}`, { replace: true, state: { from: '/admin/repos' } })}
              onOpenRepo={(repoId) => navigate(`/repos/${repoId}`, { replace: true, state: { from: '/admin/repos' } })}
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

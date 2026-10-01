import { RepoList } from './RepoList'

export interface MyReposProps {
  onCreate: () => void
  onOpen: (repoId: string) => void
}

/** 用户功能：我的存储库（仅本人拥有或可访问的库，由服务端过滤）。 */
export function MyRepos({ onCreate, onOpen }: MyReposProps): JSX.Element {
  return <RepoList isSuperAdmin={false} onCreate={onCreate} onOpen={onOpen} />
}

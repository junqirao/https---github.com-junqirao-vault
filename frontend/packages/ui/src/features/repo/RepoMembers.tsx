import { useMemo, useState } from 'react'
import { Button, Select, Space, Tooltip, Typography } from 'antd'
import type { TableColumnsType } from 'antd'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import { DataTable } from '../../components/DataTable'
import { ErrorNotice } from '../../components/ErrorNotice'
import { SectionCard } from '../../components/SectionCard'
import { useApi } from '../../api/provider'
import type { MemberDTO, Permission } from '../../api/types'
import { useI18n } from '../../i18n'
import { spacing } from '../../tokens/palette'
import { permissionLabel } from '../../utils/labels'

export interface RepoMembersProps {
  repoId: string
  isSuperAdmin: boolean
}

const PERMISSIONS: Permission[] = ['read', 'mount', 'manage']

/** 存储库可管理成员（覆盖式设置）。 */
export function RepoMembers({ repoId, isSuperAdmin }: RepoMembersProps): JSX.Element {
  const api = useApi()
  const { t } = useI18n()
  const queryClient = useQueryClient()
  const [selectedUser, setSelectedUser] = useState<string | undefined>(undefined)
  const [selectedPerm, setSelectedPerm] = useState<Permission>('read')
  const [error, setError] = useState<unknown>(null)

  const membersQuery = useQuery({
    queryKey: ['repo-members', repoId],
    queryFn: () => api.listMembers(repoId)
  })

  const usersQuery = useQuery({
    queryKey: ['users', 'options'],
    queryFn: () => api.listUsers({ limit: 200 }),
    enabled: isSuperAdmin
  })

  const userName = useMemo(() => {
    const map = new Map<string, string>()
    for (const user of usersQuery.data?.items ?? []) map.set(user.id, user.username)
    return map
  }, [usersQuery.data])

  const mutation = useMutation({
    mutationFn: (members: MemberDTO[]) => api.setMembers(repoId, members),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ['repo-members', repoId] })
    },
    onError: (err) => setError(err)
  })

  const members = membersQuery.data?.items ?? []

  const addMember = (): void => {
    if (!selectedUser) return
    setError(null)
    const next = members.filter((member) => member.user_id !== selectedUser)
    next.push({ user_id: selectedUser, perm: selectedPerm })
    mutation.mutate(next)
    setSelectedUser(undefined)
  }

  const removeMember = (userId: string): void => {
    setError(null)
    mutation.mutate(members.filter((member) => member.user_id !== userId))
  }

  const columns: TableColumnsType<MemberDTO> = [
    {
      title: t('field.user'),
      dataIndex: 'user_id',
      key: 'user_id',
      render: (value: string) => {
        const name = userName.get(value) ?? value
        return (
          <Tooltip title={name}>
            <Typography.Text ellipsis style={{ maxWidth: 240 }}>
              {name}
            </Typography.Text>
          </Tooltip>
        )
      }
    },
    {
      title: t('field.role'),
      dataIndex: 'perm',
      key: 'perm',
      width: 140,
      render: (value: string) => permissionLabel(value)
    },
    {
      title: t('common.actions'),
      key: 'actions',
      width: 100,
      render: (_value, member) => (
        <Button type="link" size="small" danger onClick={() => removeMember(member.user_id)}>
          {t('common.delete')}
        </Button>
      )
    }
  ]

  return (
    <SectionCard title={t('repo.detail.members')}>
      {error ? <ErrorNotice error={error} /> : null}
      <Space style={{ marginBottom: spacing.md }} wrap>
        <Select
          showSearch
          value={selectedUser}
          placeholder={t('field.user')}
          style={{ width: 240 }}
          loading={usersQuery.isLoading}
          onChange={setSelectedUser}
          optionFilterProp="label"
          options={(usersQuery.data?.items ?? [])
            .filter((user) => !members.some((member) => member.user_id === user.id))
            .map((user) => ({ value: user.id, label: user.username }))}
        />
        <Select<Permission>
          value={selectedPerm}
          style={{ width: 140 }}
          onChange={setSelectedPerm}
          options={PERMISSIONS.map((perm) => ({ value: perm, label: permissionLabel(perm) }))}
        />
        <Button type="primary" onClick={addMember} disabled={!selectedUser} loading={mutation.isPending}>
          {t('common.create')}
        </Button>
      </Space>
      <DataTable<MemberDTO>
        columns={columns}
        rows={members}
        rowKey={(member) => member.user_id}
        loading={membersQuery.isLoading}
        empty={t('common.empty')}
        pagination={false}
      />
    </SectionCard>
  )
}

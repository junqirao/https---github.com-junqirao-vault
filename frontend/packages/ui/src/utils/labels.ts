import { hasKey, t } from '../i18n'

/**
 * 枚举值 → 本地化文案。
 *
 * 采用 `state.<group>.<value>` 的 key 约定；缺失映射时回退为原始值
 * （保证新增枚举值时界面不出现空白，也不会显示 i18n key）。
 */
function label(group: string, value: string | undefined | null): string {
  if (!value) return '-'
  const key = `state.${group}.${value}`
  return hasKey(key) ? t(key) : value
}

export function repoStateLabel(value: string): string {
  return label('repo', value)
}

export function parentConditionLabel(value?: string): string {
  return label('parentCondition', value)
}

export function diskKindLabel(value: string): string {
  return label('diskKind', value)
}

export function diskStateLabel(value: string): string {
  return label('disk', value)
}

export function allocationStateLabel(value: string): string {
  return label('allocation', value)
}

export function leaseStateLabel(value: string): string {
  return label('lease', value)
}

export function jobStateLabel(value: string): string {
  return label('job', value)
}

export function jobTypeLabel(value: string): string {
  return label('jobType', value)
}

export function auditResultLabel(value: string): string {
  return label('audit', value)
}

export function roleLabel(value: string): string {
  const key = `role.${value}`
  return hasKey(key) ? t(key) : value
}

export function permissionLabel(value: string): string {
  const key = `perm.${value}`
  return hasKey(key) ? t(key) : value
}

export function repoModeLabel(value: string): string {
  const key = `repo.mode.${value}`
  return hasKey(key) ? t(key) : value
}

export function parentActionLabel(value: string): string {
  const key = `repo.parent.action.${value}`
  return hasKey(key) ? t(key) : value
}

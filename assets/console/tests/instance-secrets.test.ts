// 实例加密口径回归：状态标签与色调按契约六原因映射、未知状态不降级成就绪、
// 凭据汇总只在非零时列出已撤销 / 缺值、形状守卫拒绝非对象与半截计数。
// Instance-encryption wording regressions: labels/tone follow the contract, unknown
// states never read as ready, zero counts stay hidden, and the shape guard rejects
// non-object payloads and partial summaries.
import { describe, expect, it } from 'vitest'
import type { InstanceSecretsView } from '../src/api'
import {
  CREDENTIAL_SUMMARY_TITLE,
  INSTANCE_SECRETS_RECOVERY_BANNER,
  INSTANCE_SECRETS_TITLE,
  credentialSummaryText,
  credentialSummaryTone,
  instanceSecretsDetail,
  instanceSecretsLabel,
  instanceSecretsNeedsRecovery,
  instanceSecretsTone,
  isInstanceSecretsView,
  recoveryReasonText,
  shortKeyID,
} from '../src/instance-secrets'

function view(overrides: Partial<InstanceSecretsView> = {}): InstanceSecretsView {
  return {
    state: 'ready',
    key_id: 'k_7Q3M9X2P',
    initialized_at: '2026-10-20T08:00:00Z',
    key_file: '/var/lib/basalt/secrets/instance.key',
    recovery_reason: '',
    can_initialize: false,
    credential_summary: { managed: 3, managed_revoked: 0, env: 1, missing: 0 },
    ...overrides,
  }
}

describe('instance secrets wording', () => {
  it('keeps the fixed row titles and names the recovery guide in the banner', () => {
    expect(INSTANCE_SECRETS_TITLE).toBe('实例加密服务')
    expect(CREDENTIAL_SUMMARY_TITLE).toBe('当前连接凭据')
    // 横幅必须指向恢复文档，并同时禁止删除密钥文件与重新初始化两条错误动作。
    expect(INSTANCE_SECRETS_RECOVERY_BANNER).toContain('docs/Operations.md')
    expect(INSTANCE_SECRETS_RECOVERY_BANNER).toContain('不要删除密钥文件')
    expect(INSTANCE_SECRETS_RECOVERY_BANNER).toContain('不要重新初始化')
  })

  it('labels each state and shows only the key id prefix when ready', () => {
    expect(instanceSecretsLabel(view())).toBe('就绪（k_7Q3M…）')
    expect(instanceSecretsLabel(view({ state: 'ready', key_id: '' }))).toBe('就绪')
    expect(instanceSecretsLabel(view({ state: 'uninitialized', key_id: '' }))).toBe('未初始化')
    expect(
      instanceSecretsLabel(
        view({ state: 'recovery_required', key_id: '', recovery_reason: 'key_id_mismatch' }),
      ),
    ).toBe('恢复中（密钥与初始化记录不符）')
    // 契约之外的取值不得显示成可用。
    expect(
      instanceSecretsLabel({ ...view(), state: 'rotating' as InstanceSecretsView['state'] }),
    ).toBe('状态未知')
  })

  it('maps every contract recovery reason and never guesses an unknown one', () => {
    expect(recoveryReasonText('missing')).toBe('密钥文件不存在')
    expect(recoveryReasonText('corrupt')).toBe('密钥文件损坏或校验失败')
    expect(recoveryReasonText('key_id_mismatch')).toBe('密钥与初始化记录不符')
    expect(recoveryReasonText('unprovable_provenance')).toBe('存在无法证明来源的密钥文件')
    expect(recoveryReasonText('unavailable')).toBe('无法判定（读取失败）')
    expect(recoveryReasonText('key_path_missing')).toBe('主密钥路径缺失')
    expect(recoveryReasonText('something_new')).toBe('原因未知')
  })

  it('distinguishes an initialisable instance from an explicit pure-env deployment', () => {
    // 能就地初始化：首次保存自动完成，普通用户不该看到"请先初始化"。
    const legacy = view({ state: 'uninitialized', key_id: '', can_initialize: true })
    expect(instanceSecretsDetail(legacy)).toContain('首次保存加密托管凭据时会自动完成初始化')
    expect(instanceSecretsDetail(legacy)).not.toContain('请先初始化')
    // 显式纯 env：托管不可用，界面必须说清是部署方式而不是"还没做初始化"。
    const pureEnv = view({ state: 'uninitialized', key_id: '', can_initialize: false })
    expect(instanceSecretsDetail(pureEnv)).toContain('纯环境变量方式部署')
    expect(instanceSecretsDetail(pureEnv)).toContain('加密托管不可用')
    // 恢复态指向恢复文档，且不暗示重建密钥。
    const recovery = view({ state: 'recovery_required', recovery_reason: 'missing' })
    expect(instanceSecretsDetail(recovery)).toContain('docs/Operations.md')
    expect(instanceSecretsDetail(recovery)).not.toContain('重新初始化')
  })

  it('tones readiness green, uninitialized amber and unknown red', () => {
    expect(instanceSecretsTone(view())).toBe('ok')
    expect(instanceSecretsTone(view({ state: 'uninitialized' }))).toBe('warn')
    expect(instanceSecretsTone(view({ state: 'recovery_required' }))).toBe('bad')
    expect(
      instanceSecretsTone({ ...view(), state: 'rotating' as InstanceSecretsView['state'] }),
    ).toBe('bad')
    expect(instanceSecretsNeedsRecovery(view({ state: 'recovery_required' }))).toBe(true)
    expect(instanceSecretsNeedsRecovery(view())).toBe(false)
    expect(instanceSecretsNeedsRecovery(view({ state: 'uninitialized' }))).toBe(false)
  })

  it('shortens the key id to six characters without inventing an ellipsis', () => {
    expect(shortKeyID('k_7Q3M9X2P')).toBe('k_7Q3M…')
    expect(shortKeyID('abc')).toBe('abc…')
    expect(shortKeyID('')).toBe('')
  })

  it('summarises credentials and hides zero counts', () => {
    expect(credentialSummaryText({ managed: 3, managed_revoked: 0, env: 1, missing: 0 })).toBe(
      '3 个托管 / 1 个环境变量',
    )
    expect(credentialSummaryText({ managed: 0, managed_revoked: 2, env: 0, missing: 1 })).toBe(
      '2 个已撤销 / 1 个缺值',
    )
    expect(credentialSummaryText({ managed: 0, managed_revoked: 0, env: 0, missing: 0 })).toBe(
      '暂无连接凭据',
    )
  })

  it('tones the summary by the worst condition present', () => {
    expect(credentialSummaryTone({ managed: 1, managed_revoked: 0, env: 0, missing: 0 })).toBe('ok')
    expect(credentialSummaryTone({ managed: 0, managed_revoked: 1, env: 0, missing: 0 })).toBe(
      'warn',
    )
    expect(credentialSummaryTone({ managed: 1, managed_revoked: 0, env: 0, missing: 1 })).toBe('bad')
    expect(credentialSummaryTone({ managed: 0, managed_revoked: 0, env: 0, missing: 0 })).toBe(
      'muted',
    )
  })

  it('accepts only complete contract shapes', () => {
    expect(isInstanceSecretsView(view())).toBe(true)
    expect(isInstanceSecretsView([])).toBe(false)
    expect(isInstanceSecretsView(null)).toBe(false)
    expect(isInstanceSecretsView('ready')).toBe(false)
    expect(isInstanceSecretsView({ ...view(), state: 'rotating' })).toBe(false)
    expect(isInstanceSecretsView({ ...view(), credential_summary: {} })).toBe(false)
    expect(
      isInstanceSecretsView({
        ...view(),
        credential_summary: { managed: 1, managed_revoked: 0, env: 0, missing: '0' },
      }),
    ).toBe(false)
  })
})

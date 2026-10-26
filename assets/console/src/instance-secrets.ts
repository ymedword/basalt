// 实例加密服务的统一口径：总览卡片、全站恢复横幅与模型连接表单共用这里的文案与
// 判定，避免三处各写一套说法。状态与原因取值以 docs/contracts/openapi.yaml 的
// InstanceSecretsView 为准；未知状态一律按「需要处理」显示，**绝不**降级成"就绪"
// ——把读不懂的响应说成安全，比不显示更糟。
// Shared wording for the instance encryption service. The overview card, the global
// recovery banner and the model-connection form all read from this module so labels
// cannot drift. Unknown states degrade to "needs attention", never to "ready".
import type { InstanceSecretsView } from './api'
import type { Tone } from './components/ui'

/** 管理台里这条服务行的固定名字（设计 §2.4.4 / §2.6.5）。 */
// The fixed row title used by the overview card and the model-connection form.
export const INSTANCE_SECRETS_TITLE = '实例加密服务'

/** 第二行：连接自身凭据汇总的固定名字。 */
// The fixed title of the credential-summary row.
export const CREDENTIAL_SUMMARY_TITLE = '当前连接凭据'

/**
 * `recovery_required` 时顶栏下方的常驻横幅（设计 §2.6.1 / §2.6.5）。
 * 它必须点名恢复指引所在文档，并明确"不要删除密钥文件、不要重新初始化"——
 * 这两条是恢复路径上代价最高的误操作。
 * The permanent banner shown while the instance key needs recovery; it names the
 * recovery guide and forbids the two costliest missteps.
 */
export const INSTANCE_SECRETS_RECOVERY_BANNER =
  '实例主密钥需要恢复：托管凭据当前无法解密，相关调用会被拒绝。' +
  '请按 docs/Operations.md 的主密钥恢复步骤处置；不要删除密钥文件，也不要重新初始化。'

/** 恢复原因的固定文案（契约 InstanceSecretsReason 七值）。 */
// Fixed Chinese text for each recovery reason; the wire values stay unchanged.
const RECOVERY_REASON_LABELS: Readonly<Record<string, string>> = {
  missing: '密钥文件不存在',
  corrupt: '密钥文件损坏或校验失败',
  key_id_mismatch: '密钥与初始化记录不符',
  unprovable_provenance: '存在无法证明来源的密钥文件',
  unavailable: '无法判定（读取失败）',
  key_path_missing: '主密钥路径缺失',
}

/** 恢复原因 → 一句中文；未知原因不猜具体成因。 */
// Maps a recovery reason to one sentence; unknown reasons are not guessed.
export function recoveryReasonText(reason: string): string {
  return RECOVERY_REASON_LABELS[reason] ?? '原因未知'
}

/** key_id 的前缀展示：只取前 6 位再加省略号，完整值仍在详情里。 */
// Shows the first six characters of the key id; the full value stays in details.
export function shortKeyID(keyID: string): string {
  if (keyID === '') {
    return ''
  }
  return `${keyID.slice(0, 6)}…`
}

/** 实例加密服务的单行状态：就绪（含 key_id 前缀）/ 未初始化 / 恢复中（原因）。 */
// One-line state label; the key id prefix is shown only when the service is ready.
export function instanceSecretsLabel(view: InstanceSecretsView): string {
  switch (view.state) {
    case 'ready': {
      const suffix = shortKeyID(view.key_id)
      return suffix === '' ? '就绪' : `就绪（${suffix}）`
    }
    case 'uninitialized':
      return '未初始化'
    case 'recovery_required':
      return `恢复中（${recoveryReasonText(view.recovery_reason)}）`
    default:
      return '状态未知'
  }
}

/** 状态色调：就绪绿、未初始化黄、恢复中红、未知红（不静默当作可用）。 */
// Tone for the state badge; an unknown state is red rather than silently usable.
export function instanceSecretsTone(view: InstanceSecretsView): Tone {
  switch (view.state) {
    case 'ready':
      return 'ok'
    case 'uninitialized':
      return 'warn'
    default:
      return 'bad'
  }
}

/** 服务是否处于恢复态：全站横幅与表单置灰共用这一条判断。 */
// True only for the explicit recovery state; used by the banner and the form.
export function instanceSecretsNeedsRecovery(view: InstanceSecretsView): boolean {
  return view.state === 'recovery_required'
}

/**
 * 状态的一句话说明：总览卡片与模型连接表单共用，避免两处各写一套。
 * `uninitialized` 必须区分「能就地初始化」（首次保存自动完成）与「显式纯 env」
 * ——两者对用户的含义完全不同，不能都写成"请先初始化"。
 * One-sentence explanation shared by the overview card and the connection form.
 */
export function instanceSecretsDetail(view: InstanceSecretsView): string {
  switch (view.state) {
    case 'ready':
      return '主密钥已就绪；托管凭据可加密保存。'
    case 'uninitialized':
      return view.can_initialize
        ? '尚未初始化；首次保存加密托管凭据时会自动完成初始化。'
        : '此实例以纯环境变量方式部署（secrets.auto_init: false）；加密托管不可用。'
    case 'recovery_required':
      return '托管凭据当前无法解密；按 docs/Operations.md 恢复主密钥。'
    default:
      return '状态未知；以实例接口返回为准。'
  }
}

/**
 * 凭据汇总一行：`N 个托管 / N 个环境变量`，已撤销与缺值只在非零时追加——
 * 把零值也列出来会让正常实例看起来像有问题。
 * One summary line; revoked and missing counts appear only when non-zero.
 */
export function credentialSummaryText(summary: InstanceSecretsView['credential_summary']): string {
  const parts: string[] = []
  if (summary.managed > 0) {
    parts.push(`${summary.managed} 个托管`)
  }
  if (summary.env > 0) {
    parts.push(`${summary.env} 个环境变量`)
  }
  if (summary.managed_revoked > 0) {
    parts.push(`${summary.managed_revoked} 个已撤销`)
  }
  if (summary.missing > 0) {
    parts.push(`${summary.missing} 个缺值`)
  }
  return parts.length === 0 ? '暂无连接凭据' : parts.join(' / ')
}

/** 汇总色调：缺值红、已撤销黄、有可用值绿、完全没有灰色。 */
// Summary tone: missing is bad, revoked is a warning, empty is muted.
export function credentialSummaryTone(summary: InstanceSecretsView['credential_summary']): Tone {
  if (summary.missing > 0) {
    return 'bad'
  }
  if (summary.managed_revoked > 0) {
    return 'warn'
  }
  if (summary.managed + summary.env > 0) {
    return 'ok'
  }
  return 'muted'
}

/**
 * 形状守卫：接口没返回对象时（代理错误页、测试桩、旧服务端）按「未知」处理，
 * 不做字段读取。只校验状态与四个计数——其余字段缺省时页面按空串展示。
 * Shape guard for non-object payloads so unknown responses render as "unknown".
 */
export function isInstanceSecretsView(value: unknown): value is InstanceSecretsView {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) {
    return false
  }
  const record = value as Record<string, unknown>
  if (
    record.state !== 'uninitialized' &&
    record.state !== 'ready' &&
    record.state !== 'recovery_required'
  ) {
    return false
  }
  const summary = record.credential_summary
  if (typeof summary !== 'object' || summary === null || Array.isArray(summary)) {
    return false
  }
  const counts = summary as Record<string, unknown>
  return ['managed', 'managed_revoked', 'env', 'missing'].every(
    (key) => typeof counts[key] === 'number',
  )
}

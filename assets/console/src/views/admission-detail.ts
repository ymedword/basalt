// 学习准入详情（US-21 §3.1.12 / T022）：候选概要、来源声明、冻结清单、七维评估、
// 本机决策与发布 / 撤回记录。三条口径：
//
//  1. **只读**：批准 / 发布 / 撤回只在本机 CLI 执行；本页不渲染任何按钮控件。
//  2. **`not_evaluable` 不等于通过**：单维判定逐条标注，聚合只做如实转写。
//  3. **子记录缺失是"尚未发生"**：未评估 / 未决策 / 未发布各显示空态，不伪造字段；
//     来源自由备注不整段出站（服务端已置空），页面不渲染。
//
// Read-only admission detail: source declaration, frozen manifest, the seven-dimension
// evaluation, the local decision and the publication record. Missing sub-records are
// explicit empty states; not_evaluable is never rendered as a pass.
import { computed, defineComponent, h } from 'vue'
import type { VNode, VNodeChild } from 'vue'
import type {
  AdmissionDecisionView,
  AdmissionDetailView as AdmissionDetailData,
  AdmissionDimensionName,
  AdmissionDimensionStatus,
  AdmissionEvaluationView,
  AdmissionPublicationView,
  AdmissionSourceView,
} from '../api'
import { DataTable, Notice, formatTimestamp, link, shortHash } from '../components/ui'
import type { Tone } from '../components/ui'
import { admissionsHref } from '../router'
import { ADMISSION_CLI_ONLY_NOTICE, ADMISSION_RISK_LABELS, ADMISSION_STATE_LABELS } from './admissions'
import { badge, definitions, identifier, pageHeading, useLoader, viewProps } from './shared'

/** 七个固定维度文案；键名逐字等于契约取值。 */
// The seven contract dimensions, labelled; keys are the contract literals.
export const ADMISSION_DIMENSION_LABELS: Readonly<Record<AdmissionDimensionName, string>> = {
  content: '内容自洽',
  source: '来源可用',
  permissions: '权限面',
  safety: '安全扫描',
  regression: '回归影响',
  effect: '效果评测',
  cost: '成本测量',
}

/** 单维判定文案：不可评估必须写明「不等于通过」（C-02）。 */
// Dimension verdict labels; not_evaluable is spelled out, never shown as a pass.
export const ADMISSION_DIMENSION_STATUS_LABELS: Readonly<
  Record<AdmissionDimensionStatus, { readonly label: string; readonly tone: Tone }>
> = {
  passed: { label: '通过', tone: 'ok' },
  failed: { label: '失败', tone: 'bad' },
  not_evaluable: { label: '不可评估（不等于通过）', tone: 'warn' },
}

/** 聚合结论文案；incomplete 的批准需要显式确认（确认集合精确匹配）。 */
const AGGREGATE_LABELS: Readonly<Record<AdmissionEvaluationView['aggregate'], string>> = {
  passed: '七维全部通过',
  failed: '存在失败维度——批准会被拒绝',
  incomplete: '不完整：有维度不可评估，批准需显式确认',
}

const DECISION_LABELS: Readonly<Record<AdmissionDecisionView['decision'], string>> = {
  approve: '批准',
  reject: '拒绝',
}

const PUBLICATION_STATE_LABELS: Readonly<Record<AdmissionPublicationView['state'], string>> = {
  active: '发布有效',
  withdrawn: '已撤回',
}

/** 撤回分类文案；`withdraw_reason` 存的就是这三个契约字面量。 */
// Withdrawal classes; the stored withdraw_reason is one of these three literals.
const WITHDRAW_CLASS_LABELS: Readonly<Record<string, string>> = {
  restored_previous: '已把上一版内容作为新版本重新发布',
  superseded: '指针已被后续发布超越——只记录撤回，不回退指针',
  deleted: '技能已删除——只记录撤回',
}

/** 字符串列表：空集合显示破折号，不渲染空列表。 */
function listOrDash(items: readonly string[]): VNodeChild {
  return items.length === 0
    ? '—'
    : h('ul', { class: 'plain-list' }, items.map((item, index) => h('li', { key: index }, item)))
}

/** 概要：候选行 + 派生展示态说明。 */
function summarySection(detail: AdmissionDetailData): VNode {
  const state = ADMISSION_STATE_LABELS[detail.state]
  return h('section', { class: 'card' }, [
    h('h2', '候选概要'),
    definitions([
      ['候选', identifier(detail.candidate_id)],
      ['技能名', detail.skill_name],
      ['状态', badge(state.label, state.tone)],
      ['风险档', ADMISSION_RISK_LABELS[detail.risk_tier]],
      ['提出者主体', identifier(detail.proposer_subject)],
      ['候选哈希', h('code', { title: detail.candidate_hash }, shortHash(detail.candidate_hash))],
      ['来源哈希', h('code', { title: detail.source_hash }, shortHash(detail.source_hash))],
      ['创建时间', formatTimestamp(detail.created_at)],
      ['更新时间', formatTimestamp(detail.updated_at)],
    ]),
    detail.state === 'expired'
      ? h(Notice, {
        tone: 'warn',
        text: '批准已过期（派生展示态）：库内状态仍是 approved。发布会被本机 CLI 拒绝，需要重新评估并批准。',
      })
      : null,
  ])
}

/** 来源声明：结构化字段逐项展示；自由备注不出站。 */
function sourceSection(source: AdmissionSourceView): VNode {
  return h('section', { class: 'card' }, [
    h('h2', '来源声明'),
    definitions([
      ['来源种类', source.kind],
      ['来源引用', h('code', source.ref)],
      ['任务结果', source.task_outcome],
      ['确认方式', source.confirmation],
      ['授权声明', source.authorization],
      ['脱敏声明', source.redaction],
      ['保留期限', source.retention_until],
      ['适用性', source.applicable ? '适用' : '不适用'],
    ]),
    h('h3', '证据'),
    listOrDash(source.evidence),
    h('h3', '局限'),
    listOrDash(source.limitations),
    h('h3', '反例'),
    listOrDash(source.counterexamples),
    h('p', { class: 'muted small' }, '自由备注不整段出站：页面只展示上面的结构化声明。'),
  ])
}

/** 冻结清单：只有路径 / 哈希 / 大小，没有正文。 */
function filesSection(detail: AdmissionDetailData): VNode {
  return h('section', { class: 'card' }, [
    h('h2', '冻结清单'),
    h(DataTable, {
      tableClass: 'admission-files-table',
      headers: ['路径', 'SHA-256', '大小（字节）'],
      rows: detail.files.map((file) => ({
        key: file.path,
        cells: [h('code', file.path), h('code', { title: file.sha256 }, shortHash(file.sha256)), String(file.size)],
      })),
      empty: '候选清单为空',
    }),
  ])
}

/** 确定性评估：七维判定逐条展示，聚合如实转写。 */
function evaluationSection(evaluation: AdmissionEvaluationView | null): VNode {
  if (evaluation === null) {
    return h('section', { class: 'card' }, [
      h('h2', '确定性评估'),
      h(Notice, { tone: 'info', text: '尚未评估：候选还没有可绑定的评估行。' }),
    ])
  }
  return h('section', { class: 'card' }, [
    h('h2', '确定性评估'),
    definitions([
      ['聚合结论', AGGREGATE_LABELS[evaluation.aggregate]],
      ['风险档', ADMISSION_RISK_LABELS[evaluation.risk_tier]],
      ['评分器版本', evaluation.grader_version],
      ['策略哈希', h('code', { title: evaluation.policy_hash }, shortHash(evaluation.policy_hash))],
      ['候选哈希', h('code', { title: evaluation.candidate_hash }, shortHash(evaluation.candidate_hash))],
      ['评估哈希', h('code', { title: evaluation.evaluation_hash }, shortHash(evaluation.evaluation_hash))],
      ['评估时间', formatTimestamp(evaluation.evaluated_at)],
      ['候选内脚本', listOrDash(evaluation.scripts)],
      ['既有引用者', listOrDash(evaluation.referrers)],
    ]),
    h(DataTable, {
      tableClass: 'admission-dimensions-table',
      headers: ['维度', '判定', '理由'],
      rows: evaluation.dimensions.map((dimension) => ({
        key: dimension.name,
        cells: [
          ADMISSION_DIMENSION_LABELS[dimension.name] ?? dimension.name,
          badge(
            ADMISSION_DIMENSION_STATUS_LABELS[dimension.status].label,
            ADMISSION_DIMENSION_STATUS_LABELS[dimension.status].tone,
          ),
          dimension.reason === undefined || dimension.reason === '' ? '—' : dimension.reason,
        ],
      })),
      empty: '评估没有维度行',
    }),
  ])
}

/** 本机决策：批准者、有效期与理由（写入前已做上限与凭证扫描）。 */
function decisionSection(decision: AdmissionDecisionView | null): VNode {
  if (decision === null) {
    return h('section', { class: 'card' }, [
      h('h2', '本机决策'),
      h(Notice, { tone: 'info', text: '尚未决策：批准 / 拒绝只在本机 CLI 生成。' }),
    ])
  }
  const acknowledged = decision.acknowledged.map(
    (name) => ADMISSION_DIMENSION_LABELS[name as AdmissionDimensionName] ?? name,
  )
  return h('section', { class: 'card' }, [
    h('h2', '本机决策'),
    definitions([
      ['决策', DECISION_LABELS[decision.decision]],
      ['批准者主体', identifier(decision.approver_subject)],
      ['风险档', ADMISSION_RISK_LABELS[decision.risk_tier]],
      ['确认维度', listOrDash(acknowledged)],
      ['有效期至', formatTimestamp(decision.expires_at)],
      ['被超越时间', formatTimestamp(decision.superseded_at)],
      ['决策时间', formatTimestamp(decision.created_at)],
      ['理由', decision.reason === '' ? '—' : decision.reason],
    ]),
    h('p', { class: 'muted small' },
      '批准有效期上限 24 小时；到期后候选按派生态显示「批准已过期」，发布会被拒绝。'),
  ])
}

/** 发布记录：版本、内容哈希与撤回分类；撤回不删除证据。 */
function publicationSection(publication: AdmissionPublicationView | null): VNode {
  if (publication === null) {
    return h('section', { class: 'card' }, [
      h('h2', '发布记录'),
      h(Notice, { tone: 'info', text: '尚未发布：发布只在本机 CLI 执行。' }),
    ])
  }
  const withdrawn = publication.state === 'withdrawn'
  const withdrawClass = WITHDRAW_CLASS_LABELS[publication.withdraw_reason] ?? publication.withdraw_reason
  const content: VNodeChild[] = [
    definitions([
      ['技能名', publication.skill_name],
      ['版本', `v${publication.version_no}（版本 ID ${publication.version_id}）`],
      ['内容哈希', h('code', { title: publication.content_hash }, shortHash(publication.content_hash))],
      ['发布状态', PUBLICATION_STATE_LABELS[publication.state]],
      ['发布时间', formatTimestamp(publication.published_at)],
    ]),
  ]
  if (withdrawn) {
    content.push(
      definitions([
        ['撤回时间', formatTimestamp(publication.withdrawn_at)],
        ['撤回分类', withdrawClass === '' ? '—' : withdrawClass],
        ['撤回后指针版本', publication.withdraw_version_id > 0
          ? `已切换到版本 ID ${publication.withdraw_version_id}`
          : '未移动（指针已被超越或技能已删除）'],
      ]),
      h('p', { class: 'muted small' }, '撤回不删除已发布版本、候选目录或决策记录；页面不提供回滚或清理入口。'),
    )
  }
  return h('section', { class: 'card' }, [h('h2', '发布记录'), ...content])
}

/** 详情页：按 `candidate_id` 加载；无写控件。 */
// The detail page loads by candidate id and renders no write control.
export const AdmissionDetailView = defineComponent({
  name: 'AdmissionDetailView',
  props: viewProps,
  setup(props) {
    const api = props.ctx.api
    const candidateId = computed(() => props.params.id ?? '')
    const detail = useLoader(() => api.getAdmission(candidateId.value))

    return (): VNode => {
      const content: VNodeChild[] = [
        h('p', {}, [link(admissionsHref(), '← 返回学习准入列表')]),
        h(Notice, {
          tone: 'info',
          text: `${ADMISSION_CLI_ONLY_NOTICE}。本页只读：不提供批准 / 发布 / 撤回入口。`,
        }),
      ]
      if (candidateId.value === '') {
        content.push(h(Notice, { tone: 'error', text: '缺少候选标识：请从列表进入详情。' }))
      } else if (detail.error.value !== '') {
        content.push(h(Notice, { tone: 'error', text: detail.error.value }))
      } else if (detail.data.value === null) {
        content.push(h('p', { class: 'muted' }, '加载中…'))
      } else {
        const view = detail.data.value
        content.push(
          summarySection(view),
          sourceSection(view.source),
          filesSection(view),
          evaluationSection(view.evaluation),
          decisionSection(view.decision),
          publicationSection(view.publication),
        )
      }
      return h('section', { class: 'view view-admission-detail' }, [
        pageHeading('RESOURCE / 16', '准入候选详情', '候选的冻结证据、确定性评估与本机决策。'),
        ...content,
      ])
    }
  },
})

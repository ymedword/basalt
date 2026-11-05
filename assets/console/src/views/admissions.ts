// 学习准入列表（US-21 §3.1.12 / T022）：只读查看候选、派生展示态与风险档。
// 三条口径：
//
//  1. **只读**：批准 / 发布 / 撤回只在本机 CLI 执行；本页不渲染任何按钮控件，
//     过滤条件变更直接重载列表（C-14）。
//  2. **expired 是派生展示态**：批准过了有效期时页面显示「批准已过期」，数据库里
//     仍是 `approved`——发布会被 CLI 拒绝，页面不改写状态。
//  3. **列表只有数据库行**：没有清单摘要与文件正文；详情才展示冻结清单与评估。
//
// The read-only admission list. Approve / publish / withdraw stay on the local CLI;
// this page renders no button at all. The expired state is derived for display only
// and the list carries no manifest digests or file bodies.
import { defineComponent, h, ref, watch } from 'vue'
import type { VNode, VNodeChild } from 'vue'
import type { AdmissionListView, AdmissionRiskTier, AdmissionState } from '../api'
import { DataTable, Notice, formatTimestamp, shortHash } from '../components/ui'
import type { Tone } from '../components/ui'
import { admissionHref } from '../router'
import { badge, identifier, pageHeading, useLoader, viewProps } from './shared'

/** 页面顶部固定提示（颗粒度文档 §3.1.12 逐字）。 */
// The fixed notice literal from the module card.
export const ADMISSION_CLI_ONLY_NOTICE = '批准 / 发布 / 撤回只在本机 CLI 执行'

/** 状态标签与色调；`expired` 是派生展示态（库中仍是 approved）。 */
// State labels; expired is derived and never written back.
export const ADMISSION_STATE_LABELS: Readonly<
  Record<AdmissionState, { readonly label: string; readonly tone: Tone }>
> = {
  proposed: { label: '已提议', tone: 'muted' },
  evaluated: { label: '已评估', tone: 'warn' },
  approved: { label: '已批准', tone: 'ok' },
  rejected: { label: '已拒绝', tone: 'bad' },
  canceled: { label: '已取消', tone: 'muted' },
  published: { label: '已发布', tone: 'ok' },
  withdrawn: { label: '已撤回', tone: 'warn' },
  expired: { label: '批准已过期（派生）', tone: 'bad' },
}

/** 风险档文案；`unknown` 只出现在尚未评估的候选上。 */
// Risk-tier labels; `unknown` only appears before the first evaluation.
export const ADMISSION_RISK_LABELS: Readonly<Record<AdmissionRiskTier, string>> = {
  unknown: '未知（尚未评估）',
  normal: '常规',
  high: '高风险',
}

/** 过滤下拉的八个状态（含派生 expired，与契约枚举逐字一致）。 */
const STATES: readonly AdmissionState[] = [
  'proposed',
  'evaluated',
  'approved',
  'rejected',
  'canceled',
  'published',
  'withdrawn',
  'expired',
]

function normalizeState(raw: string): AdmissionState | '' {
  return (STATES as readonly string[]).includes(raw) ? (raw as AdmissionState) : ''
}

/** 列表视图：候选行直链详情；过滤即查询，没有提交按钮。 */
// The list view: rows deep-link to the detail; filters reload without a submit button.
export const AdmissionsView = defineComponent({
  name: 'AdmissionsView',
  props: viewProps,
  setup(props) {
    const api = props.ctx.api
    const state = ref<AdmissionState | ''>('')
    const skill = ref('')
    const admissions = useLoader(() =>
      api.getAdmissions({
        state: state.value === '' ? undefined : state.value,
        skill: skill.value.trim() === '' ? undefined : skill.value.trim(),
      }),
    )
    // 只读页不渲染按钮：条件一变化就重载；useLoader 已按代际丢弃过期响应。
    // No button here: a filter change reloads, and stale responses are dropped.
    watch([state, skill], () => {
      void admissions.reload()
    })

    return (): VNode => {
      const content: VNodeChild[] = [
        h(Notice, {
          tone: 'info',
          text: `${ADMISSION_CLI_ONLY_NOTICE}。本页只读：数据来自数据库行，不读取候选目录，也不提供任何写操作。`,
        }),
        h('div', { class: 'filters' }, [
          h('label', ['状态', h(
            'select',
            {
              id: 'admissions-state',
              value: state.value,
              onChange: (event: Event) => {
                state.value = normalizeState((event.target as HTMLSelectElement).value)
              },
            },
            [
              h('option', { value: '' }, '全部'),
              ...STATES.map((item) =>
                h('option', { key: item, value: item }, ADMISSION_STATE_LABELS[item].label),
              ),
            ],
          )]),
          h('label', ['技能名', h('input', {
            id: 'admissions-skill',
            type: 'text',
            value: skill.value,
            placeholder: '按技能名精确过滤',
            onChange: (event: Event) => {
              skill.value = (event.target as HTMLInputElement).value
            },
          })]),
        ]),
        h('p', { class: 'muted small' },
          '「批准已过期」是派生展示态：库内状态仍是 approved，发布时由本机 CLI 拒绝；页面不提供延期或重新批准。'),
      ]
      if (admissions.error.value !== '') {
        content.push(h(Notice, { tone: 'error', text: admissions.error.value }))
      }
      if (admissions.loading.value && admissions.data.value === null) {
        content.push(h('p', { class: 'muted' }, '加载中…'))
      }
      const data = admissions.data.value
      if (data !== null) {
        content.push(
          h(DataTable, {
            tableClass: 'admissions-table',
            headers: ['候选', '技能', '状态', '风险档', '提出者', '内容哈希', '创建时间'],
            rows: data.map((row: AdmissionListView) => ({
              key: row.candidate_id,
              cells: [
                identifier(row.candidate_id, admissionHref(row.candidate_id)),
                row.skill_name,
                badge(ADMISSION_STATE_LABELS[row.state].label, ADMISSION_STATE_LABELS[row.state].tone),
                ADMISSION_RISK_LABELS[row.risk_tier],
                identifier(row.proposer_subject),
                h('code', { class: 'cell-hash', title: row.candidate_hash }, shortHash(row.candidate_hash)),
                formatTimestamp(row.created_at),
              ],
            })),
            empty: '没有符合条件的准入候选',
          }),
        )
      }
      return h('section', { class: 'view view-admissions' }, [
        pageHeading('RESOURCE / 16', '学习准入', '查看技能候选的确定性评估、本机决策与发布记录。'),
        ...content,
      ])
    }
  },
})

// 调用方详情（US-16 §3.1.9 / T026a）：概要、周期用量与限额、预留核对、Key 签发
// 与吊销。三条契约事实决定交互形态，页面上如实说明、不绕过：
//
//  1. 没有 `GET /callers/{id}`——概要从列表里按 `caller_id` 取，取不到就是"不在列表"；
//  2. 没有 Key 列表端点——只展示本次签发返回的一次性明文；吊销按手填 `key_id`；
//  3. 没有待核对预留列表——`pending_reservations` 只给计数，核对按手填 `reservation_id`。
//
// Key 明文只存在于本视图状态：不写 URL、不写存储、不进日志；操作者确认抄录后清空。
// No single-caller endpoint, no key list and no reservation list exist in the
// contract, so the page works from the issuance response and manual ids. The
// plaintext key lives only in view state and is dropped once acknowledged.
import { computed, defineComponent, h, ref } from 'vue'
import type { VNode, VNodeChild } from 'vue'
import { errorMessage } from '../api'
import { CallersApi } from '../api-callers'
import type {
  CallerKeySummary,
  CallerLimitPeriod,
  CallerLimitView,
  CallerReconcileOutcome,
  CallerSummary,
  CallerUsageLine,
  IssuedKey,
  ReservationView,
} from '../api-callers'
import { CodeBlock, ConfirmDialog, DataTable, Notice, formatTimestamp } from '../components/ui'
import type { Tone } from '../components/ui'
import { callersHref } from '../router'
import { CALLER_KIND_LABELS, CALLER_STATUS_LABELS } from './callers'
import { badge, definitions, identifier, pageHeading, useLoader, viewProps } from './shared'

/** 六行用量类别文案；未知类别回退到原始字面量（不吞掉服务端数据）。 */
const USAGE_KIND_LABELS: Readonly<Record<string, string>> = {
  run: '运行次数',
  chat: '对话 tokens',
  compaction: '压缩 tokens',
  rerank: '重排',
  embedding_tokens: '向量化 tokens',
  ingest_bytes: '摄取字节',
}

const PERIOD_LABELS: Readonly<Record<CallerLimitPeriod, string>> = {
  day: '按日',
  month: '按月',
}

const KEY_STATUS_LABELS: Readonly<Record<string, { readonly label: string; readonly tone: Tone }>> = {
  active: { label: '有效', tone: 'ok' },
  revoked: { label: '已吊销', tone: 'bad' },
  expired: { label: '已过期', tone: 'muted' },
}

const RESERVATION_CLASS_LABELS: Readonly<Record<string, string>> = {
  run: '运行',
  chat: '对话',
  compaction: '压缩',
  ingest: '摄取',
  rerank: '重排',
}

const RESERVATION_STATUS_LABELS: Readonly<Record<string, { readonly label: string; readonly tone: Tone }>> = {
  pending: { label: '待核对', tone: 'warn' },
  committed: { label: '已提交', tone: 'ok' },
  released: { label: '已释放', tone: 'muted' },
  reconciled: { label: '已核对', tone: 'ok' },
}

/** 常规 scope 复选框：两个管理面 scope 不在其中（签发给 Key 会被 400 拒绝）。 */
const SCOPE_CHOICES = ['sessions:read', 'sessions:write', 'knowledge:read', 'knowledge:write', 'flows:read', 'flows:write'] as const

/** 把 `agents:invoke` 输入行归一成完整 scope：接受 Agent 名或完整字面量。 */
function normalizeAgentScope(line: string): string {
  const trimmed = line.trim()
  if (trimmed === '' || trimmed.startsWith('agents:invoke:')) {
    return trimmed
  }
  return `agents:invoke:${trimmed}`
}

/** 解析可选的 RFC3339 时刻：空串 = 不填（omit），非法返回 null。 */
function optionalInstant(raw: string): string | null {
  const trimmed = raw.trim()
  if (trimmed === '') {
    return ''
  }
  const parsed = new Date(trimmed)
  return Number.isNaN(parsed.getTime()) ? null : parsed.toISOString()
}

/** 非负整数解析：空串返回 null（调用方区分"没填"与"填了 0"）。 */
function requiredCount(raw: string): number | null {
  const trimmed = raw.trim()
  if (trimmed === '') {
    return null
  }
  const parsed = Number(trimmed)
  return Number.isInteger(parsed) && parsed >= 0 ? parsed : null
}

function usageLabel(kind: string): string {
  return USAGE_KIND_LABELS[kind] ?? kind
}

function keyStatusBadge(status: string): VNode {
  const info = KEY_STATUS_LABELS[status] ?? { label: status, tone: 'muted' as Tone }
  return badge(info.label, info.tone)
}

function reservationStatusBadge(status: string): VNode {
  const info = RESERVATION_STATUS_LABELS[status] ?? { label: status, tone: 'muted' as Tone }
  return badge(info.label, info.tone)
}

export const CallerDetailView = defineComponent({
  name: 'CallerDetailView',
  props: viewProps,
  setup(props) {
    const api = new CallersApi(props.ctx.api)
    const callerId = props.params.id ?? ''
    const callers = useLoader(() => api.list())
    const caller = computed<CallerSummary | null>(
      () => callers.data.value?.find((item) => item.caller_id === callerId) ?? null,
    )

    // --- 周期用量 ---------------------------------------------------------

    const periodInput = ref('')
    const periodQuery = ref('')
    const periodError = ref('')
    const usage = useLoader(() => api.usage(callerId, periodQuery.value))

    function readPeriod(): void {
      const instant = optionalInstant(periodInput.value)
      if (instant === null) {
        periodError.value = '周期时刻无法解析：请用日期时间输入框选择。'
        return
      }
      periodError.value = ''
      periodQuery.value = instant
      void usage.reload()
    }

    function backToCurrentPeriod(): void {
      periodError.value = ''
      periodInput.value = ''
      periodQuery.value = ''
      void usage.reload()
    }

    const overageLines = computed<CallerUsageLine[]>(
      () => usage.data.value?.usage.filter((line) => line.overage > 0) ?? [],
    )

    // --- 核对并清零 -------------------------------------------------------

    const clearReason = ref('')
    const clearBusy = ref(false)
    const clearError = ref('')
    const clearNotice = ref('')

    async function clearOverage(event: Event): Promise<void> {
      event.preventDefault()
      clearError.value = ''
      clearNotice.value = ''
      const reason = clearReason.value.trim()
      if (reason === '') {
        clearError.value = '清零理由必填：它会写入管理审计。'
        return
      }
      clearBusy.value = true
      try {
        const body = periodQuery.value === ''
          ? { reason }
          : { reason, period_start: periodQuery.value }
        await api.clearOverage(callerId, body)
        clearReason.value = ''
        clearNotice.value = '已清零该周期 overage（理由已写入管理审计）；used / reserved 未改动。'
        await usage.reload()
      } catch (caught) {
        clearError.value = errorMessage(caught)
      } finally {
        clearBusy.value = false
      }
    }

    // --- 核对预留 ---------------------------------------------------------

    const reservationId = ref('')
    const outcome = ref<CallerReconcileOutcome>('committed')
    const actualInput = ref('')
    const reconcileReason = ref('')
    const reconcileBusy = ref(false)
    const reconcileError = ref('')
    const reconciled = ref<ReservationView | null>(null)

    async function reconcile(event: Event): Promise<void> {
      event.preventDefault()
      reconcileError.value = ''
      reconciled.value = null
      const id = reservationId.value.trim()
      if (id === '') {
        reconcileError.value = '预留标识必填（契约没有待核对预留列表，需从审计或日志中取得）。'
        return
      }
      const reason = reconcileReason.value.trim()
      if (reason === '') {
        reconcileError.value = '核对说明必填：它会写入管理审计。'
        return
      }
      const body: { outcome: CallerReconcileOutcome; actual?: number; reason: string } = {
        outcome: outcome.value,
        reason,
      }
      if (outcome.value === 'committed') {
        const actual = requiredCount(actualInput.value)
        if (actual === null) {
          reconcileError.value = 'committed 必须给出实际用量（非负整数，0 也算给了）。'
          return
        }
        body.actual = actual
      }
      reconcileBusy.value = true
      try {
        reconciled.value = await api.reconcile(callerId, id, body)
        reservationId.value = ''
        actualInput.value = ''
        reconcileReason.value = ''
        await usage.reload()
      } catch (caught) {
        reconcileError.value = errorMessage(caught)
      } finally {
        reconcileBusy.value = false
      }
    }

    // --- 签发 Key ---------------------------------------------------------

    const scopeChecked = ref<Record<string, boolean>>({})
    const agentScopes = ref('')
    const expiresInput = ref('')
    const keyBusy = ref(false)
    const keyError = ref('')
    const issued = ref<IssuedKey | null>(null)

    function toggledScope(scope: string, checked: boolean): void {
      scopeChecked.value = { ...scopeChecked.value, [scope]: checked }
    }

    function selectedScopes(): string[] {
      const scopes: string[] = []
      for (const scope of SCOPE_CHOICES) {
        if (scopeChecked.value[scope] === true) {
          scopes.push(scope)
        }
      }
      for (const line of agentScopes.value.split('\n')) {
        const normalized = normalizeAgentScope(line)
        if (normalized !== '') {
          scopes.push(normalized)
        }
      }
      return [...new Set(scopes)]
    }

    async function issueKey(event: Event): Promise<void> {
      event.preventDefault()
      keyError.value = ''
      issued.value = null
      const scopes = selectedScopes()
      if (scopes.length === 0) {
        keyError.value = '至少选择一个 scope。'
        return
      }
      const expires = optionalInstant(expiresInput.value)
      if (expires === null) {
        keyError.value = '过期时刻无法解析：请用日期时间输入框选择。'
        return
      }
      keyBusy.value = true
      try {
        const body = expires === '' ? { scopes } : { scopes, expires_at: expires }
        issued.value = await api.issueKey(callerId, body)
        await callers.reload()
      } catch (caught) {
        keyError.value = errorMessage(caught)
      } finally {
        keyBusy.value = false
      }
    }

    function dismissIssued(): void {
      issued.value = null
    }

    // --- 吊销 Key ---------------------------------------------------------

    const revokeKeyIdInput = ref('')
    const revokeReason = ref('')
    const revokeBusy = ref(false)
    const revokeConfirmOpen = ref(false)
    const revokeError = ref('')
    const revoked = ref<CallerKeySummary | null>(null)

    function askRevoke(event: Event): void {
      event.preventDefault()
      revokeError.value = ''
      if (revokeKeyIdInput.value.trim() === '') {
        revokeError.value = 'Key 标识必填。'
        return
      }
      revokeConfirmOpen.value = true
    }

    async function confirmRevoke(): Promise<void> {
      revokeBusy.value = true
      revokeError.value = ''
      try {
        revoked.value = await api.revokeKey(
          callerId,
          revokeKeyIdInput.value.trim(),
          revokeReason.value,
        )
        revokeConfirmOpen.value = false
        revokeKeyIdInput.value = ''
        revokeReason.value = ''
        await callers.reload()
      } catch (caught) {
        revokeConfirmOpen.value = false
        revokeError.value = errorMessage(caught)
      } finally {
        revokeBusy.value = false
      }
    }

    // --- 渲染 -------------------------------------------------------------

    function summaryCard(item: CallerSummary): VNode {
      const status = CALLER_STATUS_LABELS[item.status]
      return h('section', { class: 'card caller-summary' }, [
        h('h2', {}, '概要'),
        definitions([
          ['调用方', identifier(item.caller_id)],
          ['名称', item.name],
          ['项目', item.project === '' ? '—' : item.project],
          ['种类', CALLER_KIND_LABELS[item.kind]],
          ['状态', badge(status.label, status.tone)],
          ['未吊销 Key', String(item.key_count)],
          ['创建时间', formatTimestamp(item.created_at)],
        ]),
        h('p', { class: 'muted small' }, '契约没有单个调用方的读取端点：本页按列表中的该行展示，列表变化后重新读取即可。'),
      ])
    }

    function usageCard(): VNode {
      const children: VNodeChild[] = [
        h('h2', {}, '周期用量与限额'),
        h('form', { class: 'usage-period-form', onSubmit: (event: Event) => { event.preventDefault(); readPeriod() } }, [
          h('label', { class: 'field', for: 'usage-period' }, [
            h('span', {}, '周期内任一时刻（留空 = 当前周期）'),
            h('input', {
              id: 'usage-period',
              type: 'datetime-local',
              value: periodInput.value,
              onInput: (event: Event) => {
                periodInput.value = (event.target as HTMLInputElement).value
              },
            }),
          ]),
          h('div', { class: 'form-actions' }, [
            h('button', { id: 'usage-read', type: 'submit' }, '读取'),
            h('button', {
              id: 'usage-now',
              type: 'button',
              disabled: periodInput.value === '' && periodQuery.value === '',
              onClick: backToCurrentPeriod,
            }, '回到当前周期'),
          ]),
        ]),
      ]
      if (periodError.value !== '') {
        children.push(h(Notice, { tone: 'error', text: periodError.value }))
      }
      if (usage.error.value !== '') {
        children.push(h(Notice, { tone: 'error', text: usage.error.value }))
      }
      if (usage.loading.value && usage.data.value === null) {
        children.push(h('p', { class: 'muted' }, '加载中…'))
      }
      const data = usage.data.value
      if (data === null) {
        return h('section', { class: 'card caller-usage' }, children)
      }
      children.push(
        definitions([
          ['周期开始', formatTimestamp(data.period_start)],
          ['周期结束', formatTimestamp(data.period_end)],
          ['时区', data.timezone],
        ]),
      )
      children.push(
        h(DataTable, {
          tableClass: 'usage-table',
          columnClasses: ['col-name', 'col-number', 'col-number', 'col-number'],
          headers: ['类别', '已用', '已预留', '超限'],
          rows: data.usage.map((line) => ({
            key: line.kind,
            cells: [usageLabel(line.kind), String(line.used), String(line.reserved), String(line.overage)],
          })),
          empty: '该周期没有用量行',
        }),
      )
      children.push(h('h3', {}, '当前限额'))
      children.push(limitDefinitions(data.limits))
      // 这一行的数不都是"上限"：QPS / 突发是**覆盖值**，生效值还要与实例入口预算取小；
      // 并发与周期额度的 0 各有别的含义。写清楚，用户才不会以为填了就一定生效。
      children.push(
        h(
          'p',
          { class: 'muted small' },
          'QPS / 突发是调用方覆盖值：生效值 = min(实例入口预算 callers.ingress.*, 这里)，' +
            '0 表示不覆盖（沿用实例预算）；实例预算本身没配时这里的覆盖救不回来，请求会被拒。' +
            '并发 Run 名额 0 = 用实例上限；周期额度 0 = 该维度拒绝。' +
            '限额只在创建时写入，控制台没有修改入口。',
        ),
      )
      if (data.pending_reservations > 0) {
        children.push(
          h(Notice, {
            tone: 'warn',
            text: `有 ${data.pending_reservations} 条结局未知的预留仍占用额度，不会自动释放；请在下方「核对预留」收敛。`,
          }),
        )
      }
      if (overageLines.value.length > 0) {
        const kinds = overageLines.value.map((line) => usageLabel(line.kind)).join('、')
        children.push(
          h(Notice, {
            tone: 'warn',
            text: `本周期已发生超限（${kinds}）：新预留一律 429 QUOTA_EXHAUSTED，直到核对清零。超限不会回滚已发生的调用。`,
          }),
          h('form', { class: 'clear-overage-form', onSubmit: (event: Event) => { void clearOverage(event) } }, [
            h('label', { class: 'field', for: 'clear-overage-reason' }, [
              h('span', {}, '清零理由（必填，写入管理审计）'),
              h('input', {
                id: 'clear-overage-reason',
                type: 'text',
                value: clearReason.value,
                placeholder: '例如 已与调用方确认超限原因并补录账单',
                onInput: (event: Event) => {
                  clearReason.value = (event.target as HTMLInputElement).value
                },
              }),
            ]),
            h('div', { class: 'form-actions' }, [
              h('button', { id: 'clear-overage', type: 'submit', class: 'primary', disabled: clearBusy.value }, clearBusy.value ? '清零中…' : '核对并清零'),
            ]),
          ]),
        )
      }
      if (clearError.value !== '') {
        children.push(h(Notice, { tone: 'error', text: clearError.value }))
      }
      if (clearNotice.value !== '') {
        children.push(h(Notice, { tone: 'info', text: clearNotice.value }))
      }
      return h('section', { class: 'card caller-usage' }, children)
    }

    function limitDefinitions(limit: CallerLimitView): VNode {
      return definitions([
        ['每秒请求（QPS）', String(limit.qps)],
        ['突发请求数', String(limit.burst)],
        ['并发 Run 名额', String(limit.max_concurrent)],
        ['每周期任务数', String(limit.tasks_per_period)],
        ['每周期 tokens', String(limit.tokens_per_period)],
        ['每周期摄取字节', String(limit.ingest_bytes_per_period)],
        ['周期', PERIOD_LABELS[limit.period] ?? limit.period],
        ['周期时区', limit.timezone],
      ])
    }

    function reservationCard(): VNode {
      const pending = usage.data.value?.pending_reservations ?? 0
      const children: VNodeChild[] = [
        h('h2', {}, '核对预留'),
        h(
          'p',
          { class: 'muted small' },
          `待核对预留：${pending} 条。契约没有待核对预留列表端点，需从审计或日志取得预留标识；` +
            '核对只接受 pending：二次结算由服务端按 409 CONFLICT 拒绝。',
        ),
        h('form', { class: 'reconcile-form', onSubmit: (event: Event) => { void reconcile(event) } }, [
          h('label', { class: 'field', for: 'reservation-id' }, [
            h('span', {}, '预留标识（必填）'),
            h('input', {
              id: 'reservation-id',
              type: 'text',
              value: reservationId.value,
              placeholder: 'resv_…',
              spellcheck: false,
              onInput: (event: Event) => {
                reservationId.value = (event.target as HTMLInputElement).value
              },
            }),
          ]),
          h('label', { class: 'field', for: 'reservation-outcome' }, [
            h('span', {}, '结局'),
            h('select', {
              id: 'reservation-outcome',
              value: outcome.value,
              onChange: (event: Event) => {
                outcome.value = (event.target as HTMLSelectElement).value as CallerReconcileOutcome
              },
            }, [
              h('option', { value: 'committed' }, 'committed（按实际用量记账）'),
              h('option', { value: 'released' }, 'released（确认零消耗）'),
            ]),
          ]),
          h('label', { class: 'field', for: 'reservation-actual' }, [
            h('span', {}, '实际用量（committed 必填；大于预留量记超限）'),
            h('input', {
              id: 'reservation-actual',
              type: 'number',
              min: 0,
              step: 1,
              value: actualInput.value,
              onInput: (event: Event) => {
                actualInput.value = (event.target as HTMLInputElement).value
              },
            }),
          ]),
          h('label', { class: 'field', for: 'reconcile-reason' }, [
            h('span', {}, '核对说明（必填，写入管理审计）'),
            h('input', {
              id: 'reconcile-reason',
              type: 'text',
              value: reconcileReason.value,
              onInput: (event: Event) => {
                reconcileReason.value = (event.target as HTMLInputElement).value
              },
            }),
          ]),
          h('div', { class: 'form-actions' }, [
            h('button', { id: 'reconcile-submit', type: 'submit', class: 'primary', disabled: reconcileBusy.value }, reconcileBusy.value ? '核对中…' : '核对预留'),
          ]),
        ]),
      ]
      if (reconcileError.value !== '') {
        children.push(h(Notice, { tone: 'error', text: reconcileError.value }))
      }
      const result = reconciled.value
      if (result !== null) {
        children.push(
          h('h3', {}, '核对结果'),
          definitions([
            ['预留', identifier(result.reservation_id)],
            ['类别', RESERVATION_CLASS_LABELS[result.class] ?? result.class],
            ['周期开始', formatTimestamp(result.period_start)],
            ['预留量', String(result.amount)],
            ['实际用量', result.actual === undefined ? '—' : String(result.actual)],
            ['超限', String(result.overage)],
            ['状态', reservationStatusBadge(result.status)],
          ]),
        )
      }
      return h('section', { class: 'card caller-reservations' }, children)
    }

    function issuedBlock(): VNode | null {
      const value = issued.value
      if (value === null) {
        return null
      }
      return h('div', { class: 'issued-key' }, [
        h(Notice, {
          tone: 'warn',
          text: '这是 Key 的明文，只显示这一次：离开或刷新后无法再取回，丢失只能吊销重发。请立即交给接入方并妥善保管。',
        }),
        h(CodeBlock, { text: value.key }),
        definitions([
          ['key_id', identifier(value.key_id)],
          ['前缀', value.key_prefix],
          ['scopes', value.scopes.join('、')],
          ['创建时间', formatTimestamp(value.created_at)],
          ['过期时间', formatTimestamp(value.expires_at)],
        ]),
        h('div', { class: 'form-actions' }, [
          h('button', {
            id: 'use-issued-key-id',
            type: 'button',
            onClick: () => {
              revokeKeyIdInput.value = value.key_id
            },
          }, '用这个 key_id 填写吊销表单'),
          h('button', { id: 'dismiss-issued-key', type: 'button', onClick: dismissIssued }, '我已抄录，隐藏明文'),
        ]),
      ])
    }

    function keysCard(): VNode {
      const children: VNodeChild[] = [
        h('h2', {}, 'Key'),
        h('h3', {}, '签发 Key'),
        h('p', { class: 'muted small' }, '明文只在签发响应出现一次；服务端只存指纹与前缀。管理面 scope（diagnostics:read / callers:admin）不得签发给 Key。'),
        h('form', { class: 'issue-key-form', onSubmit: (event: Event) => { void issueKey(event) } }, [
          h('fieldset', { class: 'form-section' }, [
            h('legend', {}, 'scopes（至少一项）'),
            ...SCOPE_CHOICES.map((scope) => h('label', {
              key: scope, class: 'check-row', for: `scope-${scope.replaceAll(':', '-')}`,
            }, [
              h('input', {
                id: `scope-${scope.replaceAll(':', '-')}`,
                type: 'checkbox',
                checked: scopeChecked.value[scope] === true,
                onChange: (event: Event) => {
                  toggledScope(scope, (event.target as HTMLInputElement).checked)
                },
              }),
              h('span', {}, scope),
            ])),
            h('label', { class: 'field', for: 'agent-scopes' }, [
              h('span', {}, 'Agent 调用 scope（每行一个 Agent 名，或完整 agents:invoke:<name>）'),
              h('textarea', {
                id: 'agent-scopes',
                rows: 3,
                value: agentScopes.value,
                placeholder: 'daily-report',
                onInput: (event: Event) => {
                  agentScopes.value = (event.target as HTMLTextAreaElement).value
                },
              }),
            ]),
            h('p', { class: 'field-hint muted' }, '`agents:invoke:*` 只允许管理员发放；其余未知 scope 会被服务端按 400 INVALID_SCOPE 拒绝，且零写入。'),
          ]),
          h('label', { class: 'field', for: 'key-expires' }, [
            h('span', {}, '过期时间（留空 = 不过期）'),
            h('input', {
              id: 'key-expires',
              type: 'datetime-local',
              value: expiresInput.value,
              onInput: (event: Event) => {
                expiresInput.value = (event.target as HTMLInputElement).value
              },
            }),
          ]),
          h('div', { class: 'form-actions' }, [
            h('button', { id: 'issue-key', type: 'submit', class: 'primary', disabled: keyBusy.value }, keyBusy.value ? '签发中…' : '签发 Key'),
          ]),
        ]),
      ]
      if (keyError.value !== '') {
        children.push(h(Notice, { tone: 'error', text: keyError.value }))
      }
      const block = issuedBlock()
      if (block !== null) {
        children.push(block)
      }
      children.push(h('h3', {}, '吊销 Key'))
      children.push(
        h('p', { class: 'muted small' }, '契约没有 Key 列表端点：请填写签发时得到的 key_id。吊销不改动定时任务的授权 Key，受影响任务在下次触发时按 owner_unauthorized 失败；重复吊销幂等。'),
        h('form', { class: 'revoke-key-form', onSubmit: askRevoke }, [
          h('label', { class: 'field', for: 'revoke-key-id' }, [
            h('span', {}, 'key_id（必填）'),
            h('input', {
              id: 'revoke-key-id',
              type: 'text',
              value: revokeKeyIdInput.value,
              placeholder: 'key_…',
              spellcheck: false,
              onInput: (event: Event) => {
                revokeKeyIdInput.value = (event.target as HTMLInputElement).value
              },
            }),
          ]),
          h('label', { class: 'field', for: 'revoke-reason' }, [
            h('span', {}, '理由（可选，写入管理审计；不得含凭据）'),
            h('input', {
              id: 'revoke-reason',
              type: 'text',
              value: revokeReason.value,
              onInput: (event: Event) => {
                revokeReason.value = (event.target as HTMLInputElement).value
              },
            }),
          ]),
          h('div', { class: 'form-actions' }, [
            h('button', { id: 'revoke-key', type: 'submit' }, '吊销 Key'),
          ]),
        ]),
      )
      if (revokeError.value !== '') {
        children.push(h(Notice, { tone: 'error', text: revokeError.value }))
      }
      const result = revoked.value
      if (result !== null) {
        children.push(
          h('h3', {}, '吊销结果'),
          definitions([
            ['key_id', identifier(result.key_id)],
            ['前缀', result.key_prefix],
            ['scopes', result.scopes.join('、')],
            ['状态', keyStatusBadge(result.status)],
            ['吊销时间', formatTimestamp(result.revoked_at)],
          ]),
        )
      }
      return h('section', { class: 'card caller-keys' }, children)
    }

    return (): VNode => {
      const listError = callers.error.value
      const content: VNodeChild[] = []
      if (listError !== '') {
        content.push(h(Notice, { tone: 'error', text: listError }))
      }
      if (callers.loading.value && callers.data.value === null) {
        content.push(h('p', { class: 'muted' }, '加载中…'))
      }
      if (callers.data.value !== null && caller.value === null) {
        content.push(
          h(Notice, { tone: 'error', text: `调用方 ${callerId} 不在列表中：可能已删除或标识有误。` }),
          h('p', {}, ['返回：', h('a', { href: callersHref() }, '调用方列表')]),
        )
      }
      const item = caller.value
      if (item !== null) {
        content.push(
          summaryCard(item),
          usageCard(),
          reservationCard(),
          keysCard(),
          h(ConfirmDialog, {
            open: revokeConfirmOpen.value,
            title: '吊销 Key',
            message: `吊销 Key ${revokeKeyIdInput.value.trim()} 后该 Key 立即失效，且没有恢复入口；需要继续接入只能重新签发。继续？`,
            confirmText: revokeBusy.value ? '吊销中…' : '吊销 Key',
            busy: revokeBusy.value,
            onConfirm: () => {
              void confirmRevoke()
            },
            onCancel: () => {
              if (!revokeBusy.value) {
                revokeConfirmOpen.value = false
              }
            },
          }),
        )
      }
      const title = item === null ? '调用方详情' : item.name
      return h('section', { class: 'view view-caller-detail' }, [
        pageHeading('SYSTEM / CALLERS / DETAIL', title, `调用方：${callerId}`, h('a', { href: callersHref() }, '返回列表')),
        ...content,
      ])
    }
  },
})

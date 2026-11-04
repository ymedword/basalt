// US-16A只读视图：展示事实与证据范围，不推断因果、不触发业务。
// Read-only views present facts and evidence bounds without inferred causality or business execution.
import { defineComponent, h, onMounted, onUnmounted, ref, watch } from 'vue'
import type { VNode, VNodeChild } from 'vue'
import { ApiError } from '../api'
import { ObservabilityApi } from '../api-observability'
import type { ObsAlerts, ObsCandidate, ObsCleanup, ObsContext, ObsEvidenceGap, ObsExport, ObsOverview, ObsQuery, ObsWindow } from '../api-observability'
import { DataTable, Notice, StatusBadge, formatTimestamp } from '../components/ui'
import { observabilityHref } from '../router'
import { diagnosticTimeline, metricDashboard, observationReason, overviewCards, readableNumber } from '../observation-presentation'
import type { TimelineMode } from '../observation-presentation'
import { historyBounds, initialHistoryRange, MAX_HISTORY_MS } from '../observation-range'
import type { HistoryPreset } from '../observation-range'
import { pageHeading, section, useLoader, viewProps } from './shared'
import type { LoaderState } from './shared'
const TABS = [
  [
    'overview', '总览'
  ], [
    'groups', '异常任务'
  ], [
    'run', '运行诊断'
  ], [
    'query', '关联查询'
  ], [
    'metrics', '指标'
  ], [
    'alerts', '告警'
  ], [
    'export', '导出'
  ], [
    'cleanup', '清理'
  ]
] as const
const ANCHORS = [
  [
    'run_id', '运行 ID'
  ], [
    'request_id', '请求 ID'
  ], [
    'task_id', '调度任务 ID'
  ], [
    'flow_task_id', '流程任务 ID'
  ], [
    'operation_id', '管理操作 ID'
  ], [
    'ingest_job_id', '摄取任务 ID'
  ], [
    'session_id', '会话 ID'
  ]
] as const
const SOURCES: Record<string, string> = {
  runs: '运行', task_executions: '调度执行', management_operations: '管理操作', kb_ingest_jobs: '知识摄取', embedding_calls: '向量调用', llm_calls: '模型调用', tool_invocations: '工具调用', context_assembly_records: '上下文装配', diagnostic_events: '诊断事件'
}
const RULES: Record<string, string> = {
  audit_write_failed: '审计写入失败', diagnostic_write_failed: '诊断写入失败', export_permanent_failure: '导出永久失败', export_backlog_stuck: '导出积压', cleanup_failed: '清理失败', run_failure_burst: '任务失败集中出现', llm_timeout_burst: '模型超时集中出现', tool_denial_burst: '工具权限拒绝集中出现'
}
const ALERT_REASONS: Record<string, string> = {
  state_store_unavailable: '状态存储不可用，请查服务日志', source_unavailable: '评估来源不可用', process_restart_gap: '本进程读数；重启前证据不可用', recovery_pending: '恢复条件仍在持续观察', not_evaluated: '尚未评估', disabled: '未启用'
}
const TABLES: Record<string, string> = {
  diagnostic_events: '诊断事件', context_assembly_records: '上下文装配证据', alert_history: '告警转换历史', export_history: '导出历史'
}
const STATES: Record<string, string> = {
  running: '执行中', pending: '待处理', started: '执行中', completed: '已完成', success: '已完成', succeeded: '已完成', failed: '失败', cancelled: '已取消', skipped: '已跳过', unknown: '未知', inactive: '未触发', firing: '告警中', resolved: '已恢复', limit_reached: '达到执行限制'
}
function number(v: number | null | undefined): string { return v == null ? '未记录' : String(v) }
function time(v: string | null | undefined): string { return v ? formatTimestamp(v) : '未记录' }
function reason(v: string): string { return observationReason(v) }
function notice(text: string, tone: 'info' | 'warn' | 'error' = 'info'): VNode {
  return h(Notice, {
    text, tone
  })
}
function pairs(values: readonly (readonly [
  string,
  VNodeChild
])[]): VNode {
  return h('dl', {
    class: 'definitions'
  }, values.flatMap(([k, v]) => [
    h('dt', k), h('dd', [v])
  ]))
}
function technical(values: readonly (readonly [
  string,
  VNodeChild
])[]): VNode {
  return h('details', {
    class: 'technical-details'
  }, [
    h('summary', '技术详情'), pairs(values)
  ])
}
function table(headers: string[], rows: {
  key: string
  cells: VNodeChild[]
}[], empty: string): VNode {
  return h(DataTable, {
    headers, rows, empty
  })
}
function readState<T>(s: LoaderState<T>, render: (v: T) => VNodeChild): VNodeChild {
  if (s.error.value)
    return notice(s.error.value, 'error')
  if (s.loading.value)
    return notice('正在读取…')
  return s.data.value === null ? null : render(s.data.value)
}
function gaps(values: ObsEvidenceGap[]): VNodeChild[] {
  const labels: Record<string, string> = {
    query_failed: '读取失败', sampled_out: '可能被采样省略', not_recorded: '未记录', not_supported: '该来源尚不支持', retention_window_cleaned: '该时间窗发生过清理，具体缺失原因无法确认', unknown: '证据状态未知'
  }
  return values.map(g => notice(`${SOURCES[g.source] ?? '来源'}：${labels[g.reason] ?? '证据状态未知'}${g.reason === 'retention_window_cleaned' ? '。' + g.detail : ''}`, 'warn'))
}
function status(v: string): VNode {
  return h(StatusBadge, {
    label: STATES[v] ?? '未知状态', tone: v === 'failed' || v === 'firing' ? 'bad' : v === 'unknown' || v === 'pending' ? 'warn' : 'muted'
  })
}
function groupRows(o: ObsOverview): VNodeChild {
  const groups = [...o.groups, ...(o.other_group ? [o.other_group] : [])]
  return section('异常任务组', [
    h('p', {class: 'muted small'}, '选择异常任务 → 打开失败运行 → 查看终止原因和异常步骤。'),
    o.merged_groups ? notice('其他：合并了 ' + o.merged_groups + ' 个组') : null,
    table(['Agent', '失败 / 执行', '主要原因', '定位运行'], groups.map(g => ({key: g.agent, cells: [
      h('strong', g.agent || '未记录 Agent'), String(g.failed) + ' / ' + g.total,
      g.top_reasons.map(r => reason(r.reason) + ' ' + r.count + ' 次').join('；') || '主要原因未记录',
      h('div', [h('div', {class: 'form-actions'}, g.run_ids.map((id, i) => h('a', {href: observabilityHref('run', ['run_id', id]), title: id}, '查看运行 ' + (i + 1)))),
        g.total > g.run_ids.length ? h('p', {class: 'muted small'}, '仅显示 ' + g.run_ids.length + ' 条可用异常 Run；关联列表有界，不代表组内全部执行。') : null,
        technical([['时间窗', time(g.window_start) + ' — ' + time(g.window_end)], ['Run ID', g.run_ids.join(' / ')]])])
    ]})), '没有满足条件的异常组'),
    o.truncated ? notice('仅展示有界记录，统计可能不完整。', 'warn') : null
  ])
}
function candidates(values: ObsCandidate[], hits: string[]): VNodeChild {
  const decisions: Record<string, string> = {
    kept: '保留', rank_cut: '排名截断', budget_drop: '预算裁剪', not_recorded: '未记录'
  }
  return table([
    '候选', '决定', '命中列表', '技术详情'
  ], values.map((c, i) => ({
    key: c.chunk_id, cells: [
      `候选 ${i + 1}`, decisions[c.decision] ?? '未记录', hits.includes(c.chunk_id) ? '已列入' : '未列入', technical([
        [
          '片段 ID', c.chunk_id
        ], [
          '版本 ID', c.version_id
        ], [
          '内容哈希', c.content_hash
        ], [
          '估算 token', number(c.token_estimate)
        ], [
          '检索分', number(c.retrieval_score)
        ], [
          '重排分', number(c.rerank_score)
        ], [
          '名次', number(c.rank)
        ]
      ])
    ]
  })), '没有记录可展示的候选')
}
function renderContext(v: ObsContext): VNodeChild {
  const r = v.retrieval
  const compression = (value: string) => ({none: '未压缩', compressed: '已压缩', degraded: '已降级', rejected: '已拒绝'} as Record<string, string>)[value] ?? '未记录'
  return section('上下文证据', [
    ...gaps(v.gaps), h('p', {class: 'muted small'}, '装配证据只描述实际记录的内容；是否被模型使用，需核对输出。'),
    r ? section('知识检索', [
      r.evidence.assembly === 'direct' ? h('p', r.hits.length === 0 ? '知识库无结果；本次检索装配证据已记录。' : '检索候选 ' + r.candidates.length + ' 个，列入命中列表 ' + r.hits.length + ' 个。') : notice('检索命中未记录；检索装配证据缺失，当前仅能展示已记录的诊断计数。', 'warn'),
      h('p', {class: 'muted small'}, '证据：' + (r.evidence.assembly === 'direct' ? '直接记录' : '证据不足') + '；模型是否采用：需核对输出。'),
      h('p', {class: 'muted small'}, '检索内容受当前访问范围限制；筛选统计和候选列表可展开查看。'),
      h('details', {class: 'obs-details'}, [h('summary', '检索候选与参数（' + r.candidates.length + ' 个候选）'),
        pairs([['混合过滤计数', number(r.filtered_count)], ['过滤原因', '原因未细分']]),
        h('p', {class: 'muted small'}, '集合级与检索路径的混合计数，单位未统一；不代表被过滤文档数或当前查询命中数；未记录逐项身份。'),
        r.candidates.length ? candidates(r.candidates, r.hits) : h('p', {class: 'muted'}, '没有记录可展示的候选'),
        pairs([['Top-K', number(r.top_k)], ['候选上限', number(r.candidate_k)], ['检索预算 token', number(r.budget_tokens)], ['索引代际', number(r.index_generation)]])])
    ]) : notice('该 Run 没有检索装配记录', 'warn'),
    v.rounds.length ? h('div', {class: 'obs-context-rounds'}, [h('h3', '对话上下文 · ' + v.rounds.length + ' 轮装配记录'),
      table(['轮次', '装配段', '上下文预算', '历史处理', '会话压缩', '证据 / 详情'], v.rounds.map((round, i) => ({key: round.step_id + ':' + i, cells: [
        '对话装配 ' + (i + 1), String(round.final_segments.length), round.budget ? readableNumber(round.budget.context_tokens) + ' tokens' : '本轮预算未记录',
        round.history_trim ? '历史裁剪 ' + round.history_trim.dropped_groups + ' 组；' + reason(round.history_trim.reason_code) : '历史裁剪未记录',
        round.compression ? compression(round.compression.decision) + (round.compression.reason_code ? '；' + reason(round.compression.reason_code) : '') : '会话压缩未记录',
        h('div', [h('span', {class: 'badge badge-muted'}, round.evidence.assembly === 'direct' ? '装配：直接记录' : '装配：证据不足'), h('p', {class: 'muted small'}, '模型使用：需核对输出'), h('details', {class: 'obs-details'}, [h('summary', '本轮装配详情'),
          h('p', round.final_segments.length ? '已装配，使用需核对输出' : '有装配记录，但段信息未记录；模型使用需核对输出'),
          h('p', '已记录装配段 ' + round.final_segments.length + ' 个'),
          round.budget === null ? h('p', '本轮预算未记录') : pairs([['上下文预算', number(round.budget.context_tokens)], ['协议预留', number(round.budget.protocol_reserve)], ['输出预留', number(round.budget.output_reserve)]]),
          round.candidates.length ? candidates(round.candidates, round.hits) : null,
          technical([['装配步骤 ID', round.step_id], ['估算器版本', round.budget?.estimator_version ?? '未记录'], ['装配段 ID', round.final_segments.join(' / ')], ['历史切点', round.history_trim?.cut_message_id ?? '未记录'], ['压缩链 ID', round.compression?.compaction_id ?? '未记录']])])])
      ]})), '该 Run 没有对话装配记录')]) : notice('该 Run 没有对话装配记录', 'warn'),
    v.other.length ? h('details', {class: 'obs-details'}, [h('summary', '其他装配证据（' + v.other.length + ' 条）'), table(['用途', '原因', '技术详情'], v.other.map(a => ({key: a.record_id, cells: [a.purpose, a.reason_codes.map(reason).join('；') || '未记录', technical([['记录 ID', a.record_id], ['步骤 ID', a.step_id]])]})), '没有其他装配记录')]) : null
  ])
}
function renderAlerts(v: ObsAlerts): VNodeChild {
  return h('div', [
    notice('告警不发送外部通知；需要打开管理台查看。本进程规则在重启前证据不可用。'), v.enabled ? null : notice('告警未启用', 'warn'), table([
      '规则', '状态', '当前读数 / 观察窗口', '说明', '技术详情'
    ], v.rules.flatMap(r => r.states.map((s, i) => ({
      key: `${r.rule_id}:${i}`, cells: [
        RULES[r.rule_id] ?? '内置规则', r.enabled ? status(s.state) : '未启用', !r.enabled || s.state === 'unknown' || s.reason === 'not_evaluated' ? '未确认' : readableNumber(s.value) + ' · 最近 ' + readableNumber(r.window_seconds / 60) + ' 分钟', ALERT_REASONS[s.reason] ?? (s.reason ? '原因未细分' : '已完成评估'), technical([
          [
            '规则 ID', r.rule_id
          ], [
            '作用域', s.scope_key
          ], [
            '窗口（秒）', r.window_seconds
          ], [
            '持续时间（秒）', r.for_seconds
          ], [
            '开始时间', time(s.since)
          ], [
            '最后评估', time(s.last_eval_at)
          ], [
            '本次读数', !r.enabled || s.state === 'unknown' || s.reason === 'not_evaluated' ? '未确认' : number(s.value)
          ]
        ])
      ]
    }))), '没有启用的规则或尚无规则记录'), h('p', h('a', {
      href: observabilityHref('groups')
    }, '查看异常任务组')), section('状态转换历史', [
      table([
        '时间', '规则', '原状态', '新状态'
      ], v.transitions.map(t => ({
        key: String(t.id), cells: [
          time(t.occurred_at), RULES[t.rule_id] ?? '内置规则', status(t.from_state), status(t.to_state)
        ]
      })), '该时间窗没有告警转换记录'), v.truncated ? notice('仅显示最近的有界转换历史。', 'warn') : null
    ])
  ])
}
function renderExportDetails(v: ObsExport): VNodeChild {
  const unit = ({
    span: '跨度', log_record: '日志条', data_point: '数据点'
  } as Record<string, string>)[v.last_record_unit] ?? '未记录'
  return h('div', [
    v.enabled ? notice('导出已启用；接收副本不替代本地权威审计。') : notice('导出未启用', 'warn'), pairs([
      [
        '目标', v.endpoint_configured ? '已配置' : '未配置'
      ], [
        '请求凭据', v.headers_configured ? '已配置' : '未配置'
      ]
    ]), table([
      '条目状态', '数量'
    ], [
      [
        '等待发送', v.pending
      ], [
        '处理中', v.inflight
      ], [
        '等待重试', v.retry
      ], [
        '逐项未确认', v.unconfirmed
      ], [
        '永久失败', v.failed_permanent
      ], [
        '丢弃', v.dropped
      ], [
        '过期', v.stale
      ]
    ].map(([label, count]) => ({
      key: String(label), cells: [
        label, number(count as number)
      ]
    })), '未记录导出条目'), technical([
      ['协议', v.protocol],
      ['HTTP 响应码', v.last_http_status == null ? '未记录' : number(v.last_http_status)],
      ['当前待重试项再次认领次数（合计）', v.retry_attempts == null ? '无当前待重试项' : number(v.retry_attempts)],
      ['最早下次尝试时间', time(v.next_retry_at)],
      ['重试口径', '排除每项首次认领；再次认领不证明网络已发送。时间是计划下界，实际尝试受工作循环调度约束。']
    ]), v.unknown_attempts ? notice(`此前结果未知，可能重复：${v.unknown_attempts} 次尝试。`, 'warn') : null,
    v.last_batch_id ? section('最近批次', [
      pairs([
        [
          '发送单位', unit
        ], [
          '批次记录数', number(v.last_record_count)
        ], [
          '已接收', v.last_accepted === null ? '未确认' : number(v.last_accepted)
        ], [
          '已拒绝', v.last_rejected === null ? '未确认' : number(v.last_rejected)
        ]
      ]), v.last_partial_at ? notice('发生过部分成功；逐项结果未确认，不自动重试该批次。', 'warn') : null, technical([
        [
          '协议', v.protocol
        ], [
          '批次 ID', v.last_batch_id || '未记录'
        ], [
          '批次结果', v.last_outcome || '未记录'
        ], [
          '固定错误分类', v.last_error_kind || '未记录'
        ], [
          '接收方提示', v.last_warning ? '接收方返回提示' : '无'
        ], [
          '最老待处理年龄（秒）', number(v.oldest_pending_age_seconds)
        ], [
          '任一来源最近扫描完成（非全局覆盖）', time(v.last_scan_at)
        ]
      ])
    ]) : notice('尚无发送批次记录'), section('来源覆盖', [
      notice('早于覆盖起点的历史未保证导出；提交延迟超过对账窗口的记录也未保证导出。'), table([
        '来源', '覆盖起点', '覆盖终点', '说明', '技术详情'
      ], v.sources.map(s => ({
        key: s.source, cells: [
          SOURCES[s.source] ?? '来源', time(s.covered_from), time(s.covered_through), s.covered_from === null || s.covered_through === null ? '尚无完整扫描覆盖' : '已完成此窗口扫描', technical([['最近尝试', time(s.last_attempt_at)], ['最近成功', time(s.last_success_at)], ['最近对账', time(s.last_reconcile_at)]])
        ]
      })), '尚无扫描覆盖记录')
    ])
  ])
}
function renderCleanupDetails(v: ObsCleanup): VNodeChild {
  const skip: Record<string, string> = {
    scan_not_covered: '尚无完整导出扫描覆盖', out_of_coverage: '早于导出覆盖起点', unexported_backlog: '未导出积压仍受保护', unconfirmed_export: '导出结果未确认', unexported_terminal: '当前状态尚未确认导出', policy_disabled: '策略未启用'
  }
  const historyGroups: Record<string,string> = {sessions:'闲置会话及完整运行',runs:'无会话的运行',ingest:'历史摄取尝试',operations:'未被引用的临时管理操作',maintenance:'快照、已结算用量及维护历史'}
  return h('div', [
    section('业务历史保留', [
      notice(v.history_enabled ? `已启用：超过 ${v.history_days} 天的闲置会话和已结束历史，按关联组清理。` : '业务历史清理未启用', v.history_enabled ? 'info' : 'warn'),
      h('p','会话从最后活动时间起算；活跃运行、结果未知、配额未结算、仍被引用的证据继续保留。有效知识、长期记忆、当前配置与凭据不按天删除。'),
      h('p',{class:'muted small'},'连接测试只存每个资源的最新结果，继续保留。配置来源、恢复依据、必要清理窗口记录属于保护数据。此页只读，设置在 basalt.yaml 中生效。'),
      h('p',`文件保留 ${v.files?.days ?? v.history_days ?? 30} 天：日志单文件 ${v.files?.log_file_mib ?? 32} MiB、总量 ${v.files?.log_total_mib ?? 256} MiB；按天轮转，容量超限可提前清理旧日志。当前日志保留。`),
      h('p','备份只处理已校验并完成数据库恢复验证的本实例备份；至少保留最新两份。原有未验证备份继续保留。临时文件只清理本实例遗留的上传临时文件，不删除知识源文件。'),
      v.files?.last_run_at ? h('p',{class:'muted small'},`最近文件清理：${time(v.files.last_run_at)}，${v.files.status === 'succeeded' ? '已完成' : '失败'}，删除 ${v.files.deleted_files} 个文件。`) : h('p',{class:'muted small'},'文件清理尚无执行记录。'),
      table(['执行时间','清理范围','结果','删除组数','删除行数','保护组数'],(v.history ?? []).map(r=>({key:r.id,cells:[time(r.at),historyGroups[r.group] ?? r.group,status(r.status),number(r.deleted_groups),number(r.deleted_rows),number(r.protected_groups)]})), '该时间窗暂无业务历史清理记录')
    ]),
    v.enabled ? notice('只清理已启用的可丢弃证据组。') : notice('清理未启用，数据不会被自动删除', 'warn'), notice('清理账本只证明某时间窗发生过清理，不能证明具体记录被删除。'), table([
      '策略', '状态', '保留天数', '技术详情'
    ], v.policies.map(p => ({
      key: p.table, cells: [
        TABLES[p.table] ?? '证据组', p.enabled ? '已启用' : '未启用', number(p.retention_days), technical([
          [
            '表组', p.table
          ], [
            '每批上限', p.batch_size
          ], [
            '每轮最多批数', p.max_batches
          ], [
            '要求逐条确认导出', p.require_confirmed_export ? '是' : '否'
          ]
        ])
      ]
    })), '未记录清理策略'), section('清理执行历史', [
      table([
        '执行时间', '对象', '结果', '删除', '跳过', '说明'
      ], v.runs.map(r => ({
        key: r.cleanup_id, cells: [
          time(r.started_at), TABLES[r.table_kind] ?? '证据组', status(r.status), r.status === 'skipped' ? '未执行' : number(r.deleted), r.status === 'skipped' && r.deleted === 0 && r.skipped === 0 ? '本轮未枚举候选' : number(r.skipped), h('div', [
            h('p', skip[r.skip_reason] ?? (r.status === 'failed' ? '清理失败' : '已记录窗口结果')), technical([
              [
                '清理 ID', r.cleanup_id
              ], [
                '被清理数据窗口起点', time(r.window_start)
              ], [
                '被清理数据窗口终点', time(r.window_end)
              ], [
                '扫描覆盖起点', time(r.scan_covered_from)
              ], [
                '扫描覆盖终点', time(r.scan_covered_through)
              ], [
                '结束时间', time(r.finished_at)
              ], [
                '错误分类', r.error_kind || '未记录'
              ]
            ])
          ])
        ]
      })), '该时间窗没有清理执行记录'), v.truncated ? notice('只展示最近的有界清理执行历史。', 'warn') : null
    ])
  ])
}
function renderExport(v: ObsExport): VNodeChild {
  if (v.enabled || v.last_batch_id || v.pending || v.inflight || v.retry || v.unconfirmed || v.failed_permanent || v.dropped || v.stale) return renderExportDetails(v)
  return section('外部导出', [notice('导出未启用', 'warn'), h('p', '当前只保留本地记录，不会向外部接收端发送观测数据。若需启用，请配置接收端并重启服务。'),
    h('p', {class: 'muted small'}, '本地诊断仍可使用。此页面只读，不会发起导出。'),
    h('details', {class: 'obs-details'}, [h('summary', '查看导出策略与历史'), renderExportDetails(v)])])
}
function renderCleanup(v: ObsCleanup): VNodeChild {
  if (v.enabled || v.history_enabled || v.runs.length) return renderCleanupDetails(v)
  return section('记录保留', [notice('清理未启用，数据不会被自动删除', 'warn'), h('p', '以下天数是候选保留策略，当前不会据此删除记录。此页面只读，不会触发清理。'),
    table(['记录类型', '候选保留期', '当前状态'], v.policies.map(p => ({key: p.table, cells: [TABLES[p.table] ?? '证据组', p.retention_days + ' 天', '未启用']})), '未记录清理策略'),
    h('details', {class: 'obs-details'}, [h('summary', '查看批次限制与清理历史'), renderCleanupDetails(v)])])
}
export const ObservabilityView = defineComponent({
  name: 'ObservabilityView', props: viewProps,
  setup(props) {
    const api = new ObservabilityApi(props.ctx.api)
    const view = TABS.some(([key]) => key === props.query.get('view')) ? props.query.get('view')! : 'overview'
    const forbidden = ref(false), formError = ref('')
    const timelineMode = ref<TimelineMode>(view === 'query' ? 'all' : 'execution')
    // 拒绝提示也属于一次查询，过期失败不能改变当前结果或清空新草稿。
    // Permission side effects belong to one query epoch and cannot affect newer results or drafts.
    let readEpoch = 0
    onUnmounted(() => {readEpoch++})
    const present = ANCHORS.filter(([key]) => props.query.has(key))
    const anchored = view === 'run' || view === 'query'
    const committedRange = initialHistoryRange(props.query, anchored, new Date())
    const rangePreset = ref<HistoryPreset>(committedRange.preset)
    const anchorKind = ref<string>(present[0]?.[0] ?? 'run_id'), anchorValue = ref(props.query.get(present[0]?.[0] ?? 'run_id') ?? '')
    const agent = ref(props.query.get('agent') ?? ''), from = ref(committedRange.from), to = ref(committedRange.to), source = ref(props.query.get('sources') ?? '')
    const rowLimit = ref(props.query.get('limit') ?? '200')
    const window = ref<ObsWindow>(([
      '5m', '15m', '1h', '24h'
    ].includes(props.query.get('window') ?? '') ? props.query.get('window') : '15m') as ObsWindow)
    const q: Record<string, string> = {}
    for (const key of [
      'agent', 'limit'
    ])
      if (props.query.get(key))
        q[key] = props.query.get(key)!
    if (committedRange.from) q.from = committedRange.from
    if (committedRange.to) q.to = committedRange.to
    const anchor: ObsQuery = present.length === 1 ? {
      [anchorKind.value]: anchorValue.value, limit: rowLimit.value, ...(source.value ? {
        sources: source.value
      } : {}), ...(q.from ? {
        from: q.from
      } : {}), ...(q.to ? {
        to: q.to
      } : {})
    } : {}
    async function read<T>(load: () => Promise<T>): Promise<T> {
      const epoch = readEpoch
      try {
        return await load()
      }
      catch (error) {
        if (epoch === readEpoch && error instanceof ApiError && error.status === 403) {
          forbidden.value = true
          anchorValue.value = ''
        }
        throw error
      }
    }
    const overview = useLoader(() => read(() => api.overview(q)), false)
    const timeline = useLoader(() => read(() => api.timeline(anchor)), false)
    const context = useLoader(() => read(() => api.context(String(anchor.run_id ?? ''))), false)
    const metrics = useLoader(() => read(() => api.metrics(window.value)), false)
    const alerts = useLoader(() => read(() => api.alerts(q)), false)
    const exports = useLoader(() => read(() => api.exportStatus()), false)
    const cleanup = useLoader(() => read(() => api.cleanup(q)), false)
    function dirty(): boolean {
      return rangePreset.value !== committedRange.preset || anchorKind.value !== (present[0]?.[0] ?? 'run_id') || anchorValue.value !== (props.query.get(present[0]?.[0] ?? 'run_id') ?? '') || source.value !== (props.query.get('sources') ?? '') || from.value !== committedRange.from || to.value !== committedRange.to || agent.value !== (props.query.get('agent') ?? '') || rowLimit.value !== (props.query.get('limit') ?? '200')
    }
    watch([anchorKind, anchorValue, source, from, to, agent, rowLimit, rangePreset], () => {
      readEpoch++
      formError.value = ''
      timeline.reset(); context.reset(); overview.reset(); metrics.reset(); alerts.reset(); cleanup.reset()
    })
    function load(): void {
      if (dirty() || forbidden.value) return
      readEpoch++
      const start = <T>(s: LoaderState<T>) => { s.reset(); void s.reload() }
      if (view === 'overview' || view === 'groups')
        start(overview)
      if (view === 'metrics')
        start(metrics)
      if ((view === 'run' || view === 'query') && present.length === 1 && anchorValue.value.trim()) {
        start(timeline)
        if (view === 'run' && anchorKind.value === 'run_id')
          start(context)
      }
      if (view === 'alerts')
        start(alerts)
      if (view === 'export')
        start(exports)
      if (view === 'cleanup')
        start(cleanup)
    }
    onMounted(load)
    function refresh(): void {
      if (view !== 'metrics' && view !== 'export' && ['1d', '7d', '30d'].includes(rangePreset.value)) {
        const query = new URLSearchParams(props.query)
        const bounds = historyBounds(rangePreset.value, new Date())
        query.set('range', rangePreset.value); query.set('from', bounds.from); query.set('to', bounds.to)
        navigate(query)
      } else load()
    }
    function navigate(query: URLSearchParams): void { props.ctx.router.navigate('/console/observability?' + query) }
    function submitAnchor(e: Event): void {
      e.preventDefault()
      formError.value = ''
      if (!anchorValue.value.trim() || anchorValue.value.length > 256) {
        formError.value = '请先填写一个关联标识'
        return
      }
      const query = new URLSearchParams({
        view: anchorKind.value === 'run_id' ? 'run' : 'query', [anchorKind.value]: anchorValue.value.trim()
      })
      const limit = Number(rowLimit.value)
      if (!Number.isInteger(limit) || limit < 1 || limit > 1000) {formError.value = '条数必须是1到1000之间的整数'; return}
      query.set('limit', String(limit))
      if (rangePreset.value === 'custom' && (!from.value || !to.value)) {formError.value = '请填写开始和结束时间'; return}
      const start = from.value ? Date.parse(from.value) : null, end = to.value ? Date.parse(to.value) : null
      if (start !== null && !Number.isFinite(start) || end !== null && !Number.isFinite(end) || start !== null && end !== null && (start > end || end - start > MAX_HISTORY_MS)) {formError.value = '请选择不超过30天的有效时间范围'; return}
      query.set('range', rangePreset.value)
      if (start !== null) query.set('from', new Date(start).toISOString())
      if (end !== null) query.set('to', new Date(end).toISOString())
      if (source.value)
        query.set('sources', source.value)
      navigate(query)
    }
    function submitWindow(e: Event): void {
      e.preventDefault()
      formError.value = ''
      const query = new URLSearchParams({
        view
      })
      if (!from.value || !to.value) {formError.value = '请填写开始和结束时间'; return}
      if (from.value || to.value) {
        const end = to.value ? Date.parse(to.value) : Date.now(), start = from.value ? Date.parse(from.value) : end - 86400000
        if (!Number.isFinite(start) || !Number.isFinite(end) || start > end || end - start > MAX_HISTORY_MS) {
          formError.value = '请选择不超过30天的有效时间窗'
          return
        }
        query.set('from', new Date(start).toISOString())
        query.set('to', new Date(end).toISOString())
      }
      query.set('range', rangePreset.value)
      if ((view === 'overview' || view === 'groups') && agent.value.trim())
        query.set('agent', agent.value.trim())
      navigate(query)
    }
    const field = (id: string, label: string, value: string, change: (v: string) => void, placeholder = '') => h('label', {
      for: id
    }, [
      label, h('input', {
        id, type: id === 'obs-from' || id === 'obs-to' ? 'datetime-local' : 'text', step: 1, value: (id === 'obs-from' || id === 'obs-to') && value && Number.isFinite(Date.parse(value)) ? new Date(Date.parse(value) - new Date(value).getTimezoneOffset() * 60000).toISOString().slice(0,19) : value, placeholder, onInput: (e: Event) => change((e.target as HTMLInputElement).value)
      })
    ])
    function filters(): VNodeChild {
      const rangePicker = () => h('div', {class: 'obs-range-picker'}, [h('label', {for: 'obs-range'}, ['时间范围', h('select', {
        id: 'obs-range', value: rangePreset.value, onChange: (event: Event) => {
          const selected = (event.target as HTMLSelectElement).value as HistoryPreset
          rangePreset.value = selected
          if (selected !== 'custom' || !from.value || !to.value) { const bounds = historyBounds(selected, new Date()); from.value = bounds.from; to.value = bounds.to }
        }
      }, [...(anchored ? [['all', '该关联的全部记录']] : []), ['1d', '最近1天'], ['7d', '最近7天'], ['30d', '最近30天'], ['custom', '自定义时间']].map(([key, label]) => h('option', {value: key}, label)))]),
        h('p', rangePreset.value === 'all' ? '按指定运行或会话查询全部关联记录，仍受条数上限约束。' : `${time(from.value)} → ${time(to.value)}（本地时间，最多30天）`)
      ])
      if (view === 'run' || view === 'query')
        return h('form', {
          id: 'obs-query-form', class: 'filters', onSubmit: submitAnchor
        }, [
          h('label', {
            for: 'obs-anchor-kind'
          }, [
            '关联类型', h('select', {
              id: 'obs-anchor-kind', value: anchorKind.value, onChange: (e: Event) => { anchorKind.value = (e.target as HTMLSelectElement).value }
            }, ANCHORS.map(([key, label]) => h('option', {
              value: key
            }, label)))
          ]), field('obs-anchor-value', '关联标识', anchorValue.value, v => { anchorValue.value = v }), h('label', {
            for: 'obs-sources'
          }, [
            '来源', h('select', {
              id: 'obs-sources', value: source.value, onChange: (e: Event) => { source.value = (e.target as HTMLSelectElement).value }
            }, [
              h('option', {
                value: ''
              }, '全部来源'), ...Object.entries(SOURCES).map(([key, label]) => h('option', {
                value: key
              }, label))
            ])
          ]), rangePicker(), ...(rangePreset.value === 'custom' ? [field('obs-from', '事件开始时间', from.value, v => {from.value = v}, '本地时间'), field('obs-to', '事件结束时间', to.value, v => {to.value = v}, '本地时间')] : []), field('obs-limit', '最多条数', rowLimit.value, v => {rowLimit.value = v}), h('button', {
            type: 'submit'
          }, '查询')
        ])
      if (view === 'metrics')
        return h('label', {
          for: 'obs-window'
        }, [
          '指标窗口', h('select', {
            id: 'obs-window', value: window.value, onChange: (e: Event) => navigate(new URLSearchParams({
              view, window: (e.target as HTMLSelectElement).value
            }))
          }, [
            [
              '5m', '5分钟'
            ], [
              '15m', '15分钟'
            ], [
              '1h', '1小时'
            ], [
              '24h', '24小时'
            ]
          ].map(([key, label]) => h('option', {
            value: key
          }, label)))
        ])
      if (view === 'export')
        return null
      return h('form', {
        class: 'filters', onSubmit: submitWindow
      }, [
        rangePicker(), ...(rangePreset.value === 'custom' ? [field('obs-from', '开始时间', from.value, v => { from.value = v }, '本地时间'), field('obs-to', '结束时间', to.value, v => { to.value = v }, '本地时间')] : []), view === 'overview' || view === 'groups' ? field('obs-agent', 'Agent', agent.value, v => { agent.value = v }, '全部') : null, h('button', {
          type: 'submit'
        }, '查询')
      ])
    }
    return (): VNode => {
      let content: VNodeChild
      if (forbidden.value)
        content = notice('无权访问；请使用具有管理权限的访问凭据。', 'error')
      else {
        const parts: VNodeChild[] = [
          filters(), formError.value ? notice(formError.value, 'error') : null
        ]
        if (view === 'overview')
          parts.push(readState(overview, o => h('div', [
            o.runs === 0 ? notice('该时间窗没有 Run 记录') : overviewCards(o), groupRows(o)
          ])), h('p', {class: 'obs-shortcuts'}, [h('a', {href: observabilityHref('metrics')}, '查看指标与耗时分布 →'), h('a', {href: observabilityHref('alerts')}, '查看告警规则 →')]))
        if (view === 'groups')
          parts.push(readState(overview, groupRows))
        if (view === 'run' || view === 'query') {
          parts.push(present.length > 1 ? notice('请只给出一个关联标识', 'warn') : present.length === 0 ? notice('请先填写一个关联标识再查询') : readState(timeline, v => diagnosticTimeline(v, timelineMode.value, mode => { timelineMode.value = mode })))
          if (view === 'run' && anchorKind.value === 'run_id')
            parts.push(readState(context, renderContext))
        }
        if (view === 'metrics')
          parts.push(readState(metrics, metricDashboard))
        if (view === 'alerts')
          parts.push(readState(alerts, renderAlerts))
        if (view === 'export')
          parts.push(readState(exports, renderExport))
        if (view === 'cleanup')
          parts.push(readState(cleanup, renderCleanup))
        content = h('div', parts)
      }
      return h('section', {
        class: 'view view-observability'
      }, [
        pageHeading('OBSERVABILITY', '运行排障', '先看执行结果，再定位异常步骤、耗时与上下文问题。', h('button', {
          type: 'button', disabled: dirty() || forbidden.value, onClick: refresh
        }, '刷新')), h('nav', {
          class: 'tabs', 'aria-label': '可观测视图'
        }, TABS.map(([key, label]) => h('a', {
          href: observabilityHref(key), class: ['tab', view === key ? 'active' : ''], 'aria-current': view === key ? 'page' : undefined
        }, label))), content
      ])
    }
  },
})

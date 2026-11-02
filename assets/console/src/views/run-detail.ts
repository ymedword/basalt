// 运行详情视图：准入资源清单、最终文本与取消动作（FR11 / FR12 / FR13）。
// 历史运行没有清单时明确标为"未记录"，不补造数据。
// Run detail: the admission resource manifest, final text and cancel action. Runs
// predating the manifest capability are shown as explicitly unrecorded.
import { defineComponent, h, ref } from 'vue'
import type { VNode, VNodeChild } from 'vue'
import type { RunDetail } from '../api'
import { ObservabilityApi } from '../api-observability'
import { runSummary } from '../observation-presentation'
import {
  ConfirmDialog,
  DataTable,
  Notice,
  formatTimestamp,
  shortHash,
} from '../components/ui'
import { renderSafeText } from '../render'
import { diagnosticsHref, observabilityHref } from '../router'
import { CompactionPanel } from './compaction'
import {
  AGENT_STATE_LABELS,
  RUN_STATUS_LABELS,
  badge,
  definitions,
  identifier,
  pageHeading,
  runStatusTone,
  section,
  useLoader,
  viewProps,
} from './shared'

export const RunDetailView = defineComponent({
  name: 'RunDetailView',
  props: viewProps,
  setup(props) {
    const runId = props.params.id ?? ''
    const api = props.ctx.api
    const run = useLoader(() => api.getRun(runId))
    const diagnosis = useLoader(() => new ObservabilityApi(api).timeline({run_id: runId, limit: '200'}))

    const cancelOpen = ref(false)
    const cancelBusy = ref(false)
    const cancelNotice = ref('')
    const cancelError = ref('')

    async function confirmCancel(): Promise<void> {
      if (cancelBusy.value) {
        return
      }
      cancelBusy.value = true
      cancelError.value = ''
      try {
        const result = await api.cancelRun(runId)
        cancelNotice.value =
          result.message === '' ? '取消请求已发送，等待运行自行收场。' : result.message
        cancelOpen.value = false
        await run.reload()
      } catch (caught) {
        cancelError.value = caught instanceof Error ? caught.message : '取消失败'
      } finally {
        cancelBusy.value = false
      }
    }

    function renderManifest(detail: RunDetail): VNodeChild {
      const manifest = detail.resource_manifest
      if (manifest === null || manifest.legacy_unrecorded) {
        return h(Notice, {
          tone: 'warn',
          text: '该运行未记录资源清单（早于本能力的历史运行）；按约定不补造。',
        })
      }
      return section('准入资源清单', [
        h('p', { class: 'muted small' }, [
          `清单格式 v${manifest.format_version} · 准入时生命周期状态：`,
          AGENT_STATE_LABELS[manifest.agent_lifecycle_state],
        ]),
        h('h3', 'Provider / MCP 版本'),
        h(DataTable, {
          headers: ['类型', '名称', '版本号', '内容哈希'],
          rows: [...manifest.providers, ...manifest.mcp_servers].map((item) => ({
            key: `${item.kind}-${item.name}`,
            cells: [
              item.kind === 'model_connection' ? '模型连接' : 'MCP 服务',
              item.name,
              String(item.version_no),
              h('code', { title: item.content_hash }, shortHash(item.content_hash)),
            ],
          })),
          empty: '该运行没有 Provider / MCP 资源引用',
        }),
        h('h3', '技能版本'),
        h(DataTable, {
          headers: ['技能', '版本号', '卡片哈希', '正文哈希'],
          rows: manifest.skills.map((item) => ({
            key: item.name,
            cells: [
              item.name,
              String(item.version_no),
              h('code', { title: item.skill_md_hash }, shortHash(item.skill_md_hash)),
              h('code', { title: item.content_hash }, shortHash(item.content_hash)),
            ],
          })),
          empty: '该运行没有技能引用',
        }),
        h('h3', '文件哈希'),
        h(DataTable, {
          headers: ['文件', '大小', 'SHA-256'],
          rows: manifest.files.map((file) => ({
            key: file.path,
            cells: [
              file.path,
              String(file.size),
              h('code', { title: file.sha256 }, shortHash(file.sha256)),
            ],
          })),
          empty: '该运行没有文件哈希',
        }),
      ])
    }

    return (): VNode => {
      const content: VNodeChild[] = [
        pageHeading(
          'WORKSPACE / RUNS',
          '运行详情',
          '查看执行结果、触发来源与准入时固定的资源版本。',
          // 诊断事件按 run_id 深链：运行内的模型 / 工具 / 权限 / 配额判定都在那里。
          // Diagnostics deep-link by run_id; the run page owns the id, the page does the rest.
          h('div', { class: 'form-actions' }, [
            h('a', { class: 'button primary', href: observabilityHref('run', ['run_id', runId]) }, '排查此次运行'),
            h('a', { class: 'button', href: diagnosticsHref(['run_id', runId]) }, '诊断明细'),
          ]),
        ),
        h('p', { class: 'back-link' }, [h('a', { href: '/console/runs' }, '← 返回运行列表')]),
      ]

      if (run.error.value !== '') {
        content.push(h(Notice, { tone: 'error', text: run.error.value }))
      }
      if (run.loading.value && run.data.value === null) {
        content.push(h('p', { class: 'muted' }, '加载中…'))
      }

      const detail = run.data.value
      if (detail !== null) {
        content.push(
          h('div', { class: 'detail-identity' }, [
            h('code', { class: 'detail-id', title: detail.run_id }, detail.run_id),
            badge(RUN_STATUS_LABELS[detail.status], runStatusTone(detail.status)),
            h('span', { class: 'muted' }, ` ${detail.agent} · ${detail.trigger} · ${detail.channel}`),
          ]),
          diagnosis.data.value && Array.isArray(diagnosis.data.value.entries) ? runSummary(diagnosis.data.value) : null,
          diagnosis.error.value ? h(Notice, {tone: 'warn', text: '诊断摘要暂时无法读取；下方保留权威运行记录。'}) : null,
          h('details', {class: 'obs-details'}, [h('summary', '运行标识与执行记录'),
          section('执行记录', [definitions([
            ['会话', identifier(detail.session_id)],
            ['迭代', String(detail.iterations)],
            ['工具调用', String(detail.tool_calls)],
            ['配置哈希', h('code', { title: detail.config_hash }, shortHash(detail.config_hash))],
            ['清单哈希', h('code', { title: detail.manifest_hash }, shortHash(detail.manifest_hash))],
            ['开始时间', formatTimestamp(detail.started_at)],
            ['结束时间', formatTimestamp(detail.finished_at)],
            ['原因', detail.reason === '' ? '—' : detail.reason],
          ])])]),
          // 压缩记录（T089）：无状态 Run 的压缩条目按 run 范围存；会话范围的条目在
          // 会话详情页看——两个范围各有自己的当前代次，合并列表不满足契约排序口径。
          // Run-scope compactions only; session-scope entries live on the session page.
          h('details', {class: 'obs-details'}, [h('summary', '归属于此 Run 的压缩记录'), h('p', {class: 'muted small'}, '会话范围的压缩记录在会话详情中查看；上下文诊断展示的是本轮装配引用的压缩状态。'), h(CompactionPanel, { ctx: props.ctx, runId })]),
          section('最终文本', [
            detail.final_text === ''
              ? h('p', { class: 'muted' }, '该运行没有最终文本')
              : h('div', { class: 'final-text' }, renderSafeText(detail.final_text)),
          ]),
          h('details', {class: 'obs-details'}, [h('summary', '本次固定的资源版本'), renderManifest(detail)]),
        )

        if (detail.status === 'running') {
          content.push(
            h('div', { class: 'form-actions' }, [
              h(
                'button',
                {
                  type: 'button',
                  class: 'danger',
                  disabled: cancelBusy.value,
                  onClick: () => {
                    cancelOpen.value = true
                  },
                },
                '取消运行',
              ),
            ]),
          )
        }
        if (cancelNotice.value !== '') {
          content.push(h(Notice, { tone: 'info', text: cancelNotice.value }))
        }
        if (cancelError.value !== '') {
          content.push(h(Notice, { tone: 'error', text: cancelError.value }))
        }
      }

      content.push(
        h(ConfirmDialog, {
          open: cancelOpen.value,
          title: '取消运行',
          message: '取消只会通知循环自行收场，等待最多 30 秒；不会直接改写终态。继续？',
          confirmText: '取消运行',
          busy: cancelBusy.value,
          onConfirm: () => {
            void confirmCancel()
          },
          onCancel: () => {
            cancelOpen.value = false
          },
        }),
      )

      return h('section', { class: 'view view-run-detail' }, content)
    }
  },
})

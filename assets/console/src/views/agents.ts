// Agent 工作区入口：保留生命周期与问题信息，按名称 / 状态查找。页头与空状态都提供
// 「新建 Agent」入口（US-12 页面操作闭环），并可从同一处进入「已归档」恢复。
// Agent workspace entry points with truthful lifecycle, configuration and capability
// summaries. The header and the empty state both open the create page, and the
// archived restore page is reachable from the header as well.
import { computed, defineComponent, h, ref } from 'vue'
import type { VNode, VNodeChild } from 'vue'
import { Notice } from '../components/ui'
import { agentConfigHref, agentHref } from '../router'
import { AGENT_STATE_LABELS, badge, lifecycleTone, useLoader, viewProps } from './shared'

/** 列表页的新建入口：与页头按钮共用同一个跳转，避免两处文案漂移。 */
// One shared navigation target for the header and empty-state create entries.
const CREATE_AGENT_PATH = '/console/agents/new'
const ARCHIVED_AGENTS_PATH = '/console/archived'

export const AgentsView = defineComponent({
  name: 'AgentsView', props: viewProps,
  setup(props) {
    const router = props.ctx.router
    const profiles = useLoader(() => props.ctx.api.listProfiles())

    /** 页头动作：查看已归档 + 新建 Agent（主操作）。 */
    // Header actions: archived list plus the primary create entry.
    function renderHeadingActions(data: readonly unknown[] | null): VNode {
      return h('div', { class: 'page-actions' }, [
        h('span', { class: 'page-count' }, data === null ? '正在读取' : data.length + ' 个 Agents'),
        h('button', {
          type: 'button',
          onClick: () => {
            router.navigate(ARCHIVED_AGENTS_PATH)
          },
        }, '查看已归档'),
        h('button', {
          type: 'button',
          class: 'primary',
          onClick: () => {
            router.navigate(CREATE_AGENT_PATH)
          },
        }, '新建 Agent'),
      ])
    }

    /** 空状态里的同一入口：工作区没有任何 Agent 时也不要求用户记住深链接。 */
    // The same create entry from the empty state, so no deep link is required.
    function renderEmptyState(hasAgents: boolean): VNodeChild {
      return h('div', { class: 'empty' }, [
        h('strong', hasAgents ? '没有匹配的 Agent' : '工作区中还没有 Agent'),
        h('p', hasAgents ? '试试其他名称，或调整状态筛选。' : '新建 Agent 后，它会在这里拥有自己的工作区。'),
        hasAgents ? null : h('button', {
          type: 'button',
          class: 'primary',
          onClick: () => {
            router.navigate(CREATE_AGENT_PATH)
          },
        }, '新建 Agent'),
      ])
    }
    // 一句话创建：把描述带到新建页，由那一页自动生成草稿并给出结构化预览；
    // 这里的入口只导航，不调用任何写端点（创建仍由用户确认）。
    // The one-liner entry only navigates; the create page does the author call and
    // the user still confirms the create.
    const oneLiner = ref('')
    function startOneLiner(): void {
      const text = oneLiner.value.trim()
      if (text === '') {
        return
      }
      const params = new URLSearchParams({ description: text, autodraft: '1' })
      router.navigate(`${CREATE_AGENT_PATH}?${params.toString()}`)
    }

    /** 一句话创建区：列表再长也在首屏可见，不要求先记住“新建”按钮。 */
    // The one-liner composer stays above the list so it never depends on a deep link.
    function renderOneLiner(): VNode {
      return h('section', { class: 'card one-liner-create', id: 'one-liner-create' }, [
        h('div', { class: 'one-liner-copy' }, [
          h('h2', '一句话创建 Agent'),
          h(
            'p',
            { class: 'muted' },
            '描述它要解决的问题。作者模型会起草 AGENT.md，先给你结构化预览；确认后才创建（默认停用），不会直接运行。',
          ),
        ]),
        h('div', { class: 'one-liner-bar' }, [
          h('textarea', {
            id: 'agent-one-liner',
            class: 'editor',
            rows: 2,
            spellcheck: false,
            placeholder: '例如：每天早上汇总 GitHub Trending 并发送到飞书',
            value: oneLiner.value,
            onInput: (event: Event) => {
              oneLiner.value = (event.target as HTMLTextAreaElement).value
            },
            onKeydown: (event: KeyboardEvent) => {
              if (event.key === 'Enter' && (event.ctrlKey || event.metaKey)) {
                event.preventDefault()
                startOneLiner()
              }
            },
          }),
          h(
            'button',
            {
              type: 'button',
              class: 'primary',
              id: 'agent-one-liner-start',
              disabled: oneLiner.value.trim() === '',
              onClick: startOneLiner,
            },
            '生成草稿并预览',
          ),
        ]),
      ])
    }

    const search = ref('')
    const filter = ref('all')
    const filtered = computed(() => (profiles.data.value ?? []).filter(profile => {
      const needle = search.value.trim().toLocaleLowerCase()
      const matches = !needle || [profile.name, profile.display_name, profile.description, profile.provider, profile.model].some(value => value.toLocaleLowerCase().includes(needle))
      return matches && (filter.value === 'all' || (filter.value === 'ready' ? profile.runnable : !profile.lifecycle.valid || !profile.registered || profile.problems.length > 0))
    }))
    return (): VNode => {
      const data = profiles.data.value
      const ready = data?.filter(item => item.runnable).length ?? 0
      const attention = data?.filter(item => !item.lifecycle.valid || !item.registered || item.problems.length > 0).length ?? 0
      return h('section', { class: 'view view-agents' }, [
        h('div', { class: 'page-heading' }, [h('div', [h('p', { class: 'eyebrow' }, 'WORKSPACE / 02'), h('h1', 'Agents'), h('p', { class: 'page-description' }, '每个 Agent，一个工作区。会话、执行、配置与记忆都在这里。')]), renderHeadingActions(data)]),
        profiles.error.value ? h(Notice, { tone: 'error', text: profiles.error.value }) : null,
        props.query.get('archived') === '1'
          ? h(Notice, { tone: 'info', text: '已归档该 Agent：目录移入归档区，会话、记忆、审计与定时执行历史保留，可在「查看已归档」中恢复。' })
          : null,
        renderOneLiner(),
        h('div', { class: 'agent-toolbar' }, [
          h('div', { class: 'agent-filters', 'aria-label': 'Agent 状态筛选' }, [
            ['all', '全部', data?.length ?? 0], ['ready', '可运行', ready], ['attention', '需要关注', attention],
          ].map(([key, label, count]) => h('button', { type: 'button', class: filter.value === key ? 'active' : '', 'aria-pressed': filter.value === key, onClick: () => { filter.value = String(key) } }, [String(label), h('span', String(count))]))),
          h('label', { class: 'agent-search' }, [h('span', { class: 'sr-only' }, '搜索 Agent'), h('span', { 'aria-hidden': 'true' }, '⌕'), h('input', { type: 'search', placeholder: '搜索名称、描述或模型…', value: search.value, onInput: (event: Event) => { search.value = (event.target as HTMLInputElement).value } })]),
        ]),
        profiles.loading.value && data === null ? h('p', { class: 'loading' }, '正在读取 Agents…') : null,
        data === null ? null : filtered.value.length === 0 ? renderEmptyState(data.length > 0) : h('div', { class: 'agent-grid' }, filtered.value.map(profile => {
          const valid = profile.lifecycle.valid && profile.lifecycle.state !== null
          const problem = profile.problems.join('；') || profile.lifecycle.lifecycle_error
          // 声明了通知渠道但 tools 里还没有 notify：只在旧配置（R7 之前保存）中出现，
          // 下次保存配置时会由系统自动装配；系统代投不依赖它。与配置页同一口径。
          const notifyGap = profile.notify_channels.length > 0 && !profile.tools.includes('notify')
          // 显示名只换标题文本；技术名仍以小号代码出现在同一行，目录 / API / 运行身份不变。
          // The display name only retitles the card; the technical name stays visible.
          const shownName = profile.display_name.trim() === '' ? profile.name : profile.display_name.trim()
          return h('article', { key: profile.name, class: 'agent-card' }, [
            h('div', { class: 'agent-card-top' }, [h('span', { class: 'agent-monogram', 'aria-hidden': 'true' }, profile.name.slice(0, 2).toUpperCase()), badge(valid ? AGENT_STATE_LABELS[profile.lifecycle.state!] : '状态无效', valid ? lifecycleTone(profile.lifecycle.state!) : 'bad')]),
            h('h2', [
              h('a', { href: agentHref(profile.name) }, shownName),
              shownName === profile.name
                ? null
                : h('code', { class: 'agent-technical-name', title: '技术名（目录 / API）' }, profile.name),
            ]),
            h('p', { class: 'agent-description' }, profile.description || '尚未填写描述'),
            h('div', { class: 'agent-model' }, [h('span', '模型'), h('div', [h('strong', profile.model || '未配置'), h('small', profile.provider || '未配置连接')])]),
            problem ? h('p', { class: 'agent-card-problem' }, problem) : null,
            notifyGap ? h('p', { class: 'agent-card-problem' }, '已声明通知渠道，但 tools 里还没有 notify：下次在配置页保存时会由系统自动装配（定时任务的系统投递不依赖它）') : null,
            h('div', { class: 'agent-capabilities' }, [h('span', profile.tools.length + ' 工具'), h('span', profile.skills.length + ' 技能'), h('span', profile.schedules + ' 定时')]),
            h('div', { class: 'agent-card-bottom' }, [
              h('span', { class: ['agent-runnable', profile.runnable ? 'is-ready' : ''] }, profile.runnable ? '可运行' : '暂不可运行'),
              // 配置页的快捷入口：详情页以「配置」页签为唯一入口，这里给直达链接，避免依赖深链接。
              h('a', { href: agentConfigHref(profile.name), 'aria-label': '配置 Agent ' + profile.name }, '配置'),
              h('a', { href: agentHref(profile.name), 'aria-label': '打开 Agent ' + profile.name }, '打开工作区 ↗'),
            ]),
          ])
        })),
      ])
    }
  },
})
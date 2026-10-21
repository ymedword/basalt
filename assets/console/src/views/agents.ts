// Agent 工作区入口：保留生命周期与问题信息，按名称 / 状态查找。
// Agent workspace entry points with truthful lifecycle, configuration and capability summaries.
import { computed, defineComponent, h, ref } from 'vue'
import type { VNode } from 'vue'
import { Notice } from '../components/ui'
import { agentHref } from '../router'
import { AGENT_STATE_LABELS, badge, lifecycleTone, useLoader, viewProps } from './shared'

export const AgentsView = defineComponent({
  name: 'AgentsView', props: viewProps,
  setup(props) {
    const profiles = useLoader(() => props.ctx.api.listProfiles())
    const search = ref('')
    const filter = ref('all')
    const filtered = computed(() => (profiles.data.value ?? []).filter(profile => {
      const needle = search.value.trim().toLocaleLowerCase()
      const matches = !needle || [profile.name, profile.description, profile.provider, profile.model].some(value => value.toLocaleLowerCase().includes(needle))
      return matches && (filter.value === 'all' || (filter.value === 'ready' ? profile.runnable : !profile.lifecycle.valid || !profile.registered || profile.problems.length > 0))
    }))
    return (): VNode => {
      const data = profiles.data.value
      const ready = data?.filter(item => item.runnable).length ?? 0
      const attention = data?.filter(item => !item.lifecycle.valid || !item.registered || item.problems.length > 0).length ?? 0
      return h('section', { class: 'view view-agents' }, [
        h('div', { class: 'page-heading' }, [h('div', [h('p', { class: 'eyebrow' }, 'WORKSPACE / 02'), h('h1', 'Agents'), h('p', { class: 'page-description' }, '每个 Agent，一个工作区。会话、执行、配置与记忆都在这里。')]), h('span', { class: 'page-count' }, data === null ? '正在读取' : data.length + ' 个 Agents')]),
        profiles.error.value ? h(Notice, { tone: 'error', text: profiles.error.value }) : null,
        h('div', { class: 'agent-toolbar' }, [
          h('div', { class: 'agent-filters', 'aria-label': 'Agent 状态筛选' }, [
            ['all', '全部', data?.length ?? 0], ['ready', '可运行', ready], ['attention', '需要关注', attention],
          ].map(([key, label, count]) => h('button', { type: 'button', class: filter.value === key ? 'active' : '', 'aria-pressed': filter.value === key, onClick: () => { filter.value = String(key) } }, [String(label), h('span', String(count))]))),
          h('label', { class: 'agent-search' }, [h('span', { class: 'sr-only' }, '搜索 Agent'), h('span', { 'aria-hidden': 'true' }, '⌕'), h('input', { type: 'search', placeholder: '搜索名称、描述或模型…', value: search.value, onInput: (event: Event) => { search.value = (event.target as HTMLInputElement).value } })]),
        ]),
        profiles.loading.value && data === null ? h('p', { class: 'loading' }, '正在读取 Agents…') : null,
        data === null ? null : filtered.value.length === 0 ? h('div', { class: 'empty' }, [h('strong', data.length === 0 ? '工作区中还没有 Agent' : '没有匹配的 Agent'), h('p', data.length === 0 ? '添加 Agent 后，它会在这里拥有自己的工作区。' : '试试其他名称，或调整状态筛选。')]) : h('div', { class: 'agent-grid' }, filtered.value.map(profile => {
          const valid = profile.lifecycle.valid && profile.lifecycle.state !== null
          const problem = profile.problems.join('；') || profile.lifecycle.lifecycle_error
          return h('article', { key: profile.name, class: 'agent-card' }, [
            h('div', { class: 'agent-card-top' }, [h('span', { class: 'agent-monogram', 'aria-hidden': 'true' }, profile.name.slice(0, 2).toUpperCase()), badge(valid ? AGENT_STATE_LABELS[profile.lifecycle.state!] : '状态无效', valid ? lifecycleTone(profile.lifecycle.state!) : 'bad')]),
            h('h2', [h('a', { href: agentHref(profile.name) }, profile.name)]),
            h('p', { class: 'agent-description' }, profile.description || '尚未填写描述'),
            h('div', { class: 'agent-model' }, [h('span', '模型'), h('div', [h('strong', profile.model || '未配置'), h('small', profile.provider || '未配置连接')])]),
            problem ? h('p', { class: 'agent-card-problem' }, problem) : null,
            h('div', { class: 'agent-capabilities' }, [h('span', profile.tools.length + ' 工具'), h('span', profile.skills.length + ' 技能'), h('span', profile.schedules + ' 定时')]),
            h('div', { class: 'agent-card-bottom' }, [h('span', { class: ['agent-runnable', profile.runnable ? 'is-ready' : ''] }, profile.runnable ? '可运行' : '暂不可运行'), h('a', { href: agentHref(profile.name), 'aria-label': '打开 Agent ' + profile.name }, '打开工作区 ↗')]),
          ])
        })),
      ])
    }
  },
})
// 登录视图：token 与显式的本机信任两个入口，凭据只在内存（约束 40）。
// 登录成功后回到 `next` 指定的站内路径；next 已经过 safeNextPath 清洗。
// The login view offers token and explicit local-trust entry points; credentials
// stay in memory and the sanitized `next` path is restored after verification.
import { defineComponent, h, ref } from 'vue'
import type { VNode } from 'vue'
import { errorMessage } from '../api'
import { Notice } from '../components/ui'
import { brandMark, basaltIllustration } from '../components/brand'
import { CONSOLE_BASE, safeNextPath } from '../router'
import { viewProps } from './shared'

export const LoginView = defineComponent({
  name: 'LoginView',
  props: viewProps,
  setup(props) {
    const token = ref('')
    const busy = ref(false)
    const error = ref('')
    // 登录后返回路径：只接受站内 /console 路径，否则回默认总览。
    const next = safeNextPath(props.query.get('next')) || `${CONSOLE_BASE}/`

    /** 应用凭据 → 用 GET /info 校验 → 跳转 next；失败立即丢弃凭据并显示固定文案。 */
    async function signIn(apply: () => void): Promise<void> {
      if (busy.value) {
        return
      }
      error.value = ''
      busy.value = true
      try {
        apply()
        await props.ctx.api.getInfo()
        props.ctx.router.navigate(next, { replace: true, force: true })
      } catch (caught) {
        // 校验失败（含 401 / 429）：凭据不保留，用户可改输入后重试。
        props.ctx.auth.clear()
        error.value = errorMessage(caught)
      } finally {
        busy.value = false
      }
    }

    function submit(event: Event): void {
      event.preventDefault()
      if (token.value.trim() === '') {
        error.value = '请输入访问密码'
        return
      }
      void signIn(() => {
        props.ctx.auth.setToken(token.value)
      })
    }

    return (): VNode =>
      h('div', { class: 'login-layout' }, [
        h('section', { class: 'login-intro', 'aria-label': 'Basalt Agent OS' }, [
          h('a', { href: CONSOLE_BASE + '/', class: 'login-brand' }, [brandMark(), h('strong', 'BASALT')]),
          h('p', { class: 'eyebrow' }, 'AGENT OPERATING SYSTEM'),
          h('h2', ['你的工作区，', h('br'), '由你掌控。']),
          h('p', { class: 'login-intro-note' }, '定义 Agent，查看执行，持续改进。'),
          basaltIllustration(),
        ]),
        h('section', { class: 'view view-login' }, [
        h('p', { class: 'eyebrow' }, 'WORKSPACE ACCESS'),
        h('h1', '登录管理台'),
        h('p', { class: 'muted' }, '凭据只保存在当前页面内存；刷新或退出后需要重新输入。'),
        h('p', { class: 'muted' }, '请输入部署者提供的访问密码'),
        h('form', { class: 'login-form', onSubmit: submit }, [
          h('label', { for: 'console-token' }, '访问密码'),
          h('input', {
            id: 'console-token',
            name: 'token',
            type: 'password',
            autocomplete: 'off',
            value: token.value,
            onInput: (event: Event) => {
              token.value = (event.target as HTMLInputElement).value
            },
          }),
          h(
            'button',
            { type: 'submit', class: 'primary', disabled: busy.value },
            busy.value ? '校验中…' : '登录',
          ),
        ]),
        h('div', { class: 'login-alt' }, [
          h('p', { class: 'muted' }, '或在本机直接回环访问时使用本机信任模式：'),
          h(
            'button',
            {
              type: 'button',
              disabled: busy.value,
              onClick: () => {
                void signIn(() => {
                  props.ctx.auth.setLocalTrust()
                })
              },
            },
            '使用本机信任',
          ),
        ]),
        h('details', { class: 'technical-details login-identity' }, [
          h('summary', '身份与权限说明'),
          // 设计 §2.7 的固定口径：这是**共享访问密码**，页面不得把它说成个人账户、
          // 角色或操作人隔离，也不使用「token / 密钥 / 管理员密码」这些词。
          // Fixed shared-password disclosure (design §2.7); no account or role claim.
          h(
            'p',
            { class: 'muted small' },
            '当前为共享访问密码：持有同一密码的访问者具有相同的管理权限，系统不提供个人账户、角色或操作人隔离。',
          ),
          h(
            'p',
            { class: 'muted small' },
            '审计记录只能归因到这次访问所用的密码；多人共用同一密码时，无法区分到具体的人。' +
              '后续规划：网页登录优先接入企业 SSO；自动化使用带作用域的 API 密钥；模型厂商凭据仍属「模型连接」，三者用途不同，不得混用。',
          ),
        ]),
        error.value === '' ? null : h(Notice, { tone: 'error', text: error.value }),
        next === `${CONSOLE_BASE}/`
          ? null
          : h('p', { class: 'muted' }, `登录后将返回：${next}`),
        ]),
      ])
  },
})

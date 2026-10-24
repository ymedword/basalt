// Agent 多文件编辑组件（挂在 /console/agents/:name 页内）：受管文件集合 ETag +
// If-Match，写后重新读取磁盘真值。404 / 412 / 409 各有明确分支：
//   - 412 → 刷新集合 ETag 与新清单，**保留**正在编辑的正文（只换基线原文）；
//   - 409 → 展示服务端点名的原因，不动基线、不重试（分歧闸门由服务端判）；
//   - 200 → 重新 GET 清单与每份正文，**不回显**请求体（不做乐观更新）。
// 正文读取失败的文件（超限 / 二进制 / 已消失）标记为不可提交，避免用空正文覆盖原文件。
// Multi-file editor for one agent: conditional write on the managed-set ETag, a full
// re-read after every write, and no optimistic updates.
import { computed, defineComponent, h, onMounted, onUnmounted, ref } from 'vue'
import type { PropType, VNode, VNodeChild } from 'vue'
import { ApiError, errorMessage } from '../api'
import type { ApiClient } from '../api'
import { adminApi } from '../api-admin'
import type { FileDraft } from '../api-admin'
import { Notice } from './ui'

/** 一个可编辑文件：`original` 是最近一次从服务端读到的正文，脏判定由此比较。 */
// One editable file; `original` is the last server-read text used for dirty checks.
interface EditableFile {
  readonly path: string
  content: string
  original: string
  /** false = 正文读取失败（超限 / 二进制 / 已消失），该文件不参与提交。 */
  readonly loaded: boolean
}

export const AgentFiles = defineComponent({
  name: 'AgentFiles',
  props: {
    api: { type: Object as PropType<ApiClient>, required: true },
    agent: { type: String, required: true },
  },
  setup(props) {
    const client = adminApi(props.api)
    const files = ref<EditableFile[]>([])
    const etag = ref('')
    const loading = ref(false)
    const busy = ref(false)
    const loadError = ref('')
    const actionError = ref('')
    const notice = ref('')

    const dirty = computed(() =>
      files.value.some((file) => file.loaded && file.content !== file.original),
    )
    const unreadable = computed(() =>
      files.value.filter((file) => !file.loaded).map((file) => file.path),
    )

    /** 读一份文件的正文；失败只标记该文件不可编辑，不用空正文顶替。 */
    async function readFile(path: string): Promise<EditableFile> {
      try {
        const file = await client.getWorkspaceFile(`${props.agent}/${path}`)
        return { path, content: file.content, original: file.content, loaded: true }
      } catch {
        return { path, content: '', original: '', loaded: false }
      }
    }

    /** 全量读取：清单与每份正文都以服务端为准（写后重读走同一条路径）。 */
    async function load(): Promise<void> {
      loading.value = true
      loadError.value = ''
      try {
        const view = await client.getAgentFiles(props.agent)
        etag.value = view.etag
        files.value = await Promise.all(view.files.map((entry) => readFile(entry.path)))
      } catch (caught) {
        loadError.value = errorMessage(caught)
      } finally {
        loading.value = false
      }
    }

    /**
     * 412 后的基线刷新：只换 ETag 与未编辑文件；正在编辑的正文保持用户输入，
     * 仅把基线原文更新为服务端最新值，让"脏"仍表示"与最新基线不同"。
     * After a 412 the user's text survives; only the baseline moves forward.
     */
    async function refreshAfterConflict(): Promise<void> {
      try {
        const view = await client.getAgentFiles(props.agent)
        etag.value = view.etag
        const merged: EditableFile[] = []
        for (const entry of view.files) {
          const current = files.value.find((file) => file.path === entry.path)
          if (current !== undefined && current.loaded && current.content !== current.original) {
            const server = await readFile(entry.path)
            merged.push(server.loaded ? { ...current, original: server.original } : current)
            continue
          }
          merged.push(await readFile(entry.path))
        }
        // 服务端已不存在的文件仍留在编辑器里：下一次提交会显式重建它，而不是静默丢弃输入。
        for (const file of files.value) {
          if (!view.files.some((entry) => entry.path === file.path)) {
            merged.push(file)
          }
        }
        files.value = merged
      } catch (caught) {
        actionError.value = `${actionError.value}（刷新 ETag 失败：${errorMessage(caught)}）`
      }
    }

    /** 提交全部可读文件；If-Match 用最近一次读取的集合 ETag（空串也会被服务端 412）。 */
    async function submit(): Promise<void> {
      if (busy.value) {
        return
      }
      const payload: FileDraft[] = files.value
        .filter((file) => file.loaded)
        .map((file) => ({ path: file.path, content: file.content }))
      if (payload.length === 0) {
        actionError.value = '没有可提交的文件：请先重新读取文件集合'
        return
      }
      busy.value = true
      actionError.value = ''
      notice.value = ''
      try {
        await client.submitAgentFiles(props.agent, payload, etag.value)
        // 写后重读：清单与正文重新从服务端读取，不把请求体当结果回显。
        await load()
        notice.value = '已提交，文件集合与 ETag 已重新读取'
      } catch (caught) {
        if (caught instanceof ApiError && caught.status === 412) {
          actionError.value =
            '文件集合已被其他写入修改，本次提交没有写入磁盘；已刷新 ETag，请确认内容后再提交'
          await refreshAfterConflict()
        } else {
          actionError.value = errorMessage(caught)
        }
      } finally {
        busy.value = false
      }
    }

    async function reload(): Promise<void> {
      if (dirty.value && !window.confirm('放弃未保存的修改并重新读取？')) {
        return
      }
      actionError.value = ''
      notice.value = ''
      await load()
    }

    const beforeUnload = (event: BeforeUnloadEvent): void => {
      if (!dirty.value) {
        return
      }
      event.preventDefault()
      event.returnValue = ''
    }

    onMounted(() => {
      window.addEventListener('beforeunload', beforeUnload)
      void load()
    })
    onUnmounted(() => {
      window.removeEventListener('beforeunload', beforeUnload)
    })

    function renderEditor(file: EditableFile): VNode {
      const children: VNodeChild[] = [
        h('div', { class: 'file-path' }, [
          h('code', file.path),
          file.loaded ? null : h('span', { class: 'muted small' }, '（正文无法读取，不参与提交）'),
        ]),
      ]
      if (file.loaded) {
        children.push(
          h('textarea', {
            class: ['editor', 'file-content'],
            spellcheck: false,
            rows: 8,
            value: file.content,
            onInput: (event: Event) => {
              file.content = (event.target as HTMLTextAreaElement).value
            },
          }),
        )
      } else {
        children.push(
          h('p', { class: 'muted' }, '该文件超出预览上限或不是 UTF-8 文本，请通过其他方式修改。'),
        )
      }
      return h('div', { class: 'file-editor', key: file.path }, children)
    }

    return (): VNode => {
      const children: VNodeChild[] = [
        h('header', { class: 'section-head' }, [
          h('h2', '文件'),
          h(
            'p',
            { class: 'muted small' },
            '多文件提交使用受管文件集合 ETag；成功后重新读取磁盘真值，不做乐观更新。',
          ),
        ]),
      ]
      if (loading.value) {
        children.push(h('p', { class: 'muted' }, '文件加载中…'))
      }
      if (loadError.value !== '') {
        children.push(h(Notice, { tone: 'error', text: loadError.value }))
      }
      if (actionError.value !== '') {
        children.push(h(Notice, { tone: 'error', text: actionError.value }))
      }
      if (notice.value !== '') {
        children.push(h(Notice, { tone: 'info', text: notice.value }))
      }
      if (unreadable.value.length > 0) {
        children.push(
          h(Notice, {
            tone: 'warn',
            text: `以下文件无法读取正文，提交时保持原样：${unreadable.value.join('、')}`,
          }),
        )
      }
      children.push(
        h('p', { class: 'muted small' }, [
          '集合 ETag：',
          h('code', { class: 'files-etag' }, etag.value === '' ? '—' : etag.value),
        ]),
      )
      if (!loading.value && files.value.length === 0 && loadError.value === '') {
        children.push(h('p', { class: 'empty' }, '该 Agent 目录下没有受管文件'))
      }
      children.push(
        h(
          'div',
          { class: 'agent-files' },
          files.value.map((file) => renderEditor(file)),
        ),
        h('div', { class: 'form-actions' }, [
          h(
            'button',
            {
              type: 'button',
              class: 'primary',
              disabled: !dirty.value || busy.value,
              onClick: () => {
                void submit()
              },
            },
            busy.value ? '提交中…' : '提交文件',
          ),
          h(
            'button',
            {
              type: 'button',
              disabled: busy.value,
              onClick: () => {
                void reload()
              },
            },
            '重新读取',
          ),
        ]),
      )
      return h('section', { class: 'card agent-files-card' }, children)
    }
  },
})

// US-12 R8 裁决 3 的轮询回归：可见页签每 20 秒重读、进入页签立即刷新、
// 隐藏 / 失活停止、失败退避、事件去重、单飞与卸载清理——全部只读 GET。
// Live refresh semantics: polling, activation refresh, backoff dedupe and cleanup.
import { flushPromises, mount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { defineComponent, h, ref } from 'vue'
import type { Ref } from 'vue'
import { refreshStamp, useLiveLoader } from '../src/views/shared'

type Loader = ReturnType<typeof useLiveLoader<string>>

let visibility: 'visible' | 'hidden' = 'visible'
const mounted: Array<ReturnType<typeof mount>> = []

function mountLoader(
  load: () => Promise<string>,
  options: { intervalMs?: number; active?: Ref<boolean> } = {},
): { wrapper: ReturnType<typeof mount>; loader: Loader } {
  let loader: Loader | null = null
  const Probe = defineComponent({
    setup() {
      loader = useLiveLoader(load, options)
      return () => h('p', { class: 'probe' }, refreshStamp(loader!.lastRefreshedAt.value))
    },
  })
  const wrapper = mount(Probe)
  mounted.push(wrapper)
  return { wrapper, loader: loader! }
}

function setVisibility(next: 'visible' | 'hidden'): void {
  visibility = next
  document.dispatchEvent(new Event('visibilitychange'))
}

beforeEach(() => {
  visibility = 'visible'
  Object.defineProperty(document, 'visibilityState', {
    configurable: true,
    get: () => visibility,
  })
  // 只伪造定时器与时钟：保留真实 setImmediate，flushPromises 才能继续工作。
  // Fake timers and the clock only; real setImmediate keeps flushPromises working.
  vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout', 'Date'] })
  vi.setSystemTime(new Date('2026-10-24T12:00:00Z'))
})

afterEach(() => {
  for (const wrapper of mounted.splice(0)) {
    wrapper.unmount()
  }
  vi.useRealTimers()
  visibility = 'visible'
})

describe('useLiveLoader', () => {
  it('loads on mount, stamps the refresh and polls every 20 seconds', async () => {
    const load = vi.fn(async () => 'value')
    const { wrapper, loader } = mountLoader(load)
    await flushPromises()
    expect(load).toHaveBeenCalledTimes(1)
    expect(loader.lastRefreshedAt.value).not.toBeNull()
    expect(wrapper.find('.probe').text()).toContain('最近刷新')

    await vi.advanceTimersByTimeAsync(19_999)
    expect(load).toHaveBeenCalledTimes(1)
    await vi.advanceTimersByTimeAsync(1)
    expect(load).toHaveBeenCalledTimes(2)
  })

  it('stays idle while inactive and refreshes as soon as the tab activates', async () => {
    const active = ref(false)
    const load = vi.fn(async () => 'value')
    mountLoader(load, { active })
    await flushPromises()
    await vi.advanceTimersByTimeAsync(60_000)
    expect(load).toHaveBeenCalledTimes(0)

    active.value = true
    await flushPromises()
    expect(load).toHaveBeenCalledTimes(1)

    active.value = false
    await flushPromises()
    await vi.advanceTimersByTimeAsync(60_000)
    expect(load).toHaveBeenCalledTimes(1)
  })

  it('pauses while the page is hidden and re-reads once when it becomes visible', async () => {
    const load = vi.fn(async () => 'value')
    mountLoader(load)
    await flushPromises()
    expect(load).toHaveBeenCalledTimes(1)

    setVisibility('hidden')
    await vi.advanceTimersByTimeAsync(60_000)
    expect(load).toHaveBeenCalledTimes(1)

    setVisibility('visible')
    await flushPromises()
    expect(load).toHaveBeenCalledTimes(2)
  })

  it('dedupes visibility and focus events that arrive together', async () => {
    const load = vi.fn(async () => 'value')
    mountLoader(load)
    await flushPromises()
    expect(load).toHaveBeenCalledTimes(1)

    setVisibility('hidden')
    await vi.advanceTimersByTimeAsync(5_000)
    setVisibility('visible')
    window.dispatchEvent(new Event('focus'))
    await flushPromises()
    // 隐藏后重新可见只读一次；紧随其后的 focus 落在 3 秒去重窗口内。
    expect(load).toHaveBeenCalledTimes(2)

    await vi.advanceTimersByTimeAsync(3_000)
    window.dispatchEvent(new Event('focus'))
    await flushPromises()
    expect(load).toHaveBeenCalledTimes(3)
  })

  it('backs off on failures and returns to the base interval after a success', async () => {
    const load = vi.fn<() => Promise<string>>()
      .mockRejectedValueOnce(new Error('offline'))
      .mockRejectedValueOnce(new Error('offline'))
      .mockResolvedValue('value')
    mountLoader(load)
    await flushPromises()
    expect(load).toHaveBeenCalledTimes(1)

    // 首次失败后 20 秒内不重试，下一次在 40 秒。
    await vi.advanceTimersByTimeAsync(39_999)
    expect(load).toHaveBeenCalledTimes(1)
    await vi.advanceTimersByTimeAsync(1)
    expect(load).toHaveBeenCalledTimes(2)

    // 连续失败退避到 80 秒。
    await vi.advanceTimersByTimeAsync(79_999)
    expect(load).toHaveBeenCalledTimes(2)
    await vi.advanceTimersByTimeAsync(1)
    expect(load).toHaveBeenCalledTimes(3)

    // 成功后退避档位归零：下一次回到 20 秒，而不是 160 秒。
    await vi.advanceTimersByTimeAsync(19_999)
    expect(load).toHaveBeenCalledTimes(3)
    await vi.advanceTimersByTimeAsync(1)
    expect(load).toHaveBeenCalledTimes(4)
  })

  it('keeps a single request in flight when refresh is requested twice', async () => {
    const load = vi.fn(async () => 'value')
    const { loader } = mountLoader(load)
    await flushPromises()
    expect(load).toHaveBeenCalledTimes(1)

    void loader.refreshNow()
    void loader.refreshNow()
    await flushPromises()
    expect(load).toHaveBeenCalledTimes(2)
  })

  it('shares the single-flight gate with page reloads and queues one trailing read', async () => {
    let release: (() => void) | null = null
    const load = vi.fn(async () => {
      if (load.mock.calls.length === 1) {
        await new Promise<void>((resolve) => {
          release = resolve
        })
      }
      return 'value'
    })
    const { loader } = mountLoader(load)
    await flushPromises()
    expect(load).toHaveBeenCalledTimes(1)

    // 轮询仍在途：页内写后刷新不得并发，也不能被吞掉。
    void loader.reload()
    await flushPromises()
    expect(load).toHaveBeenCalledTimes(1)

    release!()
    await flushPromises()
    expect(load).toHaveBeenCalledTimes(2)
    expect(loader.lastRefreshedAt.value).not.toBeNull()
  })

  it('stops timers and listeners on unmount', async () => {
    const load = vi.fn(async () => 'value')
    const { wrapper } = mountLoader(load)
    await flushPromises()
    expect(load).toHaveBeenCalledTimes(1)

    wrapper.unmount()
    mounted.length = 0
    await vi.advanceTimersByTimeAsync(120_000)
    window.dispatchEvent(new Event('focus'))
    await flushPromises()
    expect(load).toHaveBeenCalledTimes(1)
  })
})

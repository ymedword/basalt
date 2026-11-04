import { mount } from '@vue/test-utils'
import { describe, expect, it } from 'vitest'
import { defineComponent } from 'vue'
import { renderIsolation, renderIsolationExecutions, isolationSummary } from '../src/views/isolation'
import { isolationFixture } from './isolation-fixture'

describe('US20 read-only isolation', () => {
  it.each(['creating', 'running', 'unknown', 'timeout', 'canceled', 'failed'] as const)('does not invent an exit code for %s', state => {
    const wrapper = mount(defineComponent({ render: () => renderIsolationExecutions([{ tool_invocation_id: 1, tool_call_id: 'call', tool_name: 'shell', profile: 'oci', runtime: 'runc', state, exit_code: 0, cleanup_state: 'pending', error_reason: '', artifact_count: 0, artifact_bytes: 0, started_at: '', finished_at: '' }]) }))
    expect(wrapper.text()).toContain('退出码未知')
    expect(wrapper.text()).not.toContain('退出码 0')
    wrapper.unmount()
  })
  it('shows disabled and empty without claiming service containers isolate tasks', () => {
    const view = isolationFixture()
    const wrapper = mount(defineComponent({ render: () => renderIsolation(view) }))
    expect(wrapper.text()).toContain('本机进程（无容器隔离）')
    expect(wrapper.text()).toContain('run_code 未启用')
    expect(wrapper.text()).toContain('服务部署在容器中不等于任务已被隔离')
    expect(wrapper.findAll('button, input, select, textarea')).toHaveLength(0)
    wrapper.unmount()
    const empty = mount(defineComponent({ render: () => renderIsolationExecutions([]) }))
    expect(empty.text()).toContain('本 Run 没有隔离执行')
    empty.unmount()
  })

  it('keeps explicit bindings, unprobed profiles, long text and restart visible', () => {
    const view = isolationFixture()
    view.enabled = true
    view.backend = 'docker'
    view.tools = { shell: 'local', run_code: 'gvisor' }
    const digest = `sha256:${'a'.repeat(64)}`
    view.profiles = [{ name: 'oci', runtime: 'runc', image_digest: digest, bound: false, available: false, reason: 'not_checked', cpus: 1, memory_mb: 64, pids: 16, tmpfs_mb: 8, timeout_seconds: 30 }]
    expect(isolationSummary(view)).toContain('shell：本机进程（无容器隔离）')
    expect(isolationSummary(view)).toContain('run_code：gVisor')
    const wrapper = mount(defineComponent({ render: () => renderIsolation(view) }))
    expect(wrapper.text()).toContain(digest)
    expect(wrapper.text()).toContain('未探测')
    expect(wrapper.text()).toContain('修改后需重启')
    expect(wrapper.text()).toContain('不提供单任务输出硬配额')
    expect(wrapper.findAll('button, input, select, textarea')).toHaveLength(0)
    wrapper.unmount()
  })

  it('reports cleanup failures independently from a finished execution', () => {
    const wrapper = mount(defineComponent({ render: () => renderIsolationExecutions([{ tool_invocation_id: 1, tool_call_id: 'call-1', tool_name: 'run_code', profile: 'gvisor', runtime: 'runsc', state: 'finished', exit_code: 0, cleanup_state: 'failed', error_reason: 'cleanup_failed', artifact_count: 0, artifact_bytes: 0, started_at: '', finished_at: '' }]) }))
    expect(wrapper.text()).toContain('finished · 退出码 0')
    expect(wrapper.text()).toContain('清理失败：资源可能仍占用')
    expect(wrapper.text()).toContain('0 个 / 0 字节')
    wrapper.unmount()
  })
})

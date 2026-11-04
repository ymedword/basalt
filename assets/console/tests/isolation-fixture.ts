import type { IsolationView } from '../src/api'

export function isolationFixture(): IsolationView {
  return { enabled: false, backend: '', profiles: [], tools: { shell: 'local' }, max_concurrent: 0, max_total_cpus: 0, max_total_memory_mb: 0, max_total_temp_bytes: 0, max_total_artifact_bytes: 0, checked_at: '', unavailable_reason: '' }
}

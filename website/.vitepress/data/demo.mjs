export const DEMO_COMMAND = 'basalt serve'
export const DEMO_LABEL = {
  en: 'Scheduled execution · local scripted replies; the model is a local stub, not a quality benchmark.',
  zh: '定时任务执行 · 本地脚本化回复；模型为本地桩，不代表模型效果。'
}
export function shouldAutoplay(reducedMotion) { return !reducedMotion }
export function visibleLines(transcript, reducedMotion) {
  // 只翻译演示的进度标签；原始 CLI 采集文件和模型回复保持不变。
  // Translate presentation labels only; retain raw CLI evidence and model replies.
  const display = transcript.lines.map(line => line.replace(/msg=调度触发/u, 'msg="scheduled trigger"'))
  const lines = [`$ ${transcript.command}`, ...display]
  return reducedMotion ? lines : lines.slice(0, 1)
}

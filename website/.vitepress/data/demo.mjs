export const DEMO_LABEL = {
  en: 'Local scripted replies · demonstrates the execution path, not model quality.',
  zh: '本地脚本化回复 · 展示执行链路，不代表模型效果。'
}
export function shouldAutoplay(reducedMotion) { return !reducedMotion }
export function visibleLines(transcript, reducedMotion) {
  // 只翻译演示的进度标签；原始 CLI 采集文件和模型回复保持不变。
  // Translate presentation labels only; retain raw CLI evidence and model replies.
  const display = transcript.lines.map(line => line.replace(/^(⚙\s*)调用工具\s+(.+?)(…|\.{3})$/u, '$1Calling tool $2$3'))
  const lines = [`$ ${transcript.command}`, ...display]
  return reducedMotion ? lines : lines.slice(0, 1)
}

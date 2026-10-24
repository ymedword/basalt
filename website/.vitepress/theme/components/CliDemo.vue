<script setup>
import { computed, onMounted, onBeforeUnmount, ref } from 'vue'
import { useData } from 'vitepress'
import { DEMO_LABEL, shouldAutoplay, visibleLines } from '../../data/demo.mjs'
import transcript from '../../../demo/transcript.json'
const { lang } = useData()
const zh = computed(() => lang.value.startsWith('zh'))
const full = visibleLines(transcript, true).join('\n')
const text = ref(full)
const playing = ref(false)
const reduced = ref(false)
const stage = ref(null)
const progress = computed(() => full.length ? Math.min(100, text.value.length / full.length * 100) : 100)
const done = computed(() => text.value.length >= full.length)
let frame = 0
let previous = 0
let elapsed = 0
let observer
let preference
function showResult() { cancelAnimationFrame(frame); text.value = full; playing.value = false }
function tick(now) {
  if (!playing.value) return
  if (previous) elapsed += Math.min(now - previous, 100)
  previous = now
  text.value = full.slice(0, Math.floor(elapsed / 27))
  if (text.value.length >= full.length) showResult()
  else frame = requestAnimationFrame(tick)
}
function play(reset = false) {
  cancelAnimationFrame(frame)
  if (!shouldAutoplay(reduced.value)) { showResult(); return }
  if (reset || done.value) { elapsed = 0; text.value = '' }
  previous = 0
  playing.value = true
  frame = requestAnimationFrame(tick)
}
function toggle() {
  if (playing.value) { playing.value = false; cancelAnimationFrame(frame) }
  else play()
}
function preferenceChanged() { reduced.value = preference.matches; if (reduced.value) showResult() }
onMounted(() => {
  preference = window.matchMedia('(prefers-reduced-motion: reduce)')
  preferenceChanged()
  preference.addEventListener('change', preferenceChanged)
  if (!reduced.value && 'IntersectionObserver' in window) {
    observer = new IntersectionObserver(entries => {
      if (entries.some(entry => entry.isIntersecting)) { play(true); observer.disconnect() }
    }, { threshold: 0.35 })
    observer.observe(stage.value)
  } else if (!reduced.value) play(true)
})
onBeforeUnmount(() => { cancelAnimationFrame(frame); observer?.disconnect(); preference?.removeEventListener('change', preferenceChanged) })
</script>

<template>
  <section ref="stage" class="cli-demo" :aria-label="zh ? 'CLI 演示' : 'CLI demo'">
    <p class="cli-subtitle">{{ zh ? '读取文件，查看摘要。' : 'Read a file. Get a summary.' }}</p>
    <div class="terminal" :data-playing="playing">
      <div class="terminal-title"><span class="terminal-dot" aria-hidden="true"></span>Basalt — CLI</div>
      <pre class="terminal-output" aria-hidden="true"><span v-for="(line, index) in text.split('\n')" :key="index" class="terminal-line"><template v-if="index === 0"><span class="terminal-prompt">{{ line.slice(0, 1) }}</span>{{ line.slice(1) }}</template><template v-else>{{ line }}</template><span v-if="index === text.split('\n').length - 1" class="terminal-cursor"></span></span></pre>
      <pre class="sr-only">{{ full }}</pre>
    </div>
    <div class="cli-controls">
      <button type="button" @click="play(true)">↻ {{ zh ? '重播' : 'Replay' }}</button>
      <div class="cli-progress" aria-hidden="true"><span :style="{ width: `${progress}%` }"></span></div>
      <button v-if="!reduced" type="button" :aria-label="zh ? (playing ? '暂停动画' : '播放动画') : (playing ? 'Pause animation' : 'Play animation')" @click="toggle">{{ zh ? (playing ? '暂停' : '播放') : (playing ? 'Pause' : 'Play') }}</button>
    </div>
    <p class="demo-disclosure">{{ DEMO_LABEL[zh ? 'zh' : 'en'] }}</p>
    <span class="sr-only" role="status" aria-live="polite">{{ done ? (zh ? '演示完成。' : 'Demo complete.') : '' }}</span>
  </section>
</template>

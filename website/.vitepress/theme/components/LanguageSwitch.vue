<script setup>
import { computed, onMounted, onBeforeUnmount, ref, watch } from 'vue'
import { useRoute } from 'vitepress'
import { switchLocalePath } from '../../data/navigation.mjs'
const route = useRoute()
const current = ref(route.path)
const sync = () => { current.value = window.location.pathname + window.location.search + window.location.hash }
const zh = computed(() => /(?:^|\/)zh(?:\/|$)/.test(current.value))
watch(() => route.path, () => { current.value = route.path; if (typeof window !== 'undefined') sync() })
onMounted(() => { sync(); window.addEventListener('hashchange', sync); window.addEventListener('popstate', sync) })
onBeforeUnmount(() => { window.removeEventListener('hashchange', sync); window.removeEventListener('popstate', sync) })
</script>

<template>
  <nav class="language-switch" :aria-label="zh ? '语言' : 'Language'">
    <a :href="switchLocalePath(current, 'en')" lang="en" hreflang="en" :aria-current="!zh ? 'true' : undefined">EN</a>
    <span aria-hidden="true">/</span>
    <a :href="switchLocalePath(current, 'zh')" lang="zh-CN" hreflang="zh-CN" :aria-current="zh ? 'true' : undefined">中文</a>
  </nav>
</template>

<script setup>
import { computed } from 'vue'
import { useData, withBase } from 'vitepress'
import { localePath, resolveFooter } from '../../data/navigation.mjs'
import LanguageSwitch from './LanguageSwitch.vue'
const { lang } = useData()
const locale = computed(() => lang.value.startsWith('zh') ? 'zh' : 'en')
const external = link => /^https:/.test(link)
</script>

<template>
  <footer class="site-footer">
    <div class="footer-inner">
      <div class="footer-watermark" aria-hidden="true">BASALT</div>
      <div class="footer-columns">
        <div class="footer-brand">
          <a :href="withBase(localePath(locale, 'home'))" :aria-label="locale === 'zh' ? 'Basalt 首页' : 'Basalt home'"><img :src="withBase('/brand/basalt-logo.svg')" alt="Basalt" width="174" height="38"></a>
          <p>{{ locale === 'zh' ? '自托管 Agent OS。' : 'Self-hosted Agent OS.' }}</p>
        </div>
        <div v-for="group in resolveFooter(locale)" :key="group.title" class="footer-group">
          <h2>{{ group.title }}</h2>
          <a v-for="item in group.links" :key="item.link" :href="external(item.link) ? item.link : withBase(item.link)" :target="external(item.link) ? '_blank' : undefined" :rel="external(item.link) ? 'noopener noreferrer' : undefined">{{ item.text }}</a>
        </div>
      </div>
      <div class="footer-bottom"><span>BASALT / AGENT OS</span><LanguageSwitch /></div>
    </div>
  </footer>
</template>

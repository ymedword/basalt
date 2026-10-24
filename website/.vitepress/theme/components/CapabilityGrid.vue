<script setup>
import { computed } from 'vue'
import { useData, withBase } from 'vitepress'
import { CAPABILITIES } from '../../data/capabilities.mjs'
const { lang } = useData()
const zh = computed(() => lang.value.startsWith('zh'))
</script>

<template>
  <section class="platform-section" aria-labelledby="platform-title">
    <h2 id="platform-title">{{ zh ? '平台能力' : 'Platform capabilities.' }}</h2>
    <div class="capability-grid">
      <article v-for="(item, index) in CAPABILITIES" :key="item.id" class="capability">
        <p class="capability-tag">{{ String(index + 1).padStart(2, '0') }} <span>{{ zh ? item.tagZh : item.tagEn }}</span></p>
        <h3>{{ zh ? item.titleZh : item.titleEn }}</h3>
        <p class="capability-body">{{ zh ? item.bodyZh : item.bodyEn }}</p>
        <img :src="withBase(item.illustration)" :alt="zh ? item.altZh : item.altEn" width="400" height="320" loading="lazy">
      </article>
    </div>
  </section>
</template>

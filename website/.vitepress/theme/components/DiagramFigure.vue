<script setup>
import { computed } from 'vue'
import { useData, withBase } from 'vitepress'
import { DIAGRAMS } from '../../data/capabilities.mjs'
import { REPO_URL } from '../../data/navigation.mjs'
const props = defineProps({ id: { type: String, required: true } })
const { lang } = useData()
const zh = computed(() => lang.value.startsWith('zh'))
const item = computed(() => DIAGRAMS.find(diagram => diagram.id === props.id))
</script>

<template>
  <figure v-if="item" class="diagram-figure">
    <a :href="withBase(`/diagrams/${zh ? 'zh' : 'en'}/${id}.svg`)" target="_blank" rel="noopener noreferrer"><img :src="withBase(`/diagrams/${zh ? 'zh' : 'en'}/${id}.svg`)" :alt="zh ? item.titleZh : item.titleEn" :width="item.width ?? 1200" :height="item.height ?? 760" loading="lazy"></a>
    <figcaption>{{ zh ? item.descriptionZh : item.descriptionEn }} <a :href="withBase(`/diagrams/${zh ? 'zh' : 'en'}/${id}.svg`)" target="_blank" rel="noopener noreferrer">{{ zh ? '查看大图' : 'Full-size view' }} ↗</a> · <a :href="`${REPO_URL}/blob/main/website/diagrams/basalt-diagrams.drawio`" target="_blank" rel="noopener noreferrer">{{ zh ? '可编辑图源' : 'Editable source' }} ↗</a></figcaption>
  </figure>
</template>

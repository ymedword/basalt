<script setup>
// 模板 / Demo / MCP 目录表格：数据取自 data/templates.mjs，中英标签随站点语言切换。
// Tables for templates, demos and the MCP catalog. Data comes from data/templates.mjs and
// the labels follow the site locale, so both pages render the same facts.
import { computed } from 'vue'
import { useData } from 'vitepress'
import { CATALOG, DEMOS, TEMPLATES, TIERS, TABLE_LABELS } from '../../data/templates.mjs'

defineProps({ section: { type: String, required: true } })
const { lang } = useData()
const zh = computed(() => lang.value.startsWith('zh'))
const labels = computed(() => (zh.value ? TABLE_LABELS.zh : TABLE_LABELS.en))
// 目录里的 path 带尾斜杠（供测试做目录存在性核对），仓库链接不能带。
// Stored paths keep a trailing slash for directory checks; repository links must not.
const repoLink = item => `https://github.com/ymedword/basalt/tree/main/${item.path.replace(/\/+$/, '')}`
</script>

<template>
  <div class="template-table">
    <table v-if="section === 'demos'">
      <thead>
        <tr>
          <th scope="col">{{ labels.demo }}</th>
          <th scope="col">{{ labels.produces }}</th>
          <th scope="col">{{ labels.needs }}</th>
          <th scope="col">{{ labels.evidence }}</th>
        </tr>
      </thead>
      <tbody>
        <tr v-for="item in DEMOS" :key="item.id">
          <td><a :href="repoLink(item)">{{ zh ? item.titleZh : item.titleEn }}</a></td>
          <td>{{ zh ? item.bodyZh : item.bodyEn }}</td>
          <td>{{ zh ? item.needsZh : item.needsEn }}</td>
          <td>{{ zh ? item.evidenceZh : item.evidenceEn }}</td>
        </tr>
      </tbody>
    </table>

    <template v-else-if="section === 'templates'">
      <table>
        <thead>
          <tr>
            <th scope="col">{{ labels.template }}</th>
            <th scope="col">{{ labels.domain }}</th>
            <th scope="col">{{ labels.tier }}</th>
            <th scope="col">{{ labels.needs }}</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="item in TEMPLATES" :key="item.id">
            <td><a :href="repoLink(item)">{{ zh ? item.titleZh : item.titleEn }}</a></td>
            <td>{{ zh ? item.domainZh : item.domainEn }}</td>
            <td>{{ item.tier }}</td>
            <td>{{ zh ? item.needsZh : item.needsEn }}</td>
          </tr>
        </tbody>
      </table>
      <ul class="template-tiers">
        <li v-for="item in TIERS" :key="item.tier"><strong>{{ item.tier }}</strong> — {{ zh ? item.bodyZh : item.bodyEn }}</li>
      </ul>
    </template>

    <template v-else>
      <table>
        <thead>
          <tr>
            <th scope="col">{{ labels.catalog }}</th>
            <th scope="col">{{ labels.summary }}</th>
            <th scope="col">{{ labels.needs }}</th>
            <th scope="col">{{ labels.limit }}</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="item in CATALOG" :key="item.id">
            <td>{{ zh ? item.nameZh : item.nameEn }}</td>
            <td>{{ zh ? item.summaryZh : item.summaryEn }}</td>
            <td>{{ zh ? item.needsZh : item.needsEn }}</td>
            <td>{{ zh ? item.limitZh : item.limitEn }}</td>
          </tr>
        </tbody>
      </table>
      <p class="template-note">{{ labels.status }}</p>
    </template>
  </div>
</template>

<style scoped>
.template-tiers {
  font-size: 0.9em;
}

.template-note {
  color: var(--vp-c-text-2);
  font-size: 0.9em;
}
</style>

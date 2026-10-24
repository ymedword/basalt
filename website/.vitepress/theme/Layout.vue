<script setup>
import DefaultTheme from 'vitepress/theme'
import LanguageSwitch from './components/LanguageSwitch.vue'
import SiteFooter from './components/SiteFooter.vue'
const { Layout } = DefaultTheme
</script>

<template>
  <Layout>
    <template #nav-bar-content-after><LanguageSwitch /></template>
    <template #nav-screen-menu-after><LanguageSwitch /></template>
    <template #layout-bottom><SiteFooter /></template>
  </Layout>
</template>

import DefaultTheme from 'vitepress/theme'
import Layout from './Layout.vue'
import HomeHero from './components/HomeHero.vue'
import CapabilityGrid from './components/CapabilityGrid.vue'
import CliDemo from './components/CliDemo.vue'
import DiagramFigure from './components/DiagramFigure.vue'
import './custom.css'

export default {
  extends: DefaultTheme,
  Layout,
  enhanceApp({ app }) {
    app.component('HomeHero', HomeHero)
    app.component('CapabilityGrid', CapabilityGrid)
    app.component('CliDemo', CliDemo)
    app.component('DiagramFigure', DiagramFigure)
  }
}

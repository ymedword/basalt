// 管理台入口：创建根组件并挂载到 #app（装配细节见 app.ts）。
// Console entry: mounts the root component built by app.ts.
import { createApp } from 'vue'
import { createConsoleApp } from './app'
// 样式经 Vite 打包为 dist/assets/*.css，由构建产物统一嵌入（无 CDN、无外链字体）。
// The stylesheet is bundled by Vite into dist/assets and shipped with the binary.
import './styles.css'
import './workspace.css'

createApp(createConsoleApp()).mount('#app')

import test from 'node:test'
import assert from 'node:assert/strict'
import { readFile } from 'node:fs/promises'
function luminance(hex) {
  const channels = hex.match(/[a-f\d]{2}/gi).map(value => parseInt(value, 16) / 255).map(value => value <= 0.04045 ? value / 12.92 : ((value + 0.055) / 1.055) ** 2.4)
  return channels[0] * 0.2126 + channels[1] * 0.7152 + channels[2] * 0.0722
}
test('actual CSS text tokens pass AA and focus has a visible color', async () => {
  const css = await readFile(new URL('../.vitepress/theme/custom.css', import.meta.url), 'utf8')
  const tokens = Object.fromEntries([...css.matchAll(/--basalt-([\w-]+):\s*(#[a-f\d]{6})/gi)].map(match => [match[1], match[2]]))
  for (const [foreground, background] of [['ink', 'paper'], ['muted', 'paper'], ['footer-text', 'footer']]) {
    const values = [luminance(tokens[foreground]), luminance(tokens[background])].sort((a, b) => b - a)
    const ratio = (values[0] + 0.05) / (values[1] + 0.05)
    assert.ok(ratio >= 4.5, `${foreground}/${background}: ${ratio}, expected >= 4.5`)
  }
  assert.ok(tokens.focus)
  assert.match(css, /:focus-visible/)
  assert.match(css, /prefers-reduced-motion/)
})

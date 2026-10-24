// AGENT.md 围栏正文的客户端切分：与 internal/workspace 的 splitFrontmatter 同口径
// （容忍 BOM；首个 `---` 行之后到下一个 `---` 行为 frontmatter；其后的全部字节为正文，
// 两侧的 `\r` / `\n` 去掉，围栏之间的空行不属于正文）。只在页面读取态使用——保存
// 仍由服务端的补丁实现写回，客户端不重新序列化整份文件。
// Client-side body split matching internal/workspace's splitFrontmatter: BOM tolerated,
// frontmatter ends at the next `---` line, and the body is everything after it with
// surrounding CR/LF trimmed. The page never re-serialises the whole file.

/** 切分结果：`ok=false` 表示围栏结构无法解析（此时页面禁用正文编辑）。 */
export interface AgentBodySplit {
  readonly body: string
  readonly ok: boolean
}

/** 把 AGENT.md 原文切成围栏外正文；结构不可解析时返回空正文与 ok=false。 */
export function splitAgentBody(raw: string): AgentBodySplit {
  const text = raw.startsWith(String.fromCharCode(0xfeff)) ? raw.slice(1) : raw
  if (!text.startsWith('---\n') && !text.startsWith('---\r\n')) {
    return { body: '', ok: false }
  }
  const lines = text.slice(text.indexOf('\n') + 1).split('\n')
  for (let index = 0; index < lines.length; index += 1) {
    // 与后端口径一致：行尾的 `\r` 先去掉再比较，Windows 换行同样识别为围栏。
    if (lines[index]!.replace(/\r$/, '') !== '---') {
      continue
    }
    const remainder = lines.slice(index + 1).join('\n')
    // Go 的 strings.Trim(s, "\r\n") 去掉两端任意组合的 CR / LF。
    return { body: remainder.replace(/^[\r\n]+/, '').replace(/[\r\n]+$/, ''), ok: true }
  }
  return { body: '', ok: false }
}

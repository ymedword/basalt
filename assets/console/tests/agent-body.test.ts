// 正文切分回归：与 internal/workspace 的 splitFrontmatter 同口径——BOM 容忍、
// LF / CRLF 围栏、缺围栏或少围栏一律 ok=false、正文只去掉两端 CR/LF、内部空行保留。
// splitAgentBody regressions: BOM tolerated, LF/CRLF fences, a missing fence is
// unparsable, only surrounding CR/LF are trimmed and inner blank lines survive.
import { describe, expect, it } from 'vitest'
import { splitAgentBody } from '../src/agent-body'

describe('splitAgentBody', () => {
  it('returns the text after the closing fence without its surrounding newlines', () => {
    expect(splitAgentBody('---\nname: writer\n---\n\n你是报告作者。\n第二行\n\n')).toEqual({
      body: '你是报告作者。\n第二行',
      ok: true,
    })
  })

  it('keeps blank lines inside the body', () => {
    expect(splitAgentBody('---\n---\n第一段\n\n第二段').body).toBe('第一段\n\n第二段')
  })

  it('tolerates a BOM and CRLF fences', () => {
    const raw = `﻿---\r\nname: writer\r\n---\r\n第一行\r\n第二行\r\n`
    expect(splitAgentBody(raw)).toEqual({ body: '第一行\r\n第二行', ok: true })
  })

  it('reports ok=false when the opening fence is missing', () => {
    expect(splitAgentBody('name: writer\n---\n正文')).toEqual({ body: '', ok: false })
  })

  it('reports ok=false when the closing fence is missing', () => {
    expect(splitAgentBody('---\nname: writer\n正文\n')).toEqual({ body: '', ok: false })
  })

  it('returns an empty body when nothing follows the fences', () => {
    expect(splitAgentBody('---\nname: writer\n---\n')).toEqual({ body: '', ok: true })
  })
})

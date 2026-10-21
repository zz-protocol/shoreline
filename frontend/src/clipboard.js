/**
 * 复制文本：先用 Clipboard API（仅安全上下文可用），失败退回 textarea + execCommand('copy')；
 * 都失败返回 false，由调用方弹出文本框让用户手动复制（HTTP 内网地址场景）。
 * 返回 'clipboard' | 'execCommand' | false。
 */
export async function copyText(text, nav = globalThis.navigator, doc = globalThis.document) {
  try {
    if (nav?.clipboard?.writeText) {
      await nav.clipboard.writeText(text)
      return 'clipboard'
    }
  } catch {
    /* 回退 */
  }
  try {
    const ta = doc.createElement('textarea')
    ta.value = text
    ta.setAttribute('readonly', '')
    ta.style.position = 'fixed'
    ta.style.top = '-1000px'
    doc.body.appendChild(ta)
    ta.select()
    let ok = false
    try {
      ok = doc.execCommand('copy')
    } finally {
      doc.body.removeChild(ta)
    }
    if (ok) return 'execCommand'
  } catch {
    /* 回退 */
  }
  return false
}

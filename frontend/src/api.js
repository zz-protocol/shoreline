const TOKEN_KEY = 'sam_api_token'

export function getToken() {
  return localStorage.getItem(TOKEN_KEY) || ''
}

export function setToken(t) {
  if (t) localStorage.setItem(TOKEN_KEY, t)
  else localStorage.removeItem(TOKEN_KEY)
}

export async function api(method, path, body, opts = {}) {
  const headers = { ...(opts.headers || {}) }
  const tok = opts.token ?? getToken()
  if (tok) headers.Authorization = `Bearer ${tok}`
  let payload = body
  if (body != null && !(body instanceof Blob) && headers['Content-Type'] !== 'text/csv') {
    headers['Content-Type'] = 'application/json'
    payload = JSON.stringify(body)
  }
  const res = await fetch(path, { method, headers, body: payload })
  if (res.status === 204) return null
  const text = await res.text()
  let data = null
  try {
    data = text ? JSON.parse(text) : null
  } catch {
    data = { error: text || res.statusText }
  }
  if (!res.ok) {
    const err = new Error((data && data.error) || res.statusText || String(res.status))
    err.status = res.status
    err.data = data
    throw err
  }
  return data
}

export const get = (p, o) => api('GET', p, null, o)
export const post = (p, b, o) => api('POST', p, b, o)
export const patch = (p, b, o) => api('PATCH', p, b, o)
export const del = (p, o) => api('DELETE', p, null, o)

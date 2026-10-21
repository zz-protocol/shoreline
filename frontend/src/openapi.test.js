import { describe, it, expect } from 'vitest'
import { readFileSync } from 'fs'
import { resolve } from 'path'
import yaml from 'js-yaml'

// swagger-ui 内置 js-yaml 解析 spec；原报错 "missed comma between flow collection entries (55:68)" 即来自它。
const spec = readFileSync(resolve(__dirname, '../../backend/openapi.yaml'), 'utf8')

describe('openapi.yaml (js-yaml, same parser as swagger-ui)', () => {
  it('parses with openapi version and 28 operations', () => {
    const doc = yaml.load(spec)
    expect(doc.openapi).toBe('3.0.3')
    const ops = Object.values(doc.paths).flatMap((p) => Object.keys(p).filter((m) => ['get', 'post', 'patch', 'delete', 'put'].includes(m)))
    expect(ops.length).toBe(28)
    expect(doc.components.schemas.ScopeAccepted.properties.task_id.description).toBe('GET /api/scope-tasks/{id} 查询 in_scope 重算进度')
  })

  it('reproduces the original error for an unquoted {id} in a flow mapping', () => {
    const broken = 'a: {type: integer, description: GET /api/scope-tasks/{id} 查询}'
    expect(() => yaml.load(broken)).toThrow(/missed comma between flow collection entries/)
  })
})

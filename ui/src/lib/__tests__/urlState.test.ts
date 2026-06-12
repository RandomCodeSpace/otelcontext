import { describe, expect, it } from 'vitest'
import { buildHref, readParam } from '../urlState'

describe('readParam', () => {
  it('reads a parameter from a search string without "?"', () => {
    expect(readParam('service=checkout&trail=svc:a', 'service')).toBe('checkout')
  })

  it('tolerates a leading "?"', () => {
    expect(readParam('?service=checkout', 'service')).toBe('checkout')
  })

  it('returns null for absent keys and empty values', () => {
    expect(readParam('a=1', 'service')).toBeNull()
    expect(readParam('service=', 'service')).toBeNull()
    expect(readParam('', 'service')).toBeNull()
  })

  it('decodes encoded values', () => {
    expect(readParam('service=a%2Fb', 'service')).toBe('a/b')
  })
})

describe('buildHref', () => {
  it('sets new params while preserving unrelated ones', () => {
    expect(buildHref('/map', 'tab=deps', { service: 'checkout' })).toBe(
      '/map?tab=deps&service=checkout',
    )
  })

  it('removes params set to null', () => {
    expect(buildHref('/map', 'service=checkout&trail=svc:a', { service: null })).toBe(
      '/map?trail=svc%3Aa',
    )
  })

  it('returns the bare path when no params remain', () => {
    expect(buildHref('/map', 'service=x', { service: null })).toBe('/map')
  })

  it('overwrites existing values', () => {
    expect(buildHref('/', 'service=a', { service: 'b' })).toBe('/?service=b')
  })
})

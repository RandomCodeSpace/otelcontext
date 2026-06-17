import { describe, expect, it, vi } from 'vitest'
import { fireEvent, render } from '@testing-library/react'
import { Router } from 'wouter'
import { memoryLocation } from 'wouter/memory-location'
import { useGlobalKeys, type GlobalKeyHandlers } from '../useGlobalKeys'

function Harness(handlers: Readonly<GlobalKeyHandlers>) {
  useGlobalKeys(handlers)
  return <input aria-label="field" />
}

function setup(path = '/') {
  const memory = memoryLocation({ path, record: true })
  const onPalette = vi.fn()
  const onShortcuts = vi.fn()
  const utils = render(
    <Router hook={memory.hook} searchHook={memory.searchHook}>
      <Harness onPalette={onPalette} onShortcuts={onShortcuts} />
    </Router>,
  )
  return { memory, onPalette, onShortcuts, ...utils }
}

describe('useGlobalKeys', () => {
  it('⌘K reaches the palette handler', () => {
    const { onPalette } = setup()
    fireEvent.keyDown(window, { key: 'k', metaKey: true })
    expect(onPalette).toHaveBeenCalledTimes(1)
  })

  it('Ctrl+K works from inside an input', () => {
    const { onPalette, getByLabelText } = setup()
    fireEvent.keyDown(getByLabelText('field'), { key: 'k', ctrlKey: true })
    expect(onPalette).toHaveBeenCalledTimes(1)
  })

  it('? opens shortcuts, except while typing', () => {
    const { onShortcuts, getByLabelText } = setup()
    fireEvent.keyDown(getByLabelText('field'), { key: '?' })
    expect(onShortcuts).not.toHaveBeenCalled()
    fireEvent.keyDown(window, { key: '?' })
    expect(onShortcuts).toHaveBeenCalledTimes(1)
  })

  it('g m navigates to /map; g while typing does not', () => {
    const { memory, getByLabelText } = setup('/')
    fireEvent.keyDown(getByLabelText('field'), { key: 'g' })
    fireEvent.keyDown(getByLabelText('field'), { key: 'm' })
    expect(memory.history.at(-1)).toBe('/')

    fireEvent.keyDown(window, { key: 'g' })
    fireEvent.keyDown(window, { key: 'm' })
    expect(memory.history.at(-1)).toBe('/map')
  })

  it('the listener unbinds on unmount', () => {
    const { onPalette, unmount } = setup()
    unmount()
    fireEvent.keyDown(window, { key: 'k', metaKey: true })
    expect(onPalette).not.toHaveBeenCalled()
  })
})

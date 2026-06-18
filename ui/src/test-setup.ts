import '@testing-library/jest-dom/vitest';

// jsdom lacks a handful of platform APIs that Radix primitives touch.
// Guarded stubs — real implementations win if jsdom ever grows them.
if (!Element.prototype.hasPointerCapture) {
  Element.prototype.hasPointerCapture = () => false;
}
if (!Element.prototype.setPointerCapture) {
  Element.prototype.setPointerCapture = () => {};
}
if (!Element.prototype.releasePointerCapture) {
  Element.prototype.releasePointerCapture = () => {};
}
if (!Element.prototype.scrollIntoView) {
  Element.prototype.scrollIntoView = () => {};
}
// jsdom has no matchMedia; useMediaQuery needs the listener pair. Tests that
// exercise a specific breakpoint stub window.matchMedia themselves.
if (typeof window !== 'undefined' && typeof window.matchMedia !== 'function') {
  window.matchMedia = (query: string): MediaQueryList =>
    ({
      matches: false,
      media: query,
      onchange: null,
      addListener: () => {},
      removeListener: () => {},
      addEventListener: () => {},
      removeEventListener: () => {},
      dispatchEvent: () => false,
    }) as MediaQueryList;
}
if (!('ResizeObserver' in globalThis)) {
  class ResizeObserverStub {
    observe() {}
    unobserve() {}
    disconnect() {}
  }
  (globalThis as Record<string, unknown>).ResizeObserver = ResizeObserverStub;
}

// --- React Flow (@xyflow/react) jsdom shims ---
// React Flow measures its pane + nodes through offset sizes and parses the
// viewport transform via DOMMatrixReadOnly; jsdom provides neither, so nodes
// won't render without these. Canonical "mockReactFlow" setup.
if (!('DOMMatrixReadOnly' in globalThis)) {
  class DOMMatrixReadOnlyStub {
    m22: number;
    constructor(transform?: string) {
      const scale = transform?.match(/scale\(([^)]+)\)/);
      this.m22 = scale ? parseFloat(scale[1]) : 1;
    }
  }
  (globalThis as Record<string, unknown>).DOMMatrixReadOnly = DOMMatrixReadOnlyStub;
}
if (!Object.getOwnPropertyDescriptor(HTMLElement.prototype, 'offsetWidth')?.get) {
  Object.defineProperties(HTMLElement.prototype, {
    offsetWidth: {
      get(this: HTMLElement) {
        return parseFloat(this.style?.width) || 800;
      },
    },
    offsetHeight: {
      get(this: HTMLElement) {
        return parseFloat(this.style?.height) || 600;
      },
    },
  });
}
{
  const svgProto = typeof SVGElement !== 'undefined'
    ? (SVGElement.prototype as unknown as { getBBox?: () => DOMRect })
    : null;
  if (svgProto && !svgProto.getBBox) {
    svgProto.getBBox = () => ({ x: 0, y: 0, width: 0, height: 0 }) as DOMRect;
  }
}

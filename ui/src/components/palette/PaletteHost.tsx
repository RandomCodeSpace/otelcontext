import CommandPalette from './CommandPalette'
import ShortcutSheet from './ShortcutSheet'

// The lazy overlay chunk: ⌘K palette + '?' shortcut sheet. App mounts this
// only after the first open request, so cmdk and both dialogs cost zero
// bytes on the initial route load.

interface PaletteHostProps {
  paletteOpen: boolean
  onPaletteOpenChange: (open: boolean) => void
  shortcutsOpen: boolean
  onShortcutsOpenChange: (open: boolean) => void
  onToggleTheme: () => void
}

export default function PaletteHost({
  paletteOpen,
  onPaletteOpenChange,
  shortcutsOpen,
  onShortcutsOpenChange,
  onToggleTheme,
}: Readonly<PaletteHostProps>) {
  return (
    <>
      <CommandPalette
        open={paletteOpen}
        onOpenChange={onPaletteOpenChange}
        onToggleTheme={onToggleTheme}
      />
      <ShortcutSheet open={shortcutsOpen} onOpenChange={onShortcutsOpenChange} />
    </>
  )
}

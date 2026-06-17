import * as Dialog from '@radix-ui/react-dialog'
import { X } from 'lucide-react'
import styles from './palette.module.css'

// '?' shortcut sheet — a static Radix dialog listing every global and
// map-local key. Ships in the lazy palette chunk (PaletteHost).

const GROUPS: ReadonlyArray<{
  title: string
  keys: ReadonlyArray<[string, string]>
}> = [
  {
    title: 'Global',
    keys: [
      ['⌘K / Ctrl+K', 'Command palette'],
      ['g h · g m', 'Go to Triage / Map'],
      ['/', 'Focus the page filter'],
      ['⌫', 'Pop the investigation trail'],
      ['?', 'This sheet'],
    ],
  },
  {
    title: 'Flow map',
    keys: [
      ['← → ↑ ↓', 'Walk callers / callees / siblings'],
      ['Enter', 'Open the inspector'],
      ['f', 'Fit graph to view'],
      ['Esc', 'Clear selection'],
    ],
  },
]

interface ShortcutSheetProps {
  open: boolean
  onOpenChange: (open: boolean) => void
}

export default function ShortcutSheet({
  open,
  onOpenChange,
}: Readonly<ShortcutSheetProps>) {
  return (
    <Dialog.Root open={open} onOpenChange={onOpenChange}>
      <Dialog.Portal>
        <Dialog.Overlay className={styles.overlay} />
        <Dialog.Content className={styles.sheetContent} aria-describedby={undefined}>
          <header className={styles.sheetHeader}>
            <Dialog.Title className={styles.sheetTitle}>
              Keyboard shortcuts
            </Dialog.Title>
            <Dialog.Close asChild>
              <button
                type="button"
                className={styles.sheetClose}
                aria-label="Close shortcuts"
              >
                <X size={15} aria-hidden="true" />
              </button>
            </Dialog.Close>
          </header>
          {GROUPS.map((group) => (
            <section key={group.title} aria-label={group.title}>
              <h3 className={styles.sheetGroupTitle}>{group.title}</h3>
              <dl className={styles.shortcutList}>
                {group.keys.map(([keys, what]) => (
                  <div key={keys} className={styles.shortcutRow}>
                    <dt>
                      <kbd className={styles.kbd}>{keys}</kbd>
                    </dt>
                    <dd className={styles.shortcutWhat}>{what}</dd>
                  </div>
                ))}
              </dl>
            </section>
          ))}
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  )
}

import js from '@eslint/js'
import globals from 'globals'
import tseslint from 'typescript-eslint'
import reactHooks from 'eslint-plugin-react-hooks'
import reactRefresh from 'eslint-plugin-react-refresh'

export default tseslint.config(
  { ignores: ['dist', 'node_modules'] },
  js.configs.recommended,
  ...tseslint.configs.recommended,
  {
    // Build/CI scripts run under Node, not the browser.
    files: ['scripts/**/*.mjs'],
    languageOptions: {
      globals: globals.node,
    },
  },
  {
    files: ['**/*.{ts,tsx}'],
    languageOptions: {
      ecmaVersion: 2022,
      globals: globals.browser,
    },
    plugins: {
      'react-hooks': reactHooks,
      'react-refresh': reactRefresh,
    },
    rules: {
      ...reactHooks.configs.recommended.rules,
      'react-refresh/only-export-components': [
        'warn',
        { allowConstantExport: true },
      ],
      // The two react-compiler-era rules below fire on legitimate, codebase-wide
      // patterns rather than real defects, so they are warnings (visible) not
      // errors:
      //  - set-state-in-effect: the async data-fetch hooks setState AFTER an
      //    await (the pre-existing useSystemGraph/useDashboard do the same); the
      //    rule can't follow the await boundary.
      //  - refs: the intentional state->ref mirror in useWebSocket and the
      //    imperative cytoscape hover-overlay positioning in ServiceGraph.
      // rules-of-hooks and exhaustive-deps stay at their recommended levels.
      'react-hooks/set-state-in-effect': 'warn',
      'react-hooks/refs': 'warn',
    },
  },
)

// cytoscape-cose-bilkent ships no type declarations. Standalone ambient module
// declaration (kept out of any importing module so TS treats it as a fresh
// declaration, not an augmentation of an untyped module). The default export is
// a cytoscape extension registrar consumed only by cytoscape.use().
declare module 'cytoscape-cose-bilkent' {
  import type cytoscape from 'cytoscape'
  const ext: cytoscape.Ext
  export default ext
}

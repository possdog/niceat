import { mount } from 'svelte'
// The package's exports map resolves "." to a stylesheet, which TypeScript
// cannot type; the explicit minified CSS path is the same file the docs use
// from a CDN and is covered by vite/client's `*.css` declarations.
import '@anyblades/blades/css/blades.min.css'
// After Blades so overrides win at equal specificity; vite-plugin-singlefile
// inlines this into the same single index.html, so no external asset is emitted.
import './custom.css'
import App from './App.svelte'

const app = mount(App, {
  target: document.getElementById('app')!,
})

export default app

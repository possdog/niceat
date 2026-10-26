import { mount } from 'svelte'
import 'bulma/css/bulma.min.css'
// After Bulma so overrides win at equal specificity; vite-plugin-singlefile
// inlines this into the same single index.html, so no external asset is emitted.
import './custom.css'
import App from './App.svelte'

const app = mount(App, {
  target: document.getElementById('app')!,
})

export default app

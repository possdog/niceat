<script lang="ts">
  // The domain the user is browsing on IS the registration target: the form
  // posts same-origin, so the request's Host header carries the domain
  // selection.  No server-side markup parameterization is needed.
  const domain = location.hostname

  // bsky.app refuses custom-domain handles longer than 30 characters in total
  // (see AGENTS.md, "Composed handle"), so the label budget is whatever the
  // served domain leaves over.  Bounding the input to it also keeps the
  // composed handle inside the field instead of scrolling out of it, which
  // matters because the field's visible text is the full composed handle.
  const maxLabel = Math.max(3, 30 - `.${domain}`.length)

  let label = $state('')
  let did = $state('')
  let submitting = $state(false)
  // $state.raw: reassigned wholesale, never mutated, so no proxy is needed.
  let result = $state.raw<{ ok: boolean; message: string } | null>(null)

  async function submit(event: SubmitEvent) {
    event.preventDefault()
    if (submitting) return
    submitting = true
    result = null
    try {
      const res = await fetch('/register', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ handle: label, did }),
      })
      const body = (await res.json()) as {
        status?: string
        handle?: string
        detail?: string
      }
      if (res.ok) {
        const handle = body.handle ?? `${label}.${domain}`
        // The DNS record now (or already) points at the account, but the
        // link is only established once the account adopts the handle
        // in-app; tell the user that final step explicitly.
        result =
          body.status === 'created'
            ? {
                ok: true,
                message: `@${handle} is registered. To activate it, open Bluesky's Settings → Change handle → "I have my own domain", and enter ${handle}.`,
              }
            : {
                ok: true,
                message: `@${handle} was already registered to your DID. If that's you, activate it via Settings → Change handle in the Bluesky app.`,
              }
      } else {
        // 4xx details are written for display to the end user; anything
        // else falls back to a generic line.
        result = {
          ok: false,
          message:
            res.status < 500 && body.detail
              ? body.detail
              : 'Something went wrong; please try again.',
        }
      }
    } catch {
      result = {
        ok: false,
        message: 'Could not reach the server; please try again.',
      }
    } finally {
      submitting = false
    }
  }
</script>

<!-- Blades is classless: every element below is styled from its tag, role
     and ARIA state, so `container narrow` (see custom.css) is the only
     styling hook the page needs. -->
<main class="container narrow">
  <hgroup>
    <h1>Claim your handle</h1>
    <p>a Bluesky handle under <strong>{domain}</strong></p>
  </hgroup>

  <p>
    Registering creates the DNS record that binds the handle to your Bluesky
    account. It's free, instant, and yours until you stop using it.
  </p>

  <form onsubmit={submit}>
    <label for="handle">Handle</label>
    <!-- The field shows the handle as Bluesky will: label plus domain, as one
         string.  Only the label is editable, so the input carries it alone and
         its own glyphs are transparent — `.handle-fill` draws `label.domain`
         underneath, which leaves the caret, selection and typing inside the
         label and makes the domain unreachable with the cursor. -->
    <div class="handle-field">
      <input
        id="handle"
        type="text"
        maxlength={maxLabel}
        autocomplete="off"
        spellcheck="false"
        required
        bind:value={label}
        aria-describedby="handle-hint"
      />
      <!-- One line on purpose: the overlay's text must contain no whitespace
           between the label and the suffix, or the field would show a gap.
           While the field is empty the `{#if}` swaps in a sample name, so an
           untouched field still reads as a whole handle. -->
      <span class="handle-fill" aria-hidden="true">{#if label}{label}{:else}<span class="handle-placeholder">yourname</span>{/if}<span class="handle-suffix">.{domain}</span></span>
    </div>
    <small id="handle-hint">
      3–{maxLabel} characters: lowercase letters, digits, and hyphens (no
      leading or trailing hyphen). The .{domain} suffix is fixed — Bluesky caps
      the whole handle at 30 characters.
    </small>

    <label for="did">Bluesky DID</label>
    <input
      id="did"
      type="text"
      placeholder="did:plc:…"
      maxlength="200"
      autocomplete="off"
      spellcheck="false"
      required
      bind:value={did}
    />
    <small>
      Your account's ID, not your username. In the Bluesky app: Settings →
      Advanced → "Copy your DID".
    </small>

    <!-- aria-busy is Blades' loading state: it swaps the label for a spinner
         and keeps the button's width, so the form doesn't jump. -->
    <button type="submit" aria-busy={submitting} disabled={submitting}>
      Register
    </button>
  </form>

  {#if result}
    <!-- role="status"/"alert" makes the outcome a live region, so screen
         readers announce it without the user hunting for the message. -->
    <article
      class={result.ok ? 'result-ok' : 'result-error'}
      role={result.ok ? 'status' : 'alert'}
    >
      {result.message}
    </article>
  {/if}
</main>

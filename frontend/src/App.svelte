<script lang="ts">
  // The domain the user is browsing on IS the registration target: the form
  // posts same-origin, so the request's Host header carries the domain
  // selection.  No server-side markup parameterization is needed.
  const domain = location.hostname

  let label = $state('')
  let did = $state('')
  let submitting = $state(false)
  // $state.raw: reassigned wholesale, never mutated, so no proxy is needed.
  let result = $state.raw<{ ok: boolean; message: string } | null>(null)

  // clsx-style join helper for Bulma class lists built from conditionals.
  const cx = (...parts: (string | false | undefined)[]) =>
    parts.filter(Boolean).join(' ')

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

<section class="section">
  <div class="container">
    <div class="columns is-centered">
      <div class="column is-half-desktop">
        <h1 class="title">Claim your handle</h1>
        <p class="subtitle">
          a Bluesky handle under <strong>{domain}</strong>
        </p>

        <p>
          Registering creates the DNS record that binds the handle to your
          Bluesky account. It's free, instant, and yours until you stop
          using it.
        </p>

        <form onsubmit={submit}>
          <div class="field">
            <label class="label" for="handle">Handle</label>
            <div class="field has-addons">
              <p class="control is-expanded">
                <input
                  id="handle"
                  class="input"
                  type="text"
                  placeholder="yourname"
                  maxlength="63"
                  autocomplete="off"
                  spellcheck="false"
                  required
                  bind:value={label}
                />
              </p>
              <p class="control">
                <span class="button is-static">.{domain}</span>
              </p>
            </div>
            <p class="help">
              3–63 characters: lowercase letters, digits, and hyphens (no
              leading or trailing hyphen).
            </p>
          </div>

          <div class="field">
            <label class="label" for="did">Bluesky DID</label>
            <div class="control">
              <input
                id="did"
                class="input"
                type="text"
                placeholder="did:plc:…"
                maxlength="200"
                autocomplete="off"
                spellcheck="false"
                required
                bind:value={did}
              />
            </div>
            <p class="help">
              Your account's ID, not your username. In the Bluesky app:
              Settings → Advanced → "Copy your DID".
            </p>
          </div>

          <div class="field">
            <div class="control">
              <button
                type="submit"
                class={cx('button is-primary', submitting && 'is-loading')}
                disabled={submitting}
              >
                Register
              </button>
            </div>
          </div>
        </form>

        {#if result}
          <article
            class={cx('message', result.ok ? 'is-success' : 'is-danger')}
          >
            <div class="message-body">{result.message}</div>
          </article>
        {/if}
      </div>
    </div>
  </div>
</section>

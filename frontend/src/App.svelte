<script lang="ts">
  // The domain the user is browsing on IS the registration target: the form
  // posts same-origin, so the request's Host header carries the domain
  // selection.  No server-side markup parameterization is needed.
  const domain = location.hostname;

  // bsky.app refuses custom-domain handles longer than 30 characters in total
  // (see AGENTS.md, "Composed handle"), so the label budget is whatever the
  // served domain leaves over.  Bounding the input to it also keeps the
  // composed handle inside the field instead of scrolling out of it, which
  // matters because the field's visible text is the full composed handle.
  const maxLabel = Math.max(3, 30 - `.${domain}`.length);

  let label = $state("");
  let did = $state("");
  let submitting = $state(false);
  // $state.raw: reassigned wholesale, never mutated, so no proxy is needed.
  let result = $state.raw<{ ok: boolean; message: string } | null>(null);

  // Step completion drives the visual state of the flow list (see the
  // `.done` / `.pending` rules in custom.css): a step's marker turns green
  // once its field holds content (or the registration succeeded), and the
  // post-registration steps stay muted until then.  The heuristics are
  // deliberately loose — the backend is the real validator — they only need
  // to answer "has the user done anything here yet?"
  let handleDone = $derived(label.trim().length >= 3);
  let didDone = $derived(did.trim().length > 0);
  let registered = $derived(result?.ok === true);
  // The button stays disabled (Blades dims it to 50% opacity) until both
  // fields hold content — a quiet "fill me in first" cue that lights up
  // only when there's actually something to submit.
  let formIncomplete = $derived(label.trim() === "" || did.trim() === "");

  async function submit(event: SubmitEvent) {
    event.preventDefault();
    if (submitting) return;
    submitting = true;
    result = null;
    try {
      const res = await fetch("/register", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ handle: label, did }),
      });
      const body = (await res.json()) as {
        status?: string;
        handle?: string;
        detail?: string;
      };
      if (res.ok) {
        const handle = body.handle ?? `${label}.${domain}`;
        // The DNS record now (or already) points at the account, but the
        // link is only established once the account adopts the handle
        // in-app.  Steps 7–8 in the flow below spell out those two buttons;
        // the message confirms the handle that was registered.
        result =
          body.status === "created"
            ? {
                ok: true,
                message: `@${handle} is registered — continue with steps 7 and 8 below.`,
              }
            : {
                ok: true,
                message: `@${handle} was already registered to your DID — continue with steps 7 and 8 below.`,
              };
      } else {
        // 4xx details are written for display to the end user; anything
        // else falls back to a generic line.
        result = {
          ok: false,
          message:
            res.status < 500 && body.detail
              ? body.detail
              : "Something went wrong; please try again.",
        };
      }
    } catch {
      result = {
        ok: false,
        message: "Could not reach the server; please try again.",
      };
    } finally {
      submitting = false;
    }
  }
</script>

<!-- Blades is classless: every element below is styled from its tag, role
     and ARIA state.  The whole registration is one numbered flow — an <ol>
     whose <li> elements are the steps — with the form fields interleaved
     into the steps they belong to.  The user always sees the instruction
     for a step right next to the element they need to interact with.
     Three visual states (see custom.css) make the list feel guided rather
     than static: completed steps get a green marker, upcoming steps are
     muted until they become relevant, and the step whose field is focused
     gets a subtle card background. -->
<main class="container narrow">
  <hgroup>
    <h1>Get yourself a <code>{domain}</code> handle on Bluesky</h1>
    <p>
      Much like pouring river water in your socks, it's quick, it's easy, and
      it's free
    </p>
  </hgroup>

  <p>
    This page sets up the DNS record that ties your Bluesky account to a handle
    under this domain. Follow the steps below to grab one. The handle
    is yours to keep for as long as I can help it.
  </p>

  <form onsubmit={submit}>
    <ol class="flow">
      <!-- Steps 1–3 happen in Bluesky: they produce the DID the form needs. -->
      <li>
        In the Bluesky app, open <strong>Settings → Account</strong> and tap
        <strong>Handle</strong>. On the web, go to
        <a
          href="https://bsky.app/settings/account"
          target="_blank"
          rel="noopener">bsky.app/settings/account</a
        >.
      </li>
      <li>
        In the popup, tap <strong>"I have my own domain"</strong> (the last option).
      </li>
      <li>
        Type your full handle, <strong>including the domain</strong> (for
        example, <code>yourname.{domain}</code>). A few lines down, you'll see
        some text starting with
        <code>did=</code>
        — <strong>copy that entire line</strong>.
      </li>

      <!-- Step 4: the handle field.  The field shows the handle as Bluesky
           will: label plus domain, as one string.  Only the label is
           editable, so the input carries it alone and its own glyphs are
           transparent — `.handle-fill` draws `label.domain` underneath,
           which leaves the caret, selection and typing inside the label
           and makes the domain unreachable with the cursor. -->
      <li class:done={handleDone}>
        <label for="handle">Type your handle again here:</label>
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
          <!-- One line on purpose: the overlay's text must contain no
               whitespace between the label and the suffix, or the field
               would show a gap.  While the field is empty the `{#if}`
               swaps in a sample name, so an untouched field still reads
               as a whole handle. -->
          <span class="handle-fill" aria-hidden="true"
            >{#if label}{label}{:else}<span class="handle-placeholder"
                >yourname</span
              >{/if}<span class="handle-suffix">.{domain}</span></span
          >
        </div>
        <small id="handle-hint">
          Just the name part this time; the <code>.{domain}</code> suffix will
          be added for you. 3–{maxLabel}
          characters: lowercase letters, digits, and hyphens (no leading or trailing
          hyphen).
        </small>
      </li>

      <!-- Step 5: the DID field.  The placeholder mirrors what Bluesky's
           handle dialog shows and what the user copies: the full TXT value
           `did=did:plc:…`.  The backend strips the leading `did=` (see
           AGENTS.md), so pasting verbatim works. -->
      <li class:done={didDone}>
        <label for="did">Paste that <code>did=</code> line here:</label>
        <input
          id="did"
          type="text"
          placeholder="did=did:plc:…"
          maxlength="200"
          autocomplete="off"
          spellcheck="false"
          required
          bind:value={did}
        />
        <small>
          The text you copied from Bluesky in step 3: your public account ID.
        </small>
      </li>

      <!-- Step 6: submit.  aria-busy is Blades' loading state: it swaps the
           label for a spinner and keeps the button's width, so the form
           doesn't jump.  The button is also disabled until both fields hold
           content — Blades dims it to 50% opacity — and the marker mutes in
           lockstep (pending while the form is incomplete), so the whole step
           reads as inactive until there's something to submit.  The result
           message lives inside this step, right below the button, so the
           outcome appears where the action happened rather than at the
           bottom of the page. -->
      <li class:done={registered} class:pending={formIncomplete && !registered}>
        <button
          type="submit"
          aria-busy={submitting}
          disabled={submitting || formIncomplete}
        >
          Register
        </button>
        {#if result}
          <!-- role="status"/"alert" makes the outcome a live region, so
               screen readers announce it without the user hunting for the
               message. -->
          <article
            class={result.ok ? "result-ok" : "result-error"}
            role={result.ok ? "status" : "alert"}
          >
            {result.message}
          </article>
        {/if}
      </li>

      <!-- Steps 7–8 happen back in Bluesky.  They stay muted until the
           registration succeeds, then light up as the next thing to do. -->
      <li class:pending={!registered}>
        Return to Bluesky and tap <strong>"Verify DNS Record"</strong>. DNS can
        take a few minutes to become visible, so if it fails, wait a few moments
        and try again.
      </li>
      <li class:pending={!registered}>
        Once verified, tap <strong>"Update to your handle"</strong>. The handle
        is now yours!
      </li>
      <li class:pending={!registered}>
        (Optional) Help me pay for this domain by <a
          href="https://ko-fi.com/possdog"
          target="_blank"
          rel="noopener">supporting me on Ko-fi &hearts;</a
        >
      </li>
    </ol>
  </form>

  <footer>
    <small>
      a thing by <a
        href="https://bsky.app/profile/poss.dog"
        target="_blank"
        rel="noopener">@poss.dog</a
      >
      —
      <a
        href="http://tangled.org/poss.dog/niceat"
        target="_blank"
        rel="noopener">source code</a
      >
    </small>
  </footer>
</main>

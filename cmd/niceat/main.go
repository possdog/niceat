// niceat: self-service Bluesky handle registration.
//
// Serves one endpoint, POST /register, which takes a desired handle label
// and a Bluesky account DID, and provisions the _atproto TXT record that
// binds <label>.<domain> to the DID.  One deployment can manage several
// domains, and each request's Host header selects which domain the handle
// is registered under.  This lets communities share a domain as Bluesky
// handles without each member needing to contact the owner.
//
// DNS is the claim store: the Porkbun record itself decides whether a
// handle is available.  Without Porkbun credentials the service falls back
// to an in-memory provisioner that provisions nothing (see provision.go).

package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

func main() {
	// The config path is relative to the working directory by default, so
	// a service manager only needs to set the working directory.
	configPath := flag.String("config", defaultConfigPath, "path to the JSON config file, relative to the current working directory")
	// -addr overrides the config file's "addr" — handy when a host runs
	// several services and ports are handed out dynamically (e.g. -addr
	// :0 for an ephemeral port).  Empty means "use the config file".
	addrOverride := flag.String("addr", "", "HTTP listen address; overrides the config file's \"addr\"")
	// -reap-once runs exactly one reaper sweep and exits — no HTTP
	// server.  This is how the reaper runs on Fly: a scheduled Machine
	// (no services, so the proxy never auto-stops it) whose lifetime is
	// exactly this process's lifetime; exiting is what puts the Machine
	// back to sleep.  Overrides the config's reaper.enabled: creating
	// the scheduled Machine is the enablement.
	reapOnce := flag.Bool("reap-once", false, "run one reaper sweep and exit (no HTTP server)")
	flag.Parse()

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("loading config: %v", err)
	}
	// Precedence: -addr flag > config "addr" > default.
	if *addrOverride != "" {
		cfg.Addr = *addrOverride
	}

	// X-Forwarded-For trust is per-deployment opt-in (config
	// "trusted_proxies"); with no prefixes configured, no proxy is
	// trusted and the TCP peer address is always the client IP.
	if len(cfg.trustedProxies) > 0 {
		prefixes := make([]string, 0, len(cfg.trustedProxies))
		for _, p := range cfg.trustedProxies {
			prefixes = append(prefixes, p.String())
		}
		log.Printf("trusted proxy prefixes: %s", strings.Join(prefixes, ", "))
	}
	trustedProxies = cfg.trustedProxies

	// Per-domain Porkbun credentials (porkbun_keys.json in the working
	// directory, gitignored, or the PORKBUN_CREDENTIALS environment
	// variable) select the real DNS backend.  Without them, the in-memory
	// fallback runs: same semantics, but nothing is published and state
	// is lost on restart.
	creds, haveCreds, err := loadPorkbunCredentials(".", cfg.Domains)
	if err != nil {
		log.Fatalf("loading Porkbun credentials: %v", err)
	}
	backend := provisioner(newMemoryProvisioner())
	if haveCreds {
		backend = newPorkbunProvisioner(creds, "")
	} else {
		// ERROR, not WARNING: this is the one misconfiguration that turns
		// the service into a silent fake-success (registrations answer
		// 201/200 while nothing is published), so the line must trip
		// log-based alerting.  /healthz reports the same condition as
		// "provisioner":"memory" for machine detection.
		log.Println("ERROR: no Porkbun credentials found — running the in-memory provisioner; registrations are accepted but NOT published and are lost on restart; this instance must not serve real traffic")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// One-shot reaper mode: sweep once under a hard wall-clock deadline
	// (on top of every call's own timeout) and exit — the exit status
	// reports whether the sweep covered every zone, and exiting ends the
	// scheduled Machine's billing.
	if *reapOnce {
		if !haveCreds {
			log.Fatal("-reap-once requires Porkbun credentials")
		}
		deadlineCtx, cancelDeadline := context.WithTimeout(ctx, reaperOnceDeadline)
		defer cancelDeadline()
		rep := newReaper(cfg.Domains, backend.(*porkbunProvisioner), newPlcDirectory(""), cfg.reaperGrace)
		if err := rep.sweep(deadlineCtx); err != nil {
			log.Fatalf("reaper sweep: %v", err)
		}
		log.Println("reaper sweep complete")
		return
	}

	// The handle reaper frees labels whose accounts no longer use them.
	// It needs the real DNS backend (the in-memory one publishes
	// nothing to sweep) and runs until shutdown; see reaper.go.  On
	// Fly the serving Machine leaves this disabled (reaper.enabled =
	// false in its config) and a separate scheduled Machine reaps via
	// -reap-once instead.
	if haveCreds && cfg.reaperEnabled {
		rep := newReaper(cfg.Domains, backend.(*porkbunProvisioner), newPlcDirectory(""), cfg.reaperGrace)
		go rep.run(ctx, cfg.reaperInterval)
		log.Printf("handle reaper enabled: interval %s, grace %s", cfg.reaperInterval, cfg.reaperGrace)
	} else if !haveCreds && cfg.reaperEnabled {
		log.Println("WARNING: handle reaper disabled — no Porkbun credentials to sweep")
	}

	srv := &app{domains: cfg.managedSet(), provisioner: backend}

	mux := srv.newMux(newRateLimiter())

	httpServer := &http.Server{
		Addr:    cfg.Addr,
		Handler: mux,
		// Timeouts cap how long any single client can hold a
		// connection, so slow or stalled clients (slowloris) cannot
		// pin file descriptors indefinitely.  Every response this
		// service produces is small and immediate, so generous bounds
		// cost legitimate clients nothing.  IdleTimeout bounds
		// keep-alive reuse once the frontend serves static content.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	go func() {
		log.Printf("HTTP listening on %s, serving handles under %s", cfg.Addr, strings.Join(cfg.Domains, ", "))
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("HTTP server error: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down gracefully …")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()

	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("HTTP shutdown error: %v", err)
	}
	log.Println("server stopped")
}

// newMux wires the HTTP routes: the rate-limited registration API and the
// static frontend page.  main.go serves exactly this mux and the tests
// exercise routing through it, so their behavior cannot drift apart.
func (a *app) newMux(rl *rateLimiter) *http.ServeMux {
	mux := http.NewServeMux()
	// Every route is wrapped in rejectAbsoluteForm: Go's server sets
	// r.Host from an absolute-form request target, silently overriding
	// the Host header (audit INFO-9), so a "POST http://<managed>/register"
	// wire request was attributed to the target's host no matter what
	// the Host header said.  niceat is an origin server — its clients
	// (browsers, the Fly edge proxy) send origin-form targets — so
	// absolute-form requests are rejected outright rather than risk
	// disagreeing with any front proxy that routes by the Host header.
	// Method-specific pattern: non-POST requests to /register get a 405
	// from the mux without any handler code.  The per-IP rate limiter
	// wraps only this route — the form page is a public static read and
	// must not consume a visitor's registration budget.
	mux.Handle("POST /register", rejectAbsoluteForm(rl.middleware(http.HandlerFunc(a.handleRegister))))
	// {$} matches only "/" exactly; every other path falls through to the
	// mux's plain 404.
	mux.Handle("GET /{$}", rejectAbsoluteForm(http.HandlerFunc(a.serveIndex)))
	// Liveness probe for deploy-time health checks (Fly [http_service.checks]).
	// Not rate-limited, like the form page: health probes are
	// infrastructure traffic, not visitor registrations.
	mux.Handle("GET /healthz", rejectAbsoluteForm(http.HandlerFunc(a.handleHealthz)))
	return mux
}

// rejectAbsoluteForm answers 400 to any request whose request-target on the
// wire was not origin-form ("/path"): absolute-form ("scheme://host/path")
// because Go's server lets it override the Host header (see newMux), and
// authority-form ("host:port", CONNECT-only) because it has no meaning at
// an origin server either.  Origin-form requests pass through byte-for-byte
// unchanged in behavior.
func rejectAbsoluteForm(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if absoluteFormTarget(r.RequestURI) {
			writeJSON(w, http.StatusBadRequest, errorResponse{Detail: "Invalid request target"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// absoluteFormTarget reports whether target — the request-target exactly as
// it appeared on the wire (r.RequestURI, which the mux otherwise ignores) —
// starts with a scheme.  Per RFC 3986 a scheme starts with a letter and
// continues with letters, digits, "+", "-", or "." up to the first ":".
// Origin-form ("/path") and asterisk-form ("*") targets start with a
// non-letter and can never match.
func absoluteFormTarget(target string) bool {
	if target == "" || !isASCIILetter(target[0]) {
		return false
	}
	i := 1
	for i < len(target) && isSchemeByte(target[i]) {
		i++
	}
	return i < len(target) && target[i] == ':'
}

func isASCIILetter(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func isSchemeByte(c byte) bool {
	return isASCIILetter(c) || c >= '0' && c <= '9' || c == '+' || c == '-' || c == '.'
}

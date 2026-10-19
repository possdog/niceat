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
	flag.Parse()

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("loading config: %v", err)
	}
	// Precedence: -addr flag > config "addr" > default.
	if *addrOverride != "" {
		cfg.Addr = *addrOverride
	}

	// The Porkbun credential pair (porkbun_api_key.txt /
	// porkbun_secret_key.txt in the working directory, gitignored) selects
	// the real DNS backend.  Without it, the in-memory fallback runs: same
	// semantics, but nothing is published and state is lost on restart.
	apiKey, secretKey, haveCreds, err := loadPorkbunCredentials(".")
	if err != nil {
		log.Fatalf("loading Porkbun credentials: %v", err)
	}
	backend := provisioner(newMemoryProvisioner())
	if haveCreds {
		backend = newPorkbunProvisioner(apiKey, secretKey, "")
	} else {
		log.Println("WARNING: no Porkbun credentials found — running the in-memory provisioner; registrations are accepted but no DNS records are created")
	}

	srv := &app{domains: cfg.managedSet(), provisioner: backend}

	mux := http.NewServeMux()
	// Method-specific pattern: non-POST requests to /register get a 405
	// from the mux without any handler code.
	mux.HandleFunc("POST /register", srv.handleRegister)
	// TODO(frontend): the form will be served from this same mux, on every
	// managed domain.  It must be parameterized by the request Host — e.g.
	// "get your handle under *@wagg.ing*" on wagg.ing, the equivalent for
	// yeen.world on yeen.world — and post to the same origin, so the Host
	// header carries the domain selection for /register too.

	// Per-IP rate limiting wraps everything; see ratelimit.go.
	rl := newRateLimiter()

	httpServer := &http.Server{
		Addr:    cfg.Addr,
		Handler: rl.middleware(mux),
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

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

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

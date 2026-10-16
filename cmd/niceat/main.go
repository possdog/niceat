// niceat: self-service Bluesky handle registration.
//
// Serves one endpoint, POST /register, which takes a desired handle label
// and a Bluesky account DID, and provisions the _atproto TXT record that
// binds <label>.<domain> to the DID.  One deployment can manage several
// domains: the -domain flag is repeatable, and each request's Host header
// selects which domain the handle is registered under.  This lets
// communities share a domain as Bluesky handles without each member
// needing to contact the owner.
//
// Current status: the HTTP layer and validation are complete; DNS
// provisioning is a logging stub (see provision.go) until the Porkbun
// client is built.

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
	configPath := flag.String("config", defaultConfigPath, "path to the JSON config file")
	flag.Parse()

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("loading config: %v", err)
	}

	// TODO(defense): wrap the mux with per-IP rate limiting before this is
	// exposed publicly — see AGENTS.md "Planned defenses".
	srv := &app{domains: cfg.managedSet(), provisioner: stubProvisioner{}}
	log.Println("WARNING: stub provisioner active — registrations will be accepted but no DNS records are created")

	mux := http.NewServeMux()
	// Method-specific pattern: non-POST requests to /register get a 405
	// from the mux without any handler code.
	mux.HandleFunc("POST /register", srv.handleRegister)
	// TODO(frontend): the form will be served from this same mux, on every
	// managed domain.  It must be parameterized by the request Host — e.g.
	// "get your handle under *@wagg.ing*" on wagg.ing, the equivalent for
	// yeen.world on yeen.world — and post to the same origin, so the Host
	// header carries the domain selection for /register too.

	httpServer := &http.Server{Addr: cfg.Addr, Handler: mux}

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

// niceat: self-service Bluesky handle registration.
//
// Serves one endpoint, POST /register, which takes a desired handle label
// and a Bluesky account DID, and provisions the _atproto TXT record that
// binds <label>.<domain> to the DID.  This lets communities share a domain
// as Bluesky handles without each member needing to contact the owner.
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
	"syscall"
	"time"
)

func main() {
	domain := flag.String("domain", "", "apex domain to serve handles under (required)")
	addr := flag.String("addr", ":8080", "HTTP listen address")
	flag.Parse()

	if *domain == "" {
		log.Fatal("-domain is required (e.g. -domain example.com)")
	}

	// TODO(defense): wrap the mux with per-IP rate limiting before this is
	// exposed publicly — see AGENTS.md "Planned defenses".
	srv := &app{domain: *domain, provisioner: stubProvisioner{}}
	log.Println("WARNING: stub provisioner active — registrations will be accepted but no DNS records are created")

	mux := http.NewServeMux()
	// Method-specific pattern: non-POST requests to /register get a 405
	// from the mux without any handler code.
	mux.HandleFunc("POST /register", srv.handleRegister)

	httpServer := &http.Server{Addr: *addr, Handler: mux}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	go func() {
		log.Printf("HTTP listening on %s", httpServer.Addr)
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

package main

import "net/http"

// healthzResponse is the liveness body: {"status":"ok"}.
type healthzResponse struct {
	Status string `json:"status"`
}

// handleHealthz serves GET /healthz, a bare liveness probe for deploy-time
// health checks (Fly.io [http_service.checks]).  It must be cheap and
// side-effect free: it deliberately does NOT probe external systems
// (Porkbun, plc.directory), because a health check that depends on
// third-party network egress would flap the app on their blips and fail
// deploys for reasons that are not niceat's health.  A 200 here means only
// "the process is up and serving".
func (a *app) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, healthzResponse{Status: "ok"})
}

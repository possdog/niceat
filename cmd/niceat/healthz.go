package main

import "net/http"

// healthzResponse is the liveness body:
// {"status":"ok","provisioner":"porkbun"|"memory"}.
type healthzResponse struct {
	Status      string `json:"status"`
	Provisioner string `json:"provisioner"`
}

// handleHealthz serves GET /healthz, a bare liveness probe for deploy-time
// health checks (Fly.io [http_service.checks]).  It must be cheap and
// side-effect free: it deliberately does NOT probe external systems
// (Porkbun, plc.directory), because a health check that depends on
// third-party network egress would flap the app on their blips and fail
// deploys for reasons that are not niceat's health.  A 200 here means only
// "the process is up and serving"; the provisioner field is the one extra
// fact reported, because a deploy that silently lost its Porkbun
// credentials serves fake successes (registrations answer 201/200 while
// nothing is published) and must be machine-detectable — alert on
// "provisioner":"memory".  The name only; credential material never
// reaches logs or this body.
func (a *app) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, healthzResponse{Status: "ok", Provisioner: a.provisionerName()})
}

// provisionerName reports which DNS backend is active.  Derived from the
// concrete provisioner type rather than a separately wired string so it
// cannot drift from the backend actually answering registrations.
func (a *app) provisionerName() string {
	switch a.provisioner.(type) {
	case *porkbunProvisioner:
		return "porkbun"
	case *memoryProvisioner:
		return "memory"
	default:
		return "unknown"
	}
}

package shared

import (
	"encoding/json"
	"net/http"
)

// HealthHandler is mounted at /health on every service. Deliberately never
// checks downstream dependencies -- liveness should reflect this process
// only, so a downstream outage shows up as THIS service's requests failing
// (real evidence for the correlator) rather than as a misleading liveness
// flap that restarts a perfectly healthy pod.
func HealthHandler(service string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"status":  "ok",
			"service": service,
		})
	}
}

// WriteJSON is the one shared response helper -- small enough that pulling in
// a framework for it would cost more than it saves.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// WriteError emits a consistent {"error": "..."} body. Kept distinct from
// WriteJSON so error paths are easy to grep for in both code and logs.
func WriteError(w http.ResponseWriter, status int, message string) {
	WriteJSON(w, status, map[string]string{"error": message})
}

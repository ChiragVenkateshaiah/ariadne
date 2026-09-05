// payment-svc simulates a payment gateway. It exists in this SUT specifically
// to carry the Act 3 resilience demo: toxiproxy sits in front of it, and
// booking-api's HTTP client to it deliberately has NO request timeout (see
// booking-api/main.go) -- replicating a real, common defect class ("infinite
// spinner, no timeout handling") that functional tests never catch because
// they only ever run against a healthy dependency.
package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/chirag/ariadne/sut/shared"
	"github.com/google/uuid"
)

type chargeRequest struct {
	Amount    float64 `json:"amount"`
	Currency  string  `json:"currency"`
	CardLast4 string  `json:"card_last4"`
}

type chargeResponse struct {
	ChargeID string `json:"charge_id"`
	Status   string `json:"status"` // APPROVED | DECLINED
}

func main() {
	logger := shared.NewLogger("payment-svc")

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", shared.HealthHandler("payment-svc"))

	mux.HandleFunc("POST /api/v1/charge", func(w http.ResponseWriter, r *http.Request) {
		log := shared.LoggerFromRequest(logger, r)

		var req chargeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			shared.WriteError(w, http.StatusBadRequest, "invalid request body")
			return
		}

		// Simulated gateway latency under normal conditions -- small and
		// constant. Fault injection (toxiproxy) adds the variable, demo-
		// controlled latency on top of this, not this service's own logic.
		time.Sleep(80 * time.Millisecond)

		if req.Amount <= 0 {
			log.Warn("declined: non-positive amount", "amount", req.Amount)
			shared.WriteJSON(w, http.StatusOK, chargeResponse{Status: "DECLINED"})
			return
		}

		chargeID := uuid.NewString()
		log.Info("charge approved", "charge_id", chargeID, "amount", req.Amount, "currency", req.Currency)
		shared.WriteJSON(w, http.StatusOK, chargeResponse{ChargeID: chargeID, Status: "APPROVED"})
	})

	port := os.Getenv("PORT")
	if port == "" {
		port = "8084"
	}
	addr := fmt.Sprintf(":%s", port)
	srv := &http.Server{
		Addr:    addr,
		Handler: shared.WithTracing(logger, mux),
		// No WriteTimeout here either: this service should behave like a
		// real external gateway, whose latency profile only fault injection
		// controls -- not an artificial cap baked into the simulator.
		ReadTimeout: 5 * time.Second,
	}
	logger.Info("starting", "addr", addr, slog.String("role", "simulated payment gateway"))
	if err := srv.ListenAndServe(); err != nil {
		logger.Error("server exited", "error", err.Error())
		os.Exit(1)
	}
}

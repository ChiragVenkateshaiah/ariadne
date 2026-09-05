// booking-api orchestrates a booking: charge the card via payment-svc, then
// persist the booking in Postgres.
//
// DELIBERATE DEFECT (Act 3 of the demo): the HTTP client used to call
// payment-svc has NO timeout. This is not an oversight -- it is the specific,
// realistic defect class the resilience validator exists to catch: under
// normal conditions nobody notices, but the moment payment-svc's latency
// degrades (toxiproxy, or a real incident), every booking request hangs
// indefinitely with no user feedback. A functional test suite that only ever
// runs against a healthy payment-svc will pass forever and never find this.
package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/chirag/ariadne/sut/shared"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

const createTableSQL = `
CREATE TABLE IF NOT EXISTS bookings (
    id              TEXT PRIMARY KEY,
    flight_id       TEXT NOT NULL,
    passenger_name  TEXT NOT NULL,
    origin          TEXT NOT NULL,
    destination     TEXT NOT NULL,
    depart_at       TEXT NOT NULL,
    amount          NUMERIC(10,2) NOT NULL,
    currency        TEXT NOT NULL,
    status          TEXT NOT NULL,
    charge_id       TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
)`

type bookingRequest struct {
	FlightID       string  `json:"flight_id"`
	PassengerName  string  `json:"passenger_name"`
	Origin         string  `json:"origin"`
	Destination    string  `json:"destination"`
	DepartAt       string  `json:"depart_at"`
	Amount         float64 `json:"amount"`
	Currency       string  `json:"currency"`
	CardLast4      string  `json:"card_last4"`
}

type bookingResponse struct {
	BookingID string `json:"booking_id"`
	Status    string `json:"status"` // CONFIRMED | DECLINED | FAILED
	Reason    string `json:"reason,omitempty"`
}

func paymentSvcURL() string {
	if v := os.Getenv("PAYMENT_SVC_URL"); v != "" {
		return v
	}
	return "http://payment-svc:8084"
}

func chargeCard(traceID string, amount float64, currency, cardLast4 string) (chargeID, status string, err error) {
	body, _ := json.Marshal(map[string]any{
		"amount": amount, "currency": currency, "card_last4": cardLast4,
	})
	req, err := http.NewRequest(http.MethodPost, paymentSvcURL()+"/api/v1/charge", bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	shared.PropagateTraceHeader(req, traceID)

	// NOTE: intentionally http.DefaultClient (no timeout configured). See the
	// file-level comment -- this is the demo's resilience defect, not a bug
	// to "fix" casually.
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("payment-svc returned %d", resp.StatusCode)
	}
	var out struct {
		ChargeID string `json:"charge_id"`
		Status   string `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", "", err
	}
	return out.ChargeID, out.Status, nil
}

func main() {
	logger := shared.NewLogger("booking-api")

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://ariadne:ariadne@postgres:5432/ariadne?sslmode=disable"
	}
	db, err := sql.Open("pgx", dbURL)
	if err != nil {
		logger.Error("db open failed", "error", err.Error())
		os.Exit(1)
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := waitForDB(ctx, db, logger); err != nil {
		logger.Error("db not reachable at startup", "error", err.Error())
		os.Exit(1)
	}
	if _, err := db.Exec(createTableSQL); err != nil {
		logger.Error("schema migration failed", "error", err.Error())
		os.Exit(1)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", shared.HealthHandler("booking-api"))

	mux.HandleFunc("POST /api/v1/bookings", func(w http.ResponseWriter, r *http.Request) {
		log := shared.LoggerFromRequest(logger, r)

		var req bookingRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			shared.WriteError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if req.FlightID == "" || req.PassengerName == "" || req.Amount <= 0 {
			shared.WriteError(w, http.StatusBadRequest, "flight_id, passenger_name and a positive amount are required")
			return
		}
		if req.Currency == "" {
			req.Currency = "USD"
		}

		chargeID, chargeStatus, err := chargeCard(shared.TraceID(r.Context()), req.Amount, req.Currency, req.CardLast4)
		if err != nil {
			log.Error("payment charge failed", "error", err.Error(), "flight_id", req.FlightID)
			shared.WriteJSON(w, http.StatusBadGateway, bookingResponse{Status: "FAILED", Reason: "payment gateway error"})
			return
		}
		if chargeStatus != "APPROVED" {
			log.Warn("payment declined", "flight_id", req.FlightID)
			shared.WriteJSON(w, http.StatusPaymentRequired, bookingResponse{Status: "DECLINED"})
			return
		}

		bookingID := uuid.NewString()
		_, err = db.ExecContext(r.Context(),
			`INSERT INTO bookings (id, flight_id, passenger_name, origin, destination, depart_at, amount, currency, status, charge_id)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'CONFIRMED',$9)`,
			bookingID, req.FlightID, req.PassengerName, req.Origin, req.Destination, req.DepartAt, req.Amount, req.Currency, chargeID)
		if err != nil {
			log.Error("booking persist failed", "error", err.Error(), "charge_id", chargeID)
			shared.WriteJSON(w, http.StatusInternalServerError, bookingResponse{Status: "FAILED", Reason: "could not persist booking"})
			return
		}

		log.Info("booking confirmed", "booking_id", bookingID, "flight_id", req.FlightID, "charge_id", chargeID)
		shared.WriteJSON(w, http.StatusCreated, bookingResponse{BookingID: bookingID, Status: "CONFIRMED"})
	})

	mux.HandleFunc("GET /api/v1/bookings/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		var b bookingResponse
		var status string
		err := db.QueryRowContext(r.Context(), `SELECT status FROM bookings WHERE id = $1`, id).Scan(&status)
		if err == sql.ErrNoRows {
			shared.WriteError(w, http.StatusNotFound, "booking not found")
			return
		}
		if err != nil {
			shared.WriteError(w, http.StatusInternalServerError, "lookup failed")
			return
		}
		b.BookingID = id
		b.Status = status
		shared.WriteJSON(w, http.StatusOK, b)
	})

	port := os.Getenv("PORT")
	if port == "" {
		port = "8083"
	}
	addr := fmt.Sprintf(":%s", port)
	srv := &http.Server{
		Addr:         addr,
		Handler:      shared.WithTracing(logger, mux),
		ReadTimeout:  5 * time.Second,
		// No WriteTimeout: a slow payment-svc call must be free to hang the
		// full request lifecycle for the resilience demo to be real.
	}
	logger.Info("starting", "addr", addr, slog.String("payment_svc_url", paymentSvcURL()))
	if err := srv.ListenAndServe(); err != nil {
		logger.Error("server exited", "error", err.Error())
		os.Exit(1)
	}
}

func waitForDB(ctx context.Context, db *sql.DB, logger *slog.Logger) error {
	for {
		err := db.PingContext(ctx)
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(1 * time.Second):
			logger.Info("waiting for database", "error", err.Error())
		}
	}
}

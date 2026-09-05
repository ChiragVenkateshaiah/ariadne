// search-api returns candidate flight offers for an origin/destination/date.
// Flight data is deterministically generated in-memory (no external airline
// data needed for the demo) but pricing is a real service call -- this is the
// seam the World Model discovers as EXERCISES -> SERVED_BY -> pricing-svc,
// and the reason a pricing-svc change puts search results at risk.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/chirag/ariadne/sut/shared"
)

type offer struct {
	FlightID    string  `json:"flight_id"`
	Airline     string  `json:"airline"`
	Origin      string  `json:"origin"`
	Destination string  `json:"destination"`
	DepartAt    string  `json:"depart_at"`
	ArriveAt    string  `json:"arrive_at"`
	Amount      float64 `json:"amount"`
	Currency    string  `json:"currency"`
}

var airlines = []string{"Iberia", "Lufthansa", "Air France", "Delta"}

// syntheticFlights deterministically derives a small, stable set of flights
// from the route so the same search always returns the same offers -- the UI
// tests need reproducible results, not random ones.
func syntheticFlights(origin, destination, date string) []struct {
	airline           string
	departHour        int
	durationHrs       int
	baseFare, taxes   float64
} {
	h := fnv.New32a()
	_, _ = h.Write([]byte(origin + destination + date))
	seed := h.Sum32()

	flights := make([]struct {
		airline           string
		departHour        int
		durationHrs       int
		baseFare, taxes   float64
	}, 3)
	for i := range flights {
		s := seed + uint32(i)*2654435761
		flights[i].airline = airlines[s%uint32(len(airlines))]
		flights[i].departHour = int(6 + (s>>3)%14)          // 06:00-19:00
		flights[i].durationHrs = int(2 + (s>>7)%9)           // 2-10h
		flights[i].baseFare = float64(120 + (s>>11)%380)     // 120-499
		flights[i].taxes = float64(20 + (s>>17)%60)          // 20-79
	}
	return flights
}

func pricingURL() string {
	if v := os.Getenv("PRICING_SVC_URL"); v != "" {
		return v
	}
	return "http://pricing-svc:8082"
}

func fetchPrice(client *http.Client, traceID string, baseFare, taxes float64) (amount float64, currency string, err error) {
	body, _ := json.Marshal(map[string]any{
		"base_fare": baseFare,
		"taxes":     taxes,
		"currency":  "USD",
	})
	req, err := http.NewRequest(http.MethodPost, pricingURL()+"/api/v1/price", bytes.NewReader(body))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	shared.PropagateTraceHeader(req, traceID)

	resp, err := client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, "", fmt.Errorf("pricing-svc returned %d", resp.StatusCode)
	}
	var out struct {
		Amount   float64 `json:"amount"`
		Currency string  `json:"currency"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, "", err
	}
	return out.Amount, out.Currency, nil
}

func main() {
	logger := shared.NewLogger("search-api")
	client := &http.Client{Timeout: 3 * time.Second}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", shared.HealthHandler("search-api"))

	mux.HandleFunc("GET /api/v1/search", func(w http.ResponseWriter, r *http.Request) {
		log := shared.LoggerFromRequest(logger, r)
		origin := r.URL.Query().Get("origin")
		destination := r.URL.Query().Get("destination")
		date := r.URL.Query().Get("date")

		if origin == "" || destination == "" || date == "" {
			shared.WriteError(w, http.StatusBadRequest, "origin, destination and date are required")
			return
		}

		flights := syntheticFlights(origin, destination, date)
		offers := make([]offer, 0, len(flights))
		for i, f := range flights {
			amount, currency, err := fetchPrice(client, shared.TraceID(r.Context()), f.baseFare, f.taxes)
			if err != nil {
				log.Error("pricing-svc call failed", "error", err.Error(), "origin", origin, "destination", destination)
				shared.WriteError(w, http.StatusBadGateway, "pricing unavailable")
				return
			}
			depart := fmt.Sprintf("%sT%02d:00:00Z", date, f.departHour)
			arrive := fmt.Sprintf("%sT%02d:00:00Z", date, (f.departHour+f.durationHrs)%24)
			offers = append(offers, offer{
				FlightID:    fmt.Sprintf("%s%s%s-%d", origin, destination, date, i+1),
				Airline:     f.airline,
				Origin:      origin,
				Destination: destination,
				DepartAt:    depart,
				ArriveAt:    arrive,
				Amount:      amount,
				Currency:    currency,
			})
		}

		log.Info("search completed", "origin", origin, "destination", destination, "date", date, "offer_count", len(offers))
		shared.WriteJSON(w, http.StatusOK, map[string]any{"offers": offers})
	})

	port := os.Getenv("PORT")
	if port == "" {
		port = "8081"
	}
	addr := fmt.Sprintf(":%s", port)
	srv := &http.Server{
		Addr:         addr,
		Handler:      shared.WithTracing(logger, mux),
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
	}
	logger.Info("starting", "addr", addr, slog.String("pricing_svc_url", pricingURL()))
	if err := srv.ListenAndServe(); err != nil {
		logger.Error("server exited", "error", err.Error())
		os.Exit(1)
	}
}

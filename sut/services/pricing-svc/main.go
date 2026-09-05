// pricing-svc computes the final price for a fare. It exists in this SUT
// specifically to carry the Act 2 demo defect: its rounding behaviour is
// controlled by a mounted ConfigMap, re-read on every request, so a single
// `kubectl apply` on the ConfigMap changes production pricing behaviour with
// NO image change and NO pod restart -- exactly the class of change that
// commit-triggered CI would never see, and that only a cluster-native sensor
// (watching the ConfigMap itself) catches.
package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"os"
	"time"

	"github.com/chirag/ariadne/sut/shared"
)

const defaultFlagsPath = "/etc/pricing/flags.json"

type roundingFlags struct {
	RoundingMode string `json:"rounding_mode"` // "HALF_UP" | "FLOOR"
}

func loadFlags(path string) roundingFlags {
	flags := roundingFlags{RoundingMode: "HALF_UP"}
	data, err := os.ReadFile(path)
	if err != nil {
		return flags // ConfigMap not mounted (e.g. local dev) -- safe default
	}
	_ = json.Unmarshal(data, &flags)
	if flags.RoundingMode == "" {
		flags.RoundingMode = "HALF_UP"
	}
	return flags
}

func round(total float64, mode string) float64 {
	switch mode {
	case "FLOOR":
		return math.Floor(total*100) / 100
	default: // HALF_UP
		return math.Round(total*100) / 100
	}
}

type priceRequest struct {
	BaseFare float64 `json:"base_fare"`
	Taxes    float64 `json:"taxes"`
	Currency string  `json:"currency"`
}

type priceResponse struct {
	Amount       float64 `json:"amount"`
	Currency     string  `json:"currency"`
	RoundingMode string  `json:"rounding_mode"`
}

func main() {
	logger := shared.NewLogger("pricing-svc")
	flagsPath := os.Getenv("FLAGS_PATH")
	if flagsPath == "" {
		flagsPath = defaultFlagsPath
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", shared.HealthHandler("pricing-svc"))

	mux.HandleFunc("POST /api/v1/price", func(w http.ResponseWriter, r *http.Request) {
		log := shared.LoggerFromRequest(logger, r)

		var req priceRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			log.Warn("bad request body", "error", err.Error())
			shared.WriteError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if req.BaseFare < 0 || req.Taxes < 0 {
			shared.WriteError(w, http.StatusBadRequest, "fare and taxes must be non-negative")
			return
		}
		if req.Currency == "" {
			req.Currency = "USD"
		}

		flags := loadFlags(flagsPath)
		total := req.BaseFare + req.Taxes
		amount := round(total, flags.RoundingMode)

		log.Info("priced fare",
			"base_fare", req.BaseFare, "taxes", req.Taxes,
			"raw_total", total, "rounded_amount", amount,
			"rounding_mode", flags.RoundingMode, "currency", req.Currency)

		shared.WriteJSON(w, http.StatusOK, priceResponse{
			Amount:       amount,
			Currency:     req.Currency,
			RoundingMode: flags.RoundingMode,
		})
	})

	port := os.Getenv("PORT")
	if port == "" {
		port = "8082"
	}
	addr := fmt.Sprintf(":%s", port)
	srv := &http.Server{
		Addr:         addr,
		Handler:      shared.WithTracing(logger, mux),
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
	}
	logger.Info("starting", "addr", addr, "flags_path", flagsPath, slog.Any("initial_flags", loadFlags(flagsPath)))
	if err := srv.ListenAndServe(); err != nil {
		logger.Error("server exited", "error", err.Error())
		os.Exit(1)
	}
}

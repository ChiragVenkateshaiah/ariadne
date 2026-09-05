// web-ui is the demo's browser-facing surface: a plain server-rendered HTML
// app (no JS framework) so Playwright automates real DOM the same way a user
// would.
//
// UI_VARIANT is the Act 1 demo lever: setting it to "v2" (via `kubectl set
// env deployment/web-ui UI_VARIANT=v2`, no image rebuild) renames every
// input's id and the search button's id AND visible text. This is a real
// Deployment env change the Cluster Sensor observes as a ChangeEvent, and it
// is exactly the class of change that should be healed, never blocked.
package main

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/chirag/ariadne/sut/shared"
)

//go:embed templates/*.html
var templateFS embed.FS

var tmpl = template.Must(template.ParseFS(templateFS, "templates/*.html"))

// render executes the named content template first, then splices its output
// into layout.html as pre-escaped HTML. html/template's {{template}} action
// only accepts a string CONSTANT for the template name -- it cannot dispatch
// on a data field -- so a single layout wrapping a dynamically-chosen content
// block has to be composed in Go rather than in the template language itself.
func render(w http.ResponseWriter, logger *slog.Logger, page string, data map[string]any) {
	var contentBuf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&contentBuf, page+"-content", data); err != nil {
		logger.Error("content template render failed", "page", page, "error", err.Error())
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	data["Body"] = template.HTML(contentBuf.String()) //nolint:gosec // content is our own template output, not user input

	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "layout", data); err != nil {
		logger.Error("layout render failed", "page", page, "error", err.Error())
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = buf.WriteTo(w)
}

func searchAPIURL() string {
	if v := os.Getenv("SEARCH_API_URL"); v != "" {
		return v
	}
	return "http://search-api:8081"
}

func bookingAPIURL() string {
	if v := os.Getenv("BOOKING_API_URL"); v != "" {
		return v
	}
	return "http://booking-api:8083"
}

// JSON tags matter here: search-api emits snake_case field names, and Go's
// default case-insensitive struct matching does not ignore underscores, so
// FlightID/DepartAt/ArriveAt would silently decode as zero values without
// these (Amount/Currency/Airline/Origin/Destination happen to match anyway
// since they contain no underscore -- which is exactly what made this bug
// easy to miss in a partial test).
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

func main() {
	logger := shared.NewLogger("web-ui")
	client := &http.Client{Timeout: 5 * time.Second}
	uiVariantV2 := os.Getenv("UI_VARIANT") == "v2"

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", shared.HealthHandler("web-ui"))

	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		render(w, logger, "search", map[string]any{"UIVariantV2": uiVariantV2})
	})

	mux.HandleFunc("GET /search", func(w http.ResponseWriter, r *http.Request) {
		log := shared.LoggerFromRequest(logger, r)
		q := r.URL.Query()
		origin, destination, date := q.Get("origin"), q.Get("destination"), q.Get("date")

		data := map[string]any{"Origin": origin, "Destination": destination, "Date": date}

		req, _ := http.NewRequest(http.MethodGet, searchAPIURL()+"/api/v1/search?"+url.Values{
			"origin": {origin}, "destination": {destination}, "date": {date},
		}.Encode(), nil)
		shared.PropagateTraceHeader(req, shared.TraceID(r.Context()))

		resp, err := client.Do(req)
		if err != nil {
			log.Error("search-api call failed", "error", err.Error())
			data["Error"] = "Search is temporarily unavailable."
			render(w, logger, "results", data)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			data["Error"] = "No flights could be found for that route."
			render(w, logger, "results", data)
			return
		}
		var out struct {
			Offers []offer `json:"offers"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			log.Error("search-api response decode failed", "error", err.Error())
			data["Error"] = "Search is temporarily unavailable."
			render(w, logger, "results", data)
			return
		}
		data["Offers"] = out.Offers
		render(w, logger, "results", data)
	})

	mux.HandleFunc("GET /book", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		amount, _ := strconv.ParseFloat(q.Get("amount"), 64)
		render(w, logger, "book", map[string]any{
			"FlightID": q.Get("flight_id"), "Amount": amount, "Currency": q.Get("currency"),
			"Origin": q.Get("origin"), "Destination": q.Get("destination"), "DepartAt": q.Get("depart_at"),
		})
	})

	mux.HandleFunc("POST /book", func(w http.ResponseWriter, r *http.Request) {
		log := shared.LoggerFromRequest(logger, r)
		if err := r.ParseForm(); err != nil {
			http.Error(w, "invalid form", http.StatusBadRequest)
			return
		}
		amount, _ := strconv.ParseFloat(r.FormValue("amount"), 64)
		body, _ := json.Marshal(map[string]any{
			"flight_id":      r.FormValue("flight_id"),
			"passenger_name": r.FormValue("passenger_name"),
			"origin":         r.FormValue("origin"),
			"destination":    r.FormValue("destination"),
			"depart_at":      r.FormValue("depart_at"),
			"amount":         amount,
			"currency":       r.FormValue("currency"),
			"card_last4":     r.FormValue("card_last4"),
		})

		req, _ := http.NewRequest(http.MethodPost, bookingAPIURL()+"/api/v1/bookings", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		shared.PropagateTraceHeader(req, shared.TraceID(r.Context()))

		// Intentionally uses `client` (which has a timeout) rather than
		// booking-api's own internal call chain -- from the browser's point
		// of view under fault injection, this request will still hang until
		// booking-api itself gives up (which, per booking-api's own
		// deliberate defect, it never does). That end-to-end hang, visible
		// right here in the UI, is the thing Act 3 demonstrates.
		resp, err := client.Do(req)
		if err != nil {
			log.Error("booking-api call failed", "error", err.Error())
			render(w, logger, "confirmation", map[string]any{"Status": "FAILED", "Reason": "booking service unavailable"})
			return
		}
		defer resp.Body.Close()
		var out struct {
			BookingID string `json:"booking_id"`
			Status    string `json:"status"`
			Reason    string `json:"reason"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&out)
		render(w, logger, "confirmation", map[string]any{
			"BookingID": out.BookingID, "Status": out.Status, "Reason": out.Reason,
		})
	})

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	addr := fmt.Sprintf(":%s", port)
	srv := &http.Server{
		Addr:         addr,
		Handler:      shared.WithTracing(logger, mux),
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 15 * time.Second,
	}
	logger.Info("starting", "addr", addr, slog.Bool("ui_variant_v2", uiVariantV2))
	if err := srv.ListenAndServe(); err != nil {
		logger.Error("server exited", "error", err.Error())
		os.Exit(1)
	}
}

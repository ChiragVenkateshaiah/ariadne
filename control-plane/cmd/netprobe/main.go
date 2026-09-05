// Command netprobe is the runner image for VALIDATOR_KIND_NETWORK_CONFORMANCE.
// It attempts one TCP connection and reports the outcome via the
// ###ARIADNE-RESULT### sentinel every runner image honors (see
// internal/orchestrate). It is deliberately dumb: whether a given
// reachability result is EXPECTED (allowed vs. blocked) is a judgment the
// brain makes by comparing this against the World Model's CALLS edges --
// this binary only reports what actually happened on the wire.
package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strconv"
	"time"
)

type result struct {
	Target     string `json:"target"`
	Reachable  bool   `json:"reachable"`
	Error      string `json:"error,omitempty"`
	DurationMs int64  `json:"duration_ms"`
}

func main() {
	target := os.Getenv("ARIADNE_PROBE_TARGET") // "host:port"
	if target == "" {
		fmt.Fprintln(os.Stderr, "ARIADNE_PROBE_TARGET is required")
		os.Exit(1)
	}
	timeout := 2 * time.Second
	if ms, err := strconv.Atoi(os.Getenv("ARIADNE_PROBE_TIMEOUT_MS")); err == nil && ms > 0 {
		timeout = time.Duration(ms) * time.Millisecond
	}

	start := time.Now()
	conn, err := net.DialTimeout("tcp", target, timeout)
	r := result{Target: target, DurationMs: time.Since(start).Milliseconds()}
	if err != nil {
		r.Error = err.Error()
	} else {
		r.Reachable = true
		conn.Close()
	}

	b, _ := json.Marshal(r)
	fmt.Println(string(b)) // human-readable in `kubectl logs`
	fmt.Printf("###ARIADNE-RESULT###%s\n", string(b))
}

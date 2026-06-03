package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"time"
)

// fib uses naive recursion intentionally — the redundant calls make it
// CPU-bound, which is what drives HPA to scale Compute pods up.
func fib(n int) int {
	if n <= 1 {
		return n
	}
	return fib(n-1) + fib(n-2)
}

type computeRequest struct {
	N int `json:"n"`
}

type computeResponse struct {
	Result     int   `json:"result"`
	DurationMs int64 `json:"duration_ms"`
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	slog.Info("request",
		"method", r.Method,
		"path", r.URL.Path,
		"status", http.StatusOK,
		"duration_ms", time.Since(start).Milliseconds(),
	)
}

func computeHandler(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		slog.Warn("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", http.StatusMethodNotAllowed,
			"duration_ms", time.Since(start).Milliseconds(),
		)
		return
	}

	var req computeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		slog.Error("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", http.StatusBadRequest,
			"error", err.Error(),
			"duration_ms", time.Since(start).Milliseconds(),
		)
		return
	}

	result := fib(req.N)
	durationMs := time.Since(start).Milliseconds()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(computeResponse{
		Result:     result,
		DurationMs: durationMs,
	})

	slog.Info("request",
		"method", r.Method,
		"path", r.URL.Path,
		"status", http.StatusOK,
		"n", req.N,
		"result", result,
		"duration_ms", durationMs,
	)
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", healthHandler)
	mux.HandleFunc("/compute", computeHandler)

	slog.Info("starting", "port", port)
	if err := http.ListenAndServe(":"+port, mux); err != nil {
		slog.Error("server failed", "error", err.Error())
		os.Exit(1)
	}
}

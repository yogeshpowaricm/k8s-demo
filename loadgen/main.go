package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strconv"
	"sync"
	"time"
)

// ---- config ----------------------------------------------------------------

type config struct {
	targetURL string
	rps       int
	workers   int
	payloadN  int
	dataDir   string
}

func loadConfig() config {
	return config{
		targetURL: getEnv("TARGET_URL", "http://api-service.demo:3000"),
		rps:       getEnvInt("TARGET_RPS", 10),
		workers:   getEnvInt("WORKERS", 5),
		payloadN:  getEnvInt("PAYLOAD_N", 40),
		dataDir:   getEnv("DATA_DIR", "/data"),
	}
}

// ---- persistent totals (PVC) -----------------------------------------------

type persistedTotals struct {
	TotalSent    int `json:"total_sent"`
	TotalOK      int `json:"total_ok"`
	TotalErrors  int `json:"total_errors"`
}

func totalsPath(dataDir string) string {
	return dataDir + "/totals.json"
}

func loadTotals(dataDir string) persistedTotals {
	path := totalsPath(dataDir)
	b, err := os.ReadFile(path)
	if err != nil {
		return persistedTotals{}
	}
	var t persistedTotals
	if err := json.Unmarshal(b, &t); err != nil {
		slog.Warn("totals_load_error", "path", path, "error", err.Error())
		return persistedTotals{}
	}
	slog.Info("totals_loaded", "path", path, "total_sent", t.TotalSent)
	return t
}

func saveTotals(dataDir string, t persistedTotals) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		slog.Warn("totals_save_error", "error", err.Error())
		return
	}
	b, err := json.Marshal(t)
	if err != nil {
		slog.Warn("totals_save_error", "error", err.Error())
		return
	}
	if err := os.WriteFile(totalsPath(dataDir), b, 0o644); err != nil {
		slog.Warn("totals_save_error", "error", err.Error())
	}
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getEnvInt(key string, def int) int {
	if s := os.Getenv(key); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			return n
		}
	}
	return def
}

// ---- per-second stats ------------------------------------------------------

type bucket struct {
	mu        sync.Mutex
	sent      int
	ok        int
	errors    int
	latencies []int64
}

func (b *bucket) record(success bool, ms int64) {
	b.mu.Lock()
	b.sent++
	if success {
		b.ok++
	} else {
		b.errors++
	}
	b.latencies = append(b.latencies, ms)
	b.mu.Unlock()
}

func (b *bucket) flush() (sent, ok, errors int, p50, p99 int64) {
	b.mu.Lock()
	sent, ok, errors = b.sent, b.ok, b.errors
	lats := b.latencies
	b.sent, b.ok, b.errors = 0, 0, 0
	b.latencies = nil
	b.mu.Unlock()

	if len(lats) > 0 {
		sort.Slice(lats, func(i, j int) bool { return lats[i] < lats[j] })
		p50 = lats[len(lats)*50/100]
		p99 = lats[min(len(lats)*99/100, len(lats)-1)]
	}
	return
}

// ---- worker pool -----------------------------------------------------------

func runWorkers(cfg config, b *bucket) {
	jobs := make(chan struct{}, cfg.workers*2)

	// Dispatcher: emit one token per 1/RPS interval.
	go func() {
		interval := time.Second / time.Duration(cfg.rps)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for range ticker.C {
			jobs <- struct{}{}
		}
	}()

	payload := fmt.Sprintf(`{"n":%d}`, cfg.payloadN)
	client := &http.Client{Timeout: 15 * time.Second}
	url := cfg.targetURL + "/api/compute"

	for i := 0; i < cfg.workers; i++ {
		go func() {
			for range jobs {
				start := time.Now()
				resp, err := client.Post(url, "application/json",
					bytes.NewBufferString(payload))
				ms := time.Since(start).Milliseconds()

				success := err == nil && resp != nil && resp.StatusCode < 400
				b.record(success, ms)
				if resp != nil {
					resp.Body.Close()
				}
			}
		}()
	}
}

// ---- stats logger ----------------------------------------------------------

func runStatsLogger(b *bucket, dataDir string, totals *persistedTotals, totalsMu *sync.Mutex) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for range ticker.C {
		sent, ok, errors, p50, p99 := b.flush()
		slog.Info("stats",
			"sent", sent,
			"ok", ok,
			"errors", errors,
			"p50_ms", p50,
			"p99_ms", p99,
		)

		if sent > 0 {
			totalsMu.Lock()
			totals.TotalSent += sent
			totals.TotalOK += ok
			totals.TotalErrors += errors
			saveTotals(dataDir, *totals)
			slog.Info("totals_persisted",
				"total_sent", totals.TotalSent,
				"total_ok", totals.TotalOK,
				"total_errors", totals.TotalErrors,
			)
			totalsMu.Unlock()
		}
	}
}

// ---- k8s pod-count poller --------------------------------------------------

const (
	k8sAPIBase  = "https://kubernetes.default.svc"
	tokenPath   = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	k8sNamespace = "demo"
)

func readToken() (string, error) {
	b, err := os.ReadFile(tokenPath)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// podList is the minimal slice of the k8s PodList we need.
type podList struct {
	Items []struct {
		Status struct {
			Phase string `json:"phase"`
		} `json:"status"`
	} `json:"items"`
}

func countRunningPods(client *http.Client, token, app string) (int, error) {
	url := fmt.Sprintf("%s/api/v1/namespaces/%s/pods?labelSelector=app%%3D%s",
		k8sAPIBase, k8sNamespace, app)

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	var pl podList
	if err := json.NewDecoder(resp.Body).Decode(&pl); err != nil {
		return 0, err
	}

	count := 0
	for _, item := range pl.Items {
		if item.Status.Phase == "Running" {
			count++
		}
	}
	return count, nil
}

func runPodPoller() {
	token, err := readToken()
	if err != nil {
		slog.Warn("k8s_poller_disabled", "reason", "no service account token", "path", tokenPath)
		return
	}

	// Skip TLS verification — minikube's self-signed CA is fine for a demo.
	k8sClient := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
		},
	}

	prev := map[string]int{}

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		for _, svc := range []string{"api", "compute"} {
			n, err := countRunningPods(k8sClient, token, svc)
			if err != nil {
				slog.Warn("k8s_poll_error", "service", svc, "error", err.Error())
				continue
			}
			if p, seen := prev[svc]; seen && n != p {
				slog.Info("scale_event", "service", svc, "pods", n, "prev", p)
			}
			prev[svc] = n
		}
	}
}

// ---- main ------------------------------------------------------------------

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	cfg := loadConfig()
	totals := loadTotals(cfg.dataDir)
	var totalsMu sync.Mutex

	slog.Info("starting",
		"target", cfg.targetURL,
		"rps", cfg.rps,
		"workers", cfg.workers,
		"payload_n", cfg.payloadN,
		"data_dir", cfg.dataDir,
		"total_sent", totals.TotalSent,
	)

	b := &bucket{}
	go runWorkers(cfg, b)
	go runPodPoller()
	runStatsLogger(b, cfg.dataDir, &totals, &totalsMu) // blocks forever
}

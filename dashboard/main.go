package main

import (
	_ "embed"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed web/index.html
var indexHTML []byte

type AppConfig struct {
	Port            string
	ServerAPIURL    string
	APIToken        string
	TargetContainer string
	CustomLogPath   string
	PollInterval    time.Duration
}

func loadConfig() AppConfig {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	apiURL := os.Getenv("SERVER_API_URL")
	if apiURL == "" {
		apiURL = "https://satisfactory-server:7777"
	}
	apiURL = strings.TrimSuffix(apiURL, "/")

	containerName := os.Getenv("TARGET_CONTAINER")
	if containerName == "" {
		containerName = "satisfactory-server"
	}

	pollSec := 5
	if s := os.Getenv("POLL_INTERVAL"); s != "" {
		if val, err := strconv.Atoi(s); err == nil && val > 0 {
			pollSec = val
		}
	}

	return AppConfig{
		Port:            port,
		ServerAPIURL:    apiURL,
		APIToken:        os.Getenv("API_TOKEN"),
		TargetContainer: containerName,
		CustomLogPath:   os.Getenv("LOG_FILE_PATH"),
		PollInterval:    time.Duration(pollSec) * time.Second,
	}
}

type ServerStateSummary struct {
	ServerHealthy     bool          `json:"serverHealthy"`
	SessionName       string        `json:"sessionName"`
	Latest            MetricPoint   `json:"latest"`
	History           []MetricPoint `json:"history"`
	OnlinePlayerNames []string      `json:"onlinePlayerNames"`
	LastSave          SaveInfo      `json:"lastSave"`
}

type ServerApp struct {
	config       AppConfig
	apiClient    *APIClient
	collector    *Collector
	logParser    *LogParser
	savesScanner *SavesScanner
	mu           sync.RWMutex
	lastState    ServerStateSummary
}

func main() {
	cfg := loadConfig()
	log.Printf("[Dashboard] Starting Satisfactory Web Dashboard on port :%s", cfg.Port)
	log.Printf("[Dashboard] Monitoring container: %s | Server API: %s", cfg.TargetContainer, cfg.ServerAPIURL)

	customPaths := []string{}
	if cfg.CustomLogPath != "" {
		customPaths = append(customPaths, cfg.CustomLogPath)
	}

	app := &ServerApp{
		config:       cfg,
		apiClient:    NewAPIClient(cfg.ServerAPIURL, cfg.APIToken),
		collector:    NewCollector(cfg.TargetContainer, 180),
		logParser:    NewLogParser(customPaths, 250),
		savesScanner: NewSavesScanner(),
	}

	// Initial log scan
	app.logParser.ProcessLogs()

	// Start background workers
	go app.startLogTailer()
	go app.startMetricsCollector()

	// HTTP Routes
	mux := http.NewServeMux()
	mux.HandleFunc("/api/stats", app.handleStats)
	mux.HandleFunc("/api/events", app.handleEvents)
	mux.HandleFunc("/api/saves", app.handleSaves)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("/", app.handleIndex)

	server := &http.Server{
		Addr:         ":" + cfg.Port,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("[Dashboard] Server error: %v", err)
	}
}

func (app *ServerApp) startLogTailer() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		app.logParser.ProcessLogs()
	}
}

func (app *ServerApp) startMetricsCollector() {
	ticker := time.NewTicker(app.config.PollInterval)
	defer ticker.Stop()

	// Run initial collection immediately
	app.sampleMetrics()

	for range ticker.C {
		app.sampleMetrics()
	}
}

func (app *ServerApp) sampleMetrics() {
	// 1. Health check Satisfactory HTTPS API
	isHealthy, _, _ := app.apiClient.CheckHealth()

	// 2. Query server state if token configured
	var sessionName string
	var apiPlayerCount = -1

	if app.config.APIToken != "" {
		if stateResp, err := app.apiClient.QueryState(); err == nil && stateResp != nil {
			isHealthy = true
			sessionName = stateResp.Data.ServerGameState.ActiveSessionName
			apiPlayerCount = stateResp.Data.ServerGameState.NumConnectedPlayers
		}
	}

	// 3. Fallback / supplementary player info from logs
	onlinePlayers := app.logParser.GetOnlinePlayers()
	playerCount := len(onlinePlayers)
	if apiPlayerCount >= 0 {
		playerCount = apiPlayerCount
	}

	// 4. Sample CPU and RAM
	pt := app.collector.CollectStats(playerCount, isHealthy)

	// 5. Update cached state
	lastSave := app.logParser.GetLastSave()

	app.mu.Lock()
	app.lastState = ServerStateSummary{
		ServerHealthy:     isHealthy,
		SessionName:       sessionName,
		Latest:            pt,
		History:           app.collector.GetHistory(),
		OnlinePlayerNames: onlinePlayers,
		LastSave:          lastSave,
	}
	app.mu.Unlock()
}

func (app *ServerApp) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write(indexHTML)
}

func (app *ServerApp) handleStats(w http.ResponseWriter, r *http.Request) {
	app.mu.RLock()
	state := app.lastState
	app.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(state)
}

func (app *ServerApp) handleEvents(w http.ResponseWriter, r *http.Request) {
	sinceID := 0
	if s := r.URL.Query().Get("since"); s != "" {
		sinceID, _ = strconv.Atoi(s)
	}

	events := app.logParser.GetEvents(sinceID)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(events)
}

func (app *ServerApp) handleSaves(w http.ResponseWriter, r *http.Request) {
	limit := 10
	if s := r.URL.Query().Get("limit"); s != "" {
		if val, err := strconv.Atoi(s); err == nil && val > 0 {
			limit = val
		}
	}

	saves := app.savesScanner.ListSaves(limit)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(saves)
}


package main

import (
	_ "embed"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed web/index.html
var indexHTML []byte

type AppConfig struct {
	Port               string
	ServerAPIURL       string
	APIToken           string
	ServerPassword     string
	TargetContainer    string
	CustomLogPath      string
	DataDir            string
	PollInterval       time.Duration
	DefaultPlayerLimit int
	ServerName         string
}

// loadDotEnv parses .env files from candidate locations and injects variables into environment
func loadDotEnv() {
	candidates := []string{
		os.Getenv("ENV_FILE"),
		".env",
		"../.env",
		"/config/.env",
		"./satisfactory-server/.env",
		"/data/.env",
	}

	for _, path := range candidates {
		if path == "" {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		lines := strings.Split(string(data), "\n")
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			parts := strings.SplitN(line, "=", 2)
			if len(parts) != 2 {
				continue
			}
			key := strings.TrimSpace(parts[0])
			val := strings.TrimSpace(parts[1])
			val = strings.Trim(val, `"'`)
			if os.Getenv(key) == "" {
				_ = os.Setenv(key, val)
			}
		}
		log.Printf("[Dashboard] Loaded environment settings from %s", path)
	}
}

// getServerPassword extracts the server password from various environment keys
func getServerPassword() string {
	candidates := []string{
		"server_password",
		"SERVER_PASSWORD",
		"Server_Password",
		"PASSWORD",
		"ADMIN_PASSWORD",
		"admin_password",
	}
	for _, key := range candidates {
		if val := os.Getenv(key); val != "" {
			return val
		}
	}
	for _, env := range os.Environ() {
		parts := strings.SplitN(env, "=", 2)
		if len(parts) == 2 && strings.EqualFold(parts[0], "server_password") && parts[1] != "" {
			return parts[1]
		}
	}
	return ""
}

func loadConfig() AppConfig {
	loadDotEnv()

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

	dataDir := os.Getenv("DATA_DIR")
	if dataDir == "" {
		dataDir = "/data"
	}

	pollSec := 5
	if s := os.Getenv("POLL_INTERVAL"); s != "" {
		if val, err := strconv.Atoi(s); err == nil && val > 0 {
			pollSec = val
		}
	}

	maxPlayers := 4
	if s := os.Getenv("MAXPLAYERS"); s != "" {
		if val, err := strconv.Atoi(s); err == nil && val > 0 {
			maxPlayers = val
		}
	}

	serverName := os.Getenv("SERVER_NAME")
	if serverName == "" {
		serverName = os.Getenv("SERVERNAME")
	}

	serverPass := getServerPassword()

	return AppConfig{
		Port:               port,
		ServerAPIURL:       apiURL,
		APIToken:           os.Getenv("API_TOKEN"),
		ServerPassword:     serverPass,
		TargetContainer:    containerName,
		CustomLogPath:      os.Getenv("LOG_FILE_PATH"),
		DataDir:            dataDir,
		PollInterval:       time.Duration(pollSec) * time.Second,
		DefaultPlayerLimit: maxPlayers,
		ServerName:         serverName,
	}
}

type ServerStateSummary struct {
	ServerHealthy     bool          `json:"serverHealthy"`
	IsGameRunning     bool          `json:"isGameRunning"`
	ServerName        string        `json:"serverName"`
	SessionName       string        `json:"sessionName"`
	GameName          string        `json:"gameName"`
	TechTier          int           `json:"techTier"`
	ActiveSchematic   string        `json:"activeSchematic"`
	GamePhase         string        `json:"gamePhase"`
	AverageTickRate   float64       `json:"averageTickRate"`
	TotalGameDuration float64       `json:"totalGameDuration"`
	ConnectedPlayers  int           `json:"connectedPlayers"`
	PlayerLimit       int           `json:"playerLimit"`
	EngineType        string        `json:"engineType"`
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
	log.Printf("[Dashboard] Monitoring container: %s | Server API: %s | Data Dir: %s", cfg.TargetContainer, cfg.ServerAPIURL, cfg.DataDir)
	if cfg.APIToken != "" {
		log.Printf("[Dashboard] Game API authentication: Configured via API_TOKEN")
	} else if cfg.ServerPassword != "" {
		log.Printf("[Dashboard] Game API authentication: Configured via server_password (PasswordLogin)")
	} else {
		log.Printf("[Dashboard] Game API authentication: Auto-login (PasswordlessLogin / local access)")
	}

	customPaths := []string{}
	if cfg.CustomLogPath != "" {
		customPaths = append(customPaths, cfg.CustomLogPath)
	}

	app := &ServerApp{
		config:       cfg,
		apiClient:    NewAPIClient(cfg.ServerAPIURL, cfg.APIToken, cfg.ServerPassword),
		collector:    NewCollector(cfg.TargetContainer, cfg.DataDir),
		logParser:    NewLogParser(customPaths, 250),
		savesScanner: NewSavesScanner(),
	}

	// Initial log scan
	app.logParser.ProcessLogs()

	// Start background workers
	go app.startLogTailer()
	go app.startMetricsCollector()
	go app.startPersistenceWorker()

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
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		if n := app.logParser.ProcessLogs(); n > 0 {
			// Immediately sample metrics so player join/leave events update the graph immediately
			app.sampleMetrics()
		}
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

func (app *ServerApp) startPersistenceWorker() {
	ticker := time.NewTicker(3 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		app.collector.SaveHistoryToDisk()
	}
}

func (app *ServerApp) sampleMetrics() {
	// 1. Health check Satisfactory HTTPS API
	isHealthy, _, _ := app.apiClient.CheckHealth()

	// 2. Query server state if token configured
	serverName := app.config.ServerName
	var sessionName string
	var apiPlayerCount = -1
	var isGameRunning bool
	var totalGameDuration float64
	var techTier int
	var activeSchematic string
	var gamePhase string
	var averageTickRate float64
	playerLimit := app.config.DefaultPlayerLimit
	if playerLimit <= 0 {
		playerLimit = 4
	}

	// 2. Query server state from Dedicated Server API
	if stateResp, err := app.apiClient.QueryState(); err == nil && stateResp != nil {
		isHealthy = true
		if stateResp.Data.ServerName != "" {
			serverName = stateResp.Data.ServerName
		} else if stateResp.Data.ServerGameState.ServerName != "" {
			serverName = stateResp.Data.ServerGameState.ServerName
		}
		sessionName = stateResp.Data.ServerGameState.ActiveSessionName
		apiPlayerCount = stateResp.Data.ServerGameState.NumConnectedPlayers
		if stateResp.Data.ServerGameState.PlayerLimit > 0 {
			playerLimit = stateResp.Data.ServerGameState.PlayerLimit
		}
		isGameRunning = stateResp.Data.ServerGameState.IsGameRunning
		totalGameDuration = stateResp.Data.ServerGameState.TotalGameDuration
		techTier = stateResp.Data.ServerGameState.TechTier
		activeSchematic = CleanSchematicName(stateResp.Data.ServerGameState.ActiveSchematic)
		gamePhase = CleanGamePhase(stateResp.Data.ServerGameState.GamePhase)
		averageTickRate = stateResp.Data.ServerGameState.AverageTickRate
	}

	if serverName == "" {
		if opts, err := app.apiClient.QueryServerOptions(); err == nil && opts != nil {
			for k, v := range opts {
				if strings.Contains(strings.ToLower(k), "servername") && v != "" {
					serverName = v
					break
				}
			}
		}
	}

	// 3. Fallback / supplementary session info from logs
	if sessionName == "" {
		sessionName = app.logParser.GetSessionName()
	}
	if !isGameRunning {
		isGameRunning = app.logParser.IsGameRunning()
	}
	if totalGameDuration == 0 {
		totalGameDuration = app.logParser.GetTotalGameDuration()
	}

	// 4. Player count must be strictly sourced from the API, NOT from logs
	playerCount := 0
	if apiPlayerCount >= 0 {
		playerCount = apiPlayerCount
	}

	onlinePlayers := app.logParser.GetOnlinePlayers()
	if playerCount == 0 && len(onlinePlayers) > 0 {
		// When API reports 0 players, clear stale log-derived player names
		app.logParser.ResetOnlinePlayers()
		onlinePlayers = nil
	}

	// 5. Sample CPU and RAM
	pt := app.collector.CollectStats(playerCount, isHealthy)

	// 6. Update cached state and fallback from save files
	lastSave := app.logParser.GetLastSave()
	diskSave := app.savesScanner.GetLatestSave()
	if diskSave != nil {
		if lastSave.LastSaveTime.IsZero() || diskSave.ModTime.After(lastSave.LastSaveTime) {
			lastSave.LastSaveTime = diskSave.ModTime
			lastSave.SaveName = diskSave.Name
		}
		if sessionName == "" {
			if diskSave.SessionName != "" {
				sessionName = diskSave.SessionName
			} else {
				sessionName = strings.TrimSuffix(diskSave.Name, filepath.Ext(diskSave.Name))
			}
		}
		if totalGameDuration == 0 && diskSave.PlayDurationSec > 0 {
			totalGameDuration = float64(diskSave.PlayDurationSec)
		}
	}

	// If session is known or players are connected, and server is healthy, mark game as running
	if !isGameRunning && isHealthy && (sessionName != "" || playerCount > 0) {
		isGameRunning = true
	}

	if serverName == "" {
		if app.config.TargetContainer != "" && app.config.TargetContainer != "satisfactory-server" {
			serverName = app.config.TargetContainer
		} else {
			serverName = "FICSIT Dedicated Server"
		}
	}

	app.mu.Lock()
	app.lastState = ServerStateSummary{
		ServerHealthy:     isHealthy,
		IsGameRunning:     isGameRunning,
		ServerName:        serverName,
		SessionName:       sessionName,
		GameName:          sessionName,
		TechTier:          techTier,
		ActiveSchematic:   activeSchematic,
		GamePhase:         gamePhase,
		AverageTickRate:   averageTickRate,
		TotalGameDuration: totalGameDuration,
		ConnectedPlayers:  playerCount,
		PlayerLimit:       playerLimit,
		EngineType:        app.collector.GetEngineType(),
		Latest:            pt,
		History:           app.collector.GetHistory("1h"),
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
	timeRange := r.URL.Query().Get("range")
	if timeRange == "" {
		timeRange = "1h"
	}

	app.mu.RLock()
	state := app.lastState
	app.mu.RUnlock()

	// Ensure lastSave has latest disk save if logs have not recorded one yet
	if state.LastSave.LastSaveTime.IsZero() {
		if diskSave := app.savesScanner.GetLatestSave(); diskSave != nil {
			state.LastSave.LastSaveTime = diskSave.ModTime
			state.LastSave.SaveName = diskSave.Name
		}
	}

	// Supply history for the requested time range (1h, 24h, 7d, 30d)
	state.History = app.collector.GetHistory(timeRange)

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

// CleanGamePhase converts raw Unreal Engine GamePhase asset paths to friendly names
func CleanGamePhase(raw string) string {
	if raw == "" || strings.EqualFold(raw, "none") || strings.EqualFold(raw, "null") {
		return "None"
	}
	s := strings.Trim(strings.TrimSpace(raw), "'\"")

	lower := strings.ToLower(s)
	if strings.Contains(lower, "phase_0") || strings.Contains(lower, "phase0") {
		return "Onboarding"
	}
	if strings.Contains(lower, "phase_1") || strings.Contains(lower, "phase1") {
		return "Phase 1"
	}
	if strings.Contains(lower, "phase_2") || strings.Contains(lower, "phase2") {
		return "Phase 2"
	}
	if strings.Contains(lower, "phase_3") || strings.Contains(lower, "phase3") {
		return "Phase 3"
	}
	if strings.Contains(lower, "phase_4") || strings.Contains(lower, "phase4") {
		return "Phase 4"
	}
	if strings.Contains(lower, "phase_5") || strings.Contains(lower, "phase5") {
		return "Phase 5"
	}

	// Fallback cleanup if custom or modded phase
	if idx := strings.LastIndex(s, "."); idx != -1 {
		s = s[idx+1:]
	} else if idx := strings.LastIndex(s, "/"); idx != -1 {
		s = s[idx+1:]
	}
	s = strings.Trim(s, "'\"")
	s = strings.TrimPrefix(s, "GP_")
	s = strings.TrimPrefix(s, "GamePhase_")
	s = strings.ReplaceAll(s, "_", " ")
	s = strings.TrimSpace(s)
	if s == "" {
		return "None"
	}
	return s
}

var knownMilestones = map[string]string{
	// Tutorial / Onboarding HUB Upgrades
	"schematic_tutorial_1": "HUB Upgrade 1",
	"schematic_tutorial_2": "HUB Upgrade 2",
	"schematic_tutorial_3": "HUB Upgrade 3",
	"schematic_tutorial_4": "HUB Upgrade 4",
	"schematic_tutorial_5": "HUB Upgrade 5",
	"schematic_tutorial_6": "HUB Upgrade 6",

	// Tier 1
	"schematic_1-1": "Base Building",
	"schematic_1-2": "Logistics",
	"schematic_1-3": "Field Research",

	// Tier 2
	"schematic_2-1": "Part Assembly",
	"schematic_2-2": "Obstacle Clearing",
	"schematic_2-3": "Jump Pads",
	"schematic_2-4": "Resource Sink Bonus Program",

	// Tier 3
	"schematic_3-1": "Coal Power",
	"schematic_3-2": "Vehicular Transport",
	"schematic_3-3": "Basic Steel Production",

	// Tier 4
	"schematic_4-1": "Advanced Steel Production",
	"schematic_4-2": "Expanded Power Infrastructure",

	// Tier 5
	"schematic_5-1": "Oil Processing",
	"schematic_5-2": "Industrial Manufacturing",
	"schematic_5-3": "Alternative Fluid Transport",

	// Tier 6
	"schematic_6-1": "Gas Power",
	"schematic_6-2": "Expanded Packaging",
	"schematic_6-3": "Pipeline Engineering Mk.2",

	// Tier 7
	"schematic_7-1": "Bauxite Refinement",
	"schematic_7-2": "Logistics Mk.5",
	"schematic_7-3": "Aeronautical Engineering",

	// Tier 8
	"schematic_8-1": "Nuclear Power",
	"schematic_8-2": "Advanced Aluminum Production",
	"schematic_8-3": "Leading-Edge Production",
	"schematic_8-4": "Particle Enrichment",

	// Tier 9
	"schematic_9-1": "Matter Conversion",
	"schematic_9-2": "Quantum Encoding",
	"schematic_9-3": "Spatial Energy Regulation",
	"schematic_9-4": "Peak Efficiency",
}

// CleanSchematicName converts raw Unreal Engine schematic asset paths to clean milestone names
func CleanSchematicName(raw string) string {
	if raw == "" || strings.EqualFold(raw, "none") || strings.EqualFold(raw, "null") {
		return "None"
	}
	s := strings.Trim(strings.TrimSpace(raw), "'\"")

	// Extract base object identifier from Unreal path
	if idx := strings.LastIndex(s, "."); idx != -1 {
		s = s[idx+1:]
	} else if idx := strings.LastIndex(s, "/"); idx != -1 {
		s = s[idx+1:]
	}
	s = strings.Trim(s, "'\"")
	s = strings.TrimSuffix(s, "_C")

	lower := strings.ToLower(s)
	if name, ok := knownMilestones[lower]; ok {
		return name
	}

	// Fallback: strip "Schematic_" prefix and replace underscores with spaces
	s = strings.TrimPrefix(s, "Schematic_")
	s = strings.TrimPrefix(s, "schematic_")
	s = strings.ReplaceAll(s, "_", " ")
	s = strings.TrimSpace(s)
	if s == "" {
		return "None"
	}
	return s
}

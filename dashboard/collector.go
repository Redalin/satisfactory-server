package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// MetricPoint represents resource utilization at a point in time
type MetricPoint struct {
	Timestamp     time.Time `json:"timestamp"`
	CPUPercent    float64   `json:"cpuPercent"`
	MemoryUsedMB  float64   `json:"memoryUsedMB"`
	MemoryLimitMB float64   `json:"memoryLimitMB"`
	MemoryPercent float64   `json:"memoryPercent"`
	PlayerCount   int       `json:"playerCount"`
	ServerHealthy bool      `json:"serverHealthy"`
}

// Collector periodically samples CPU, memory, and status metrics with 30-day retention
type Collector struct {
	mu              sync.RWMutex
	recentHistory   []MetricPoint // High-res buffer for the last ~1-2 hours (~720 points)
	longTermHistory []MetricPoint // Long-term history (downsampled to ~5-15 min buckets, retained for 30 days)
	containerName   string
	dataFilePath    string
	dockerClient    *http.Client
	hasDockerSock   bool
	engineType      string // "Docker", "Podman", "PID (Shared Namespace)", or "Host"
	socketPath      string
	prevHostTotal   uint64
	prevHostIdle    uint64
	prevProcTicks   uint64
	prevProcTime    time.Time
	lastLongTermAdd time.Time
}

// NewCollector initializes the resource collector with auto-detection for Docker, Podman, and PID namespaces
func NewCollector(containerName, dataDir string) *Collector {
	sockPath, dClient, engine := detectContainerSocket()
	hasSock := dClient != nil

	if hasSock {
		log.Printf("[Collector] Connected to %s socket at %s", engine, sockPath)
	} else {
		log.Printf("[Collector] No container socket found. Will monitor via shared PID namespace (/proc) or system metrics.")
	}

	// Determine data storage directory
	if dataDir == "" {
		dataDir = "/data"
	}
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		// Fallback to local ./data or /tmp if /data is not writable
		dataDir = "./data"
		if err := os.MkdirAll(dataDir, 0755); err != nil {
			dataDir = "/tmp"
		}
	}
	dataPath := filepath.Join(dataDir, "metrics_history.json")

	c := &Collector{
		recentHistory:   make([]MetricPoint, 0, 720),
		longTermHistory: make([]MetricPoint, 0, 4500), // ~30 days @ 10-minute intervals
		containerName:   containerName,
		dataFilePath:    dataPath,
		dockerClient:    dClient,
		hasDockerSock:   hasSock,
		socketPath:      sockPath,
		engineType:      engine,
	}

	c.loadHistoryFromDisk()
	return c
}

// detectContainerSocket probes common socket paths and identifies Docker vs Podman
func detectContainerSocket() (string, *http.Client, string) {
	candidates := []string{
		os.Getenv("CONTAINER_SOCKET"),
		os.Getenv("DOCKER_SOCKET"),
	}

	if h := os.Getenv("DOCKER_HOST"); strings.HasPrefix(h, "unix://") {
		candidates = append(candidates, strings.TrimPrefix(h, "unix://"))
	}

	candidates = append(candidates,
		"/var/run/docker.sock",
		"/run/podman/podman.sock",
		"/var/run/podman/podman.sock",
		"/run/docker.sock",
	)

	// Check for any user rootless podman sockets: e.g. /run/user/1000/podman/podman.sock
	if matches, err := filepath.Glob("/run/user/*/podman/podman.sock"); err == nil && len(matches) > 0 {
		candidates = append(candidates, matches...)
	}

	for _, p := range candidates {
		if p == "" {
			continue
		}
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		// Skip regular files if accidentally created as a file
		if fi.Mode()&os.ModeSocket == 0 && fi.Mode().IsRegular() {
			continue
		}

		targetPath := p
		client := &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, "unix", targetPath)
				},
			},
			Timeout: 3 * time.Second,
		}

		// Query version to test connectivity and detect Docker vs Podman
		engine := probeEngine(client)
		if engine != "" {
			return targetPath, client, engine
		}
	}

	return "", nil, ""
}

// probeEngine queries the daemon's /version endpoint to detect Docker or Podman
func probeEngine(client *http.Client) string {
	resp, err := client.Get("http://localhost/version")
	if err != nil {
		if pingResp, pingErr := client.Get("http://localhost/_ping"); pingErr == nil {
			pingResp.Body.Close()
			return "Docker"
		}
		return ""
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return ""
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "Container Engine"
	}
	bodyStr := strings.ToLower(string(body))

	if strings.Contains(bodyStr, "podman") {
		return "Podman"
	}
	if strings.Contains(bodyStr, "docker") {
		return "Docker"
	}
	return "Container Engine"
}

// loadHistoryFromDisk loads existing 30-day metrics on startup
func (c *Collector) loadHistoryFromDisk() {
	data, err := os.ReadFile(c.dataFilePath)
	if err != nil {
		return
	}

	var loaded []MetricPoint
	if err := json.Unmarshal(data, &loaded); err != nil {
		log.Printf("[Collector] Warning: could not parse existing %s: %v", c.dataFilePath, err)
		return
	}

	// Filter out points older than 30 days
	cutoff := time.Now().Add(-30 * 24 * time.Hour)
	valid := make([]MetricPoint, 0, len(loaded))
	for _, pt := range loaded {
		if pt.Timestamp.After(cutoff) {
			valid = append(valid, pt)
		}
	}

	c.longTermHistory = valid
	log.Printf("[Collector] Loaded %d historical metric points from %s", len(valid), c.dataFilePath)
}

// SaveHistoryToDisk persists long-term history to disk atomically
func (c *Collector) SaveHistoryToDisk() {
	c.mu.RLock()
	dataCopy := make([]MetricPoint, len(c.longTermHistory))
	copy(dataCopy, c.longTermHistory)
	c.mu.RUnlock()

	if len(dataCopy) == 0 {
		return
	}

	encoded, err := json.Marshal(dataCopy)
	if err != nil {
		return
	}

	tmpFile := c.dataFilePath + ".tmp"
	if err := os.WriteFile(tmpFile, encoded, 0644); err != nil {
		return
	}
	_ = os.Rename(tmpFile, c.dataFilePath)
}

// AddPoint records a metric point and manages tiered aggregation for up to 30 days
func (c *Collector) AddPoint(p MetricPoint) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// 1. High-resolution recent buffer (last ~1 hour, up to 720 points @ 5s)
	if len(c.recentHistory) >= 720 {
		c.recentHistory = c.recentHistory[1:]
	}
	c.recentHistory = append(c.recentHistory, p)

	// 2. Long-term buffer (every 5 minutes or whenever player count changes)
	now := p.Timestamp
	playerCountChanged := len(c.longTermHistory) > 0 && c.longTermHistory[len(c.longTermHistory)-1].PlayerCount != p.PlayerCount
	if c.lastLongTermAdd.IsZero() || playerCountChanged || now.Sub(c.lastLongTermAdd) >= 5*time.Minute {
		c.lastLongTermAdd = now

		// Remove points older than 30 days
		cutoff := now.Add(-30 * 24 * time.Hour)
		if len(c.longTermHistory) > 0 && c.longTermHistory[0].Timestamp.Before(cutoff) {
			idx := 0
			for idx < len(c.longTermHistory) && c.longTermHistory[idx].Timestamp.Before(cutoff) {
				idx++
			}
			c.longTermHistory = c.longTermHistory[idx:]
		}

		c.longTermHistory = append(c.longTermHistory, p)
	}
}

// GetLatest returns the most recent metric point
func (c *Collector) GetLatest() (MetricPoint, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if len(c.recentHistory) == 0 {
		return MetricPoint{}, false
	}
	return c.recentHistory[len(c.recentHistory)-1], true
}

// GetHistory returns historical metrics downsampled for the requested timescale ("1h", "24h", "7d", "30d")
func (c *Collector) GetHistory(timeRange string) []MetricPoint {
	c.mu.RLock()
	defer c.mu.RUnlock()

	now := time.Now()
	var duration time.Duration
	var targetPoints int

	switch timeRange {
	case "24h", "1d":
		duration = 24 * time.Hour
		targetPoints = 120
	case "7d", "1w":
		duration = 7 * 24 * time.Hour
		targetPoints = 140
	case "30d", "1m":
		duration = 30 * 24 * time.Hour
		targetPoints = 150
	case "1h":
		fallthrough
	default:
		duration = 1 * time.Hour
		targetPoints = 120
	}

	cutoff := now.Add(-duration)

	// If 1h is requested and recentHistory has sufficient points, use high-res recent history
	if duration <= 1*time.Hour && len(c.recentHistory) > 0 {
		var filtered []MetricPoint
		for _, pt := range c.recentHistory {
			if pt.Timestamp.After(cutoff) {
				filtered = append(filtered, pt)
			}
		}
		if len(filtered) > targetPoints {
			return downsample(filtered, targetPoints)
		}
		return filtered
	}

	// For longer time ranges, combine longTermHistory and recentHistory
	mergedMap := make(map[int64]MetricPoint)
	for _, pt := range c.longTermHistory {
		if pt.Timestamp.After(cutoff) {
			mergedMap[pt.Timestamp.Unix()] = pt
		}
	}
	// Also include recent points so current moments are included
	for _, pt := range c.recentHistory {
		if pt.Timestamp.After(cutoff) {
			mergedMap[pt.Timestamp.Unix()] = pt
		}
	}

	combined := make([]MetricPoint, 0, len(mergedMap))
	for _, pt := range mergedMap {
		combined = append(combined, pt)
	}

	sort.Slice(combined, func(i, j int) bool {
		return combined[i].Timestamp.Before(combined[j].Timestamp)
	})

	if len(combined) <= targetPoints {
		return combined
	}

	return downsample(combined, targetPoints)
}

// downsample groups data points into buckets to maintain fast rendering across long timescales
func downsample(pts []MetricPoint, targetCount int) []MetricPoint {
	if len(pts) <= targetCount || targetCount <= 0 {
		return pts
	}

	result := make([]MetricPoint, 0, targetCount)
	bucketSize := float64(len(pts)) / float64(targetCount)

	for i := 0; i < targetCount; i++ {
		startIdx := int(float64(i) * bucketSize)
		endIdx := int(float64(i+1) * bucketSize)
		if endIdx > len(pts) {
			endIdx = len(pts)
		}
		if startIdx >= endIdx {
			continue
		}

		var sumCPU, sumMemMB, sumMemLimit, sumMemPercent float64
		maxPlayers := 0
		healthy := true
		count := float64(endIdx - startIdx)

		for j := startIdx; j < endIdx; j++ {
			p := pts[j]
			sumCPU += p.CPUPercent
			sumMemMB += p.MemoryUsedMB
			sumMemLimit += p.MemoryLimitMB
			sumMemPercent += p.MemoryPercent
			if p.PlayerCount > maxPlayers {
				maxPlayers = p.PlayerCount
			}
			if !p.ServerHealthy {
				healthy = false
			}
		}

		// Use middle timestamp of bucket
		midIdx := startIdx + (endIdx-startIdx)/2
		result = append(result, MetricPoint{
			Timestamp:     pts[midIdx].Timestamp,
			CPUPercent:    round2(sumCPU / count),
			MemoryUsedMB:  round2(sumMemMB / count),
			MemoryLimitMB: round2(sumMemLimit / count),
			MemoryPercent: round2(sumMemPercent / count),
			PlayerCount:   maxPlayers,
			ServerHealthy: healthy,
		})
	}

	return result
}

// CollectStats gathers current stats from Docker/Podman socket, shared PID namespace, or system fallbacks
func (c *Collector) CollectStats(playerCount int, isHealthy bool) MetricPoint {
	pt := MetricPoint{
		Timestamp:     time.Now(),
		PlayerCount:   playerCount,
		ServerHealthy: isHealthy,
	}

	// 1. Try Docker / Podman socket first if available
	if c.hasDockerSock && c.dockerClient != nil {
		if cpu, memUsed, memLimit, err := c.collectFromDocker(); err == nil {
			pt.CPUPercent = cpu
			pt.MemoryUsedMB = memUsed
			pt.MemoryLimitMB = memLimit
			if memLimit > 0 {
				pt.MemoryPercent = (memUsed / memLimit) * 100.0
			}
			c.AddPoint(pt)
			return pt
		}
	}

	// 2. Try shared PID namespace (/proc) to inspect FactoryServer directly
	if cpu, memUsed, memLimit, ok := c.collectFromProcesses(); ok {
		c.mu.Lock()
		c.engineType = "PID (Shared)"
		c.mu.Unlock()
		pt.CPUPercent = cpu
		pt.MemoryUsedMB = memUsed
		pt.MemoryLimitMB = memLimit
		if memLimit > 0 {
			pt.MemoryPercent = (memUsed / memLimit) * 100.0
		}
		c.AddPoint(pt)
		return pt
	}

	// 3. Fallback to /proc/stat and host meminfo
	cpu, memUsed, memLimit := c.collectFromSystem()
	c.mu.Lock()
	if c.engineType == "" {
		c.engineType = "Host"
	}
	c.mu.Unlock()
	pt.CPUPercent = cpu
	pt.MemoryUsedMB = memUsed
	pt.MemoryLimitMB = memLimit
	if memLimit > 0 {
		pt.MemoryPercent = (memUsed / memLimit) * 100.0
	}

	c.AddPoint(pt)
	return pt
}

// collectFromProcesses inspects the shared PID namespace (/proc) to directly measure FactoryServer
func (c *Collector) collectFromProcesses() (float64, float64, float64, bool) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0, 0, 0, false
	}

	var totalVmRSS float64
	var totalTicks uint64
	foundGameProc := false

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}

		commBytes, _ := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
		comm := strings.TrimSpace(string(commBytes))

		cmdlineBytes, _ := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		cmdline := string(cmdlineBytes)

		// Check if it is the FactoryServer dedicated server or game process
		isGame := strings.Contains(comm, "FactoryServer") ||
			strings.Contains(comm, "FactoryGame") ||
			strings.Contains(cmdline, "FactoryServer") ||
			strings.Contains(cmdline, "FactoryGame") ||
			(strings.Contains(cmdline, "satisfactory") && !strings.Contains(cmdline, "satisfactory-dashboard"))

		if !isGame {
			continue
		}

		foundGameProc = true

		// Read VmRSS from /proc/[pid]/status
		if statusBytes, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid)); err == nil {
			for _, line := range strings.Split(string(statusBytes), "\n") {
				if strings.HasPrefix(line, "VmRSS:") {
					fields := strings.Fields(line)
					if len(fields) >= 2 {
						if kb, err := strconv.ParseFloat(fields[1], 64); err == nil {
							totalVmRSS += kb / 1024.0
						}
					}
					break
				}
			}
		}

		// Read CPU ticks from /proc/[pid]/stat
		if statBytes, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
			s := string(statBytes)
			if idx := strings.LastIndex(s, ")"); idx != -1 && idx+2 < len(s) {
				fields := strings.Fields(s[idx+2:])
				if len(fields) > 12 {
					u, _ := strconv.ParseUint(fields[11], 10, 64)
					st, _ := strconv.ParseUint(fields[12], 10, 64)
					totalTicks += u + st
				}
			}
		}
	}

	if !foundGameProc {
		return 0, 0, 0, false
	}

	now := time.Now()
	var cpuPercent float64
	if !c.prevProcTime.IsZero() && c.prevProcTicks > 0 && totalTicks >= c.prevProcTicks {
		elapsedSec := now.Sub(c.prevProcTime).Seconds()
		if elapsedSec > 0 {
			tickDelta := totalTicks - c.prevProcTicks
			cpuPercent = (float64(tickDelta) / 100.0 / elapsedSec) * 100.0
		}
	}
	c.prevProcTicks = totalTicks
	c.prevProcTime = now

	// Memory Limit
	memLimitMB := 8192.0
	if envLimit := os.Getenv("SERVER_MEMORY_LIMIT_MB"); envLimit != "" {
		if val, err := strconv.ParseFloat(envLimit, 64); err == nil && val > 0 {
			memLimitMB = val
		}
	} else if data, err := os.ReadFile("/sys/fs/cgroup/memory.max"); err == nil {
		val := strings.TrimSpace(string(data))
		if val != "max" {
			if bytesVal, err := strconv.ParseUint(val, 10, 64); err == nil && bytesVal > 0 {
				memLimitMB = float64(bytesVal) / (1024 * 1024)
			}
		}
	}

	return round2(cpuPercent), round2(totalVmRSS), round2(memLimitMB), true
}

// GetEngineType returns the detected container or host monitoring mode
func (c *Collector) GetEngineType() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.engineType != "" {
		return c.engineType
	}
	if c.hasDockerSock {
		return "Container"
	}
	return "Host"
}

type dockerContainerStats struct {
	CPUStats struct {
		CPUUsage struct {
			TotalUsage uint64 `json:"total_usage"`
		} `json:"cpu_usage"`
		SystemCPUUsage uint64 `json:"system_cpu_usage"`
		OnlineCPUs     uint32 `json:"online_cpus"`
	} `json:"cpu_stats"`
	PreCPUStats struct {
		CPUUsage struct {
			TotalUsage uint64 `json:"total_usage"`
		} `json:"cpu_usage"`
		SystemCPUUsage uint64 `json:"system_cpu_usage"`
	} `json:"precpu_stats"`
	MemoryStats struct {
		Usage uint64 `json:"usage"`
		Limit uint64 `json:"limit"`
		Stats struct {
			Cache        uint64 `json:"cache"`
			InactiveFile uint64 `json:"inactive_file"`
		} `json:"stats"`
	} `json:"memory_stats"`
}

func (c *Collector) collectFromDocker() (float64, float64, float64, error) {
	url := fmt.Sprintf("http://localhost/containers/%s/stats?stream=false", c.containerName)
	resp, err := c.dockerClient.Get(url)
	if err != nil {
		return 0, 0, 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, 0, 0, fmt.Errorf("docker stats returned %d", resp.StatusCode)
	}

	var stats dockerContainerStats
	if err := json.NewDecoder(resp.Body).Decode(&stats); err != nil {
		return 0, 0, 0, err
	}

	// CPU %
	cpuDelta := float64(stats.CPUStats.CPUUsage.TotalUsage - stats.PreCPUStats.CPUUsage.TotalUsage)
	systemDelta := float64(stats.CPUStats.SystemCPUUsage - stats.PreCPUStats.SystemCPUUsage)
	onlineCPUs := float64(stats.CPUStats.OnlineCPUs)
	if onlineCPUs == 0 {
		onlineCPUs = 1
	}

	var cpuPercent float64
	if systemDelta > 0 && cpuDelta > 0 {
		cpuPercent = (cpuDelta / systemDelta) * onlineCPUs * 100.0
	}

	// Memory
	used := stats.MemoryStats.Usage
	cache := stats.MemoryStats.Stats.InactiveFile
	if cache == 0 {
		cache = stats.MemoryStats.Stats.Cache
	}
	if used > cache {
		used -= cache
	}

	memUsedMB := float64(used) / (1024 * 1024)
	memLimitMB := float64(stats.MemoryStats.Limit) / (1024 * 1024)

	return round2(cpuPercent), round2(memUsedMB), round2(memLimitMB), nil
}

func (c *Collector) collectFromSystem() (float64, float64, float64) {
	var memUsedMB, memLimitMB float64

	// cgroup v2
	if data, err := os.ReadFile("/sys/fs/cgroup/memory.current"); err == nil {
		if bytesVal, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64); err == nil {
			memUsedMB = float64(bytesVal) / (1024 * 1024)
		}
	}
	if data, err := os.ReadFile("/sys/fs/cgroup/memory.max"); err == nil {
		val := strings.TrimSpace(string(data))
		if val != "max" {
			if bytesVal, err := strconv.ParseUint(val, 10, 64); err == nil {
				memLimitMB = float64(bytesVal) / (1024 * 1024)
			}
		}
	}

	// Fallback to /proc/meminfo
	if memUsedMB == 0 {
		total, avail := readProcMeminfo()
		if total > 0 {
			memLimitMB = total
			memUsedMB = total - avail
		}
	}

	cpuPercent := c.readProcStatCPU()

	// Default reasonable memory limit (8GB) if unbounded
	if memLimitMB == 0 || memLimitMB > 1024*1024 {
		memLimitMB = 8192
	}

	return round2(cpuPercent), round2(memUsedMB), round2(memLimitMB)
}

func (c *Collector) readProcStatCPU() float64 {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) == 0 {
		return 0
	}

	fields := strings.Fields(lines[0])
	if len(fields) < 5 || fields[0] != "cpu" {
		return 0
	}

	var total uint64
	var idle uint64
	for i := 1; i < len(fields); i++ {
		val, _ := strconv.ParseUint(fields[i], 10, 64)
		total += val
		if i == 4 {
			idle = val
		}
	}

	if c.prevHostTotal == 0 {
		c.prevHostTotal = total
		c.prevHostIdle = idle
		return 0
	}

	totalDelta := total - c.prevHostTotal
	idleDelta := idle - c.prevHostIdle
	c.prevHostTotal = total
	c.prevHostIdle = idle

	if totalDelta == 0 {
		return 0
	}
	busy := float64(totalDelta - idleDelta)
	return round2((busy / float64(totalDelta)) * 100.0)
}

func readProcMeminfo() (totalMB, availMB float64) {
	file, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	defer file.Close()

	buf, err := io.ReadAll(file)
	if err != nil {
		return 0, 0
	}

	lines := strings.Split(string(buf), "\n")
	for _, l := range lines {
		parts := strings.Fields(l)
		if len(parts) >= 2 {
			if parts[0] == "MemTotal:" {
				kb, _ := strconv.ParseFloat(parts[1], 64)
				totalMB = kb / 1024
			} else if parts[0] == "MemAvailable:" {
				kb, _ := strconv.ParseFloat(parts[1], 64)
				availMB = kb / 1024
			}
		}
	}
	return totalMB, availMB
}

func round2(val float64) float64 {
	return float64(int(val*100+0.5)) / 100
}

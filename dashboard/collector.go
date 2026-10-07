package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// MetricPoint represents resource utilization at a point in time
type MetricPoint struct {
	Timestamp        time.Time `json:"timestamp"`
	CPUPercent       float64   `json:"cpuPercent"`
	MemoryUsedMB     float64   `json:"memoryUsedMB"`
	MemoryLimitMB    float64   `json:"memoryLimitMB"`
	MemoryPercent    float64   `json:"memoryPercent"`
	PlayerCount      int       `json:"playerCount"`
	ServerHealthy    bool      `json:"serverHealthy"`
}

// Collector periodically samples CPU, memory, and status metrics
type Collector struct {
	mu             sync.RWMutex
	history        []MetricPoint
	maxHistory     int
	containerName  string
	dockerClient   *http.Client
	hasDockerSock  bool
	prevHostTotal  uint64
	prevHostIdle   uint64
}

// NewCollector initializes the resource collector
func NewCollector(containerName string, maxHistory int) *Collector {
	if maxHistory <= 0 {
		maxHistory = 180 // ~15-30 minutes of historical data
	}

	_, err := os.Stat("/var/run/docker.sock")
	hasSock := err == nil

	var dClient *http.Client
	if hasSock {
		dClient = &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, "unix", "/var/run/docker.sock")
				},
			},
			Timeout: 4 * time.Second,
		}
	}

	return &Collector{
		history:       make([]MetricPoint, 0, maxHistory),
		maxHistory:    maxHistory,
		containerName: containerName,
		dockerClient:  dClient,
		hasDockerSock: hasSock,
	}
}

// AddPoint appends a metric point and maintains maxHistory capacity
func (c *Collector) AddPoint(p MetricPoint) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(c.history) >= c.maxHistory {
		c.history = c.history[1:]
	}
	c.history = append(c.history, p)
}

// GetHistory returns a copy of historical metrics
func (c *Collector) GetHistory() []MetricPoint {
	c.mu.RLock()
	defer c.mu.RUnlock()

	out := make([]MetricPoint, len(c.history))
	copy(out, c.history)
	return out
}

// GetLatest returns the most recent metric point
func (c *Collector) GetLatest() (MetricPoint, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if len(c.history) == 0 {
		return MetricPoint{}, false
	}
	return c.history[len(c.history)-1], true
}

// CollectStats gathers current stats from Docker socket or system fallbacks
func (c *Collector) CollectStats(playerCount int, isHealthy bool) MetricPoint {
	pt := MetricPoint{
		Timestamp:     time.Now(),
		PlayerCount:   playerCount,
		ServerHealthy: isHealthy,
	}

	// 1. Try Docker socket first if available
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

	// 2. Fallback to /proc or cgroups
	cpu, memUsed, memLimit := c.collectFromSystem()
	pt.CPUPercent = cpu
	pt.MemoryUsedMB = memUsed
	pt.MemoryLimitMB = memLimit
	if memLimit > 0 {
		pt.MemoryPercent = (memUsed / memLimit) * 100.0
	}

	c.AddPoint(pt)
	return pt
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
	// Remove page cache or inactive file if available
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
	// Attempt to read cgroup v2 / v1 memory
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
		if i == 4 { // idle is 4th field after "cpu"
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


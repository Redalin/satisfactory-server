package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// LogEvent represents a structured parsed event from server logs
type LogEvent struct {
	ID         int       `json:"id"`
	Timestamp  time.Time `json:"timestamp"`
	TimeString string    `json:"timeString"`
	Category   string    `json:"category"` // "player_join", "player_leave", "save", "server", "warning"
	Message    string    `json:"message"`
	Raw        string    `json:"raw"`
}

// SaveInfo holds information about the latest detected game save
type SaveInfo struct {
	LastSaveTime time.Time `json:"lastSaveTime"`
	SaveName     string    `json:"saveName"`
	DurationSec  float64   `json:"durationSec"`
}

// LogParser monitors and extracts key events from Satisfactory logs
type LogParser struct {
	mu            sync.RWMutex
	logPaths      []string
	activePath    string
	lastOffset    int64
	events        []LogEvent
	maxEvents     int
	nextID        int
	onlinePlayers map[string]time.Time
	lastSave      SaveInfo
}

var (
	// Regex for Unreal Engine timestamp: [2024.09.12-11.27.38:063][  0]
	reUETimestamp = regexp.MustCompile(`^\[(\d{4})\.(\d{2})\.(\d{2})-(\d{2})\.(\d{2})\.(\d{2}):(\d{3})\](?:\[\s*\d+\])?(.*)$`)

	// Regexes for key events
	rePlayerJoin1  = regexp.MustCompile(`Join succeeded:\s*([^\r\n]+)`)
	rePlayerJoin2  = regexp.MustCompile(`Player\s+([^\r\n]+?)\s+entered the game`)
	rePlayerLeave1 = regexp.MustCompile(`Client\s+([^\r\n ]+)\s+disconnected`)
	rePlayerLeave2 = regexp.MustCompile(`Closing connection for player\s*([^\r\n]+)`)
	rePlayerLeave3 = regexp.MustCompile(`PlayerName:\s*([^,\]\r\n]+).*Close`)

	reWorldSave = regexp.MustCompile(`World Save took\s+([0-9.]+)\s+seconds`)
	reAutoSave  = regexp.MustCompile(`Autosave took\s+([0-9.]+)\s+seconds`)
	reSaveTo    = regexp.MustCompile(`Saving to\s+([^\r\n]+)`)

	reServerAPI     = regexp.MustCompile(`Server API listening on\s*'([^']+)'`)
	reHttpListener  = regexp.MustCompile(`Created new HttpListener on\s*([^\r\n]+)`)
	reAppInstalled  = regexp.MustCompile(`Success!\s*App '1690800' fully installed`)
	reEngineStart   = regexp.MustCompile(`Running engine for game:\s*FactoryGame`)
	reCrashGUID     = regexp.MustCompile(`Session CrashGUID >\s*([A-Za-z0-9-]+)`)
)

// NewLogParser creates a log parser with candidate log paths
func NewLogParser(customPaths []string, maxEvents int) *LogParser {
	if maxEvents <= 0 {
		maxEvents = 200
	}

	paths := []string{
		"/config/gamefiles/FactoryGame/Saved/Logs/FactoryGame.log",
		"./satisfactory-server/gamefiles/FactoryGame/Saved/Logs/FactoryGame.log",
		"/config/server.log",
		"./server.log",
		"server.log",
	}
	if len(customPaths) > 0 {
		paths = append(customPaths, paths...)
	}

	return &LogParser{
		logPaths:      paths,
		events:        make([]LogEvent, 0, maxEvents),
		maxEvents:     maxEvents,
		nextID:        1,
		onlinePlayers: make(map[string]time.Time),
	}
}

// FindActiveLogFile locates the first available log file
func (p *LogParser) FindActiveLogFile() string {
	for _, path := range p.logPaths {
		if fi, err := os.Stat(path); err == nil && !fi.IsDir() {
			return path
		}
	}
	return ""
}

// ProcessLogs scans new lines from the active log file
func (p *LogParser) ProcessLogs() int {
	p.mu.Lock()
	defer p.mu.Unlock()

	active := p.FindActiveLogFile()
	if active == "" {
		return 0
	}

	// Detect if log file changed or rotated
	if active != p.activePath {
		p.activePath = active
		p.lastOffset = 0
	}

	file, err := os.Open(p.activePath)
	if err != nil {
		return 0
	}
	defer file.Close()

	fi, err := file.Stat()
	if err != nil {
		return 0
	}

	// Handle log truncation or rotation
	if fi.Size() < p.lastOffset {
		p.lastOffset = 0
	}

	// If reading for the first time and the file is very large (> 2MB), start near the end
	if p.lastOffset == 0 && fi.Size() > 2*1024*1024 {
		p.lastOffset = fi.Size() - (512 * 1024)
	}

	if _, err := file.Seek(p.lastOffset, io.SeekStart); err != nil {
		return 0
	}

	scanner := bufio.NewScanner(file)
	// Support long log lines
	buf := make([]byte, 64*1024)
	scanner.Buffer(buf, 1024*1024)

	newEventsCount := 0

	for scanner.Scan() {
		line := scanner.Text()
		if evt := p.parseLine(line); evt != nil {
			p.addEventLocked(*evt)
			newEventsCount++
		}
	}

	newOffset, err := file.Seek(0, io.SeekCurrent)
	if err == nil {
		p.lastOffset = newOffset
	}

	return newEventsCount
}

func (p *LogParser) parseLine(line string) *LogEvent {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return nil
	}

	eventTime := time.Now()
	msg := trimmed

	if match := reUETimestamp.FindStringSubmatch(trimmed); len(match) == 8 {
		year, _ := strconv.Atoi(match[1])
		month, _ := strconv.Atoi(match[2])
		day, _ := strconv.Atoi(match[3])
		hour, _ := strconv.Atoi(match[4])
		min, _ := strconv.Atoi(match[5])
		sec, _ := strconv.Atoi(match[6])
		ms, _ := strconv.Atoi(match[7])

		parsed := time.Date(year, time.Month(month), day, hour, min, sec, ms*1000000, time.UTC)
		if !parsed.IsZero() {
			eventTime = parsed
		}
		msg = strings.TrimSpace(match[8])
	}

	timeStr := eventTime.Format("15:04:05")

	// 1. Player joins
	if m := rePlayerJoin1.FindStringSubmatch(msg); len(m) > 1 {
		pName := cleanName(m[1])
		p.onlinePlayers[pName] = eventTime
		return &LogEvent{
			Timestamp:  eventTime,
			TimeString: timeStr,
			Category:   "player_join",
			Message:    fmt.Sprintf("Player '%s' joined the server", pName),
			Raw:        line,
		}
	}
	if m := rePlayerJoin2.FindStringSubmatch(msg); len(m) > 1 {
		pName := cleanName(m[1])
		p.onlinePlayers[pName] = eventTime
		return &LogEvent{
			Timestamp:  eventTime,
			TimeString: timeStr,
			Category:   "player_join",
			Message:    fmt.Sprintf("Player '%s' entered the game", pName),
			Raw:        line,
		}
	}

	// 2. Player leaves
	if m := rePlayerLeave1.FindStringSubmatch(msg); len(m) > 1 {
		pName := cleanName(m[1])
		delete(p.onlinePlayers, pName)
		return &LogEvent{
			Timestamp:  eventTime,
			TimeString: timeStr,
			Category:   "player_leave",
			Message:    fmt.Sprintf("Player '%s' disconnected", pName),
			Raw:        line,
		}
	}
	if m := rePlayerLeave2.FindStringSubmatch(msg); len(m) > 1 {
		pName := cleanName(m[1])
		delete(p.onlinePlayers, pName)
		return &LogEvent{
			Timestamp:  eventTime,
			TimeString: timeStr,
			Category:   "player_leave",
			Message:    fmt.Sprintf("Player '%s' disconnected", pName),
			Raw:        line,
		}
	}
	if m := rePlayerLeave3.FindStringSubmatch(msg); len(m) > 1 {
		pName := cleanName(m[1])
		delete(p.onlinePlayers, pName)
		return &LogEvent{
			Timestamp:  eventTime,
			TimeString: timeStr,
			Category:   "player_leave",
			Message:    fmt.Sprintf("Player '%s' disconnected", pName),
			Raw:        line,
		}
	}

	// 3. Game Saves
	if m := reWorldSave.FindStringSubmatch(msg); len(m) > 1 {
		sec, _ := strconv.ParseFloat(m[1], 64)
		p.lastSave = SaveInfo{
			LastSaveTime: eventTime,
			DurationSec:  sec,
		}
		return &LogEvent{
			Timestamp:  eventTime,
			TimeString: timeStr,
			Category:   "save",
			Message:    fmt.Sprintf("World save completed in %.2fs", sec),
			Raw:        line,
		}
	}
	if m := reAutoSave.FindStringSubmatch(msg); len(m) > 1 {
		sec, _ := strconv.ParseFloat(m[1], 64)
		p.lastSave = SaveInfo{
			LastSaveTime: eventTime,
			DurationSec:  sec,
		}
		return &LogEvent{
			Timestamp:  eventTime,
			TimeString: timeStr,
			Category:   "save",
			Message:    fmt.Sprintf("Autosave finished in %.2fs", sec),
			Raw:        line,
		}
	}
	if m := reSaveTo.FindStringSubmatch(msg); len(m) > 1 {
		saveFile := strings.TrimSpace(m[1])
		p.lastSave.SaveName = saveFile
		return &LogEvent{
			Timestamp:  eventTime,
			TimeString: timeStr,
			Category:   "save",
			Message:    fmt.Sprintf("Saving game to '%s'", saveFile),
			Raw:        line,
		}
	}

	// 4. Server Lifecycle
	if m := reServerAPI.FindStringSubmatch(msg); len(m) > 1 {
		return &LogEvent{
			Timestamp:  eventTime,
			TimeString: timeStr,
			Category:   "server",
			Message:    fmt.Sprintf("Satisfactory Server API ready and listening on %s", m[1]),
			Raw:        line,
		}
	}
	if m := reHttpListener.FindStringSubmatch(msg); len(m) > 1 {
		return &LogEvent{
			Timestamp:  eventTime,
			TimeString: timeStr,
			Category:   "server",
			Message:    fmt.Sprintf("HTTP listener created on %s", m[1]),
			Raw:        line,
		}
	}
	if reAppInstalled.MatchString(msg) {
		return &LogEvent{
			Timestamp:  eventTime,
			TimeString: timeStr,
			Category:   "server",
			Message:    "Game files updated / verified via SteamCMD",
			Raw:        line,
		}
	}
	if reEngineStart.MatchString(msg) {
		return &LogEvent{
			Timestamp:  eventTime,
			TimeString: timeStr,
			Category:   "server",
			Message:    "Unreal Engine started for FactoryGame",
			Raw:        line,
		}
	}

	// 5. Warnings / Crashes
	if m := reCrashGUID.FindStringSubmatch(msg); len(m) > 1 {
		return &LogEvent{
			Timestamp:  eventTime,
			TimeString: timeStr,
			Category:   "warning",
			Message:    fmt.Sprintf("Server session crash recorded (GUID: %s)", m[1]),
			Raw:        line,
		}
	}

	return nil
}

func (p *LogParser) addEventLocked(evt LogEvent) {
	evt.ID = p.nextID
	p.nextID++

	if len(p.events) >= p.maxEvents {
		p.events = p.events[1:]
	}
	p.events = append(p.events, evt)
}

// GetEvents returns logged events since a given ID
func (p *LogParser) GetEvents(sinceID int) []LogEvent {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if sinceID <= 0 {
		out := make([]LogEvent, len(p.events))
		copy(out, p.events)
		return out
	}

	var out []LogEvent
	for _, e := range p.events {
		if e.ID > sinceID {
			out = append(out, e)
		}
	}
	return out
}

// GetOnlinePlayers returns the list of online players derived from logs
func (p *LogParser) GetOnlinePlayers() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()

	var list []string
	for name := range p.onlinePlayers {
		list = append(list, name)
	}
	return list
}

// GetLastSave returns the latest save event details
func (p *LogParser) GetLastSave() SaveInfo {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.lastSave
}

func cleanName(raw string) string {
	s := strings.TrimSpace(raw)
	s = strings.Trim(s, "'\"")
	if idx := strings.Index(s, "|"); idx != -1 {
		s = strings.TrimSpace(s[:idx])
	}
	return s
}


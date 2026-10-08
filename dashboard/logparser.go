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
	Category   string    `json:"category"` // "player_join", "player_leave", "save", "server", "warning", "error"
	Message    string    `json:"message"`
	Count      int       `json:"count"`
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
	// Regex for Unreal Engine timestamp anywhere in the line: [2024.09.12-11.27.38:063][  0]
	// Allows container prefixes like [satisfactory-server] 08.10.2026 12:00:10 [2026.10.07-23.00.04:102][827]
	reUETimestamp = regexp.MustCompile(`\[(\d{4})\.(\d{2})\.(\d{2})-(\d{2})\.(\d{2})\.(\d{2}):(\d{3})\](?:\[\s*\d+\])?\s*(.*)$`)

	// Container timestamp fallback: [satisfactory-server] 08.10.2026 12:00:10
	reContainerTimestamp = regexp.MustCompile(`(\d{2})\.(\d{2})\.(\d{4})\s+(\d{2}):(\d{2}):(\d{2})`)

	// Player joins
	rePlayerJoin1 = regexp.MustCompile(`Join succeeded:\s*([^\r\n]+)`)
	rePlayerJoin2 = regexp.MustCompile(`Player\s+([^\r\n]+?)\s+entered the game`)

	// Player leaves / disconnects
	rePlayerLeave1    = regexp.MustCompile(`Client\s+([^\r\n ]+)\s+disconnected`)
	rePlayerLeave2    = regexp.MustCompile(`Closing connection for player\s*([^\r\n]+)`)
	rePlayerLeave3    = regexp.MustCompile(`PlayerName:\s*([^,\]\r\n]+).*Close`)
	reNetRemoveClient = regexp.MustCompile(`UNetDriver::RemoveClientConnection - Removed address\s*([^\s\r\n]+)`)
	reChannelCleanUp  = regexp.MustCompile(`UChannel::CleanUp: .* RemoteAddr:\s*([^\s,\]\r\n]+)`)

	// Game saves & serialization
	reWorldSave         = regexp.MustCompile(`(?i)(?:world save took|autosave took|save took)[:\s]+([0-9.]+)\s*seconds?`)
	reSaveSerialization = regexp.MustCompile(`(?i)World Serialization \(save\)[:\s]+([0-9.]+)\s*seconds`)
	reLoadSerialization = regexp.MustCompile(`(?i)World Serialization \(load\)[:\s]+([0-9.]+)\s*seconds`)
	reSaveTo            = regexp.MustCompile(`(?i)(?:saving (?:world |game )?to|saved (?:world |game )?to|saving savegame)[:\s]+([^\r\n]+)`)

	// Server lifecycle & match state
	reServerAPI           = regexp.MustCompile(`Server API listening on\s*'([^']+)'`)
	reHttpListener        = regexp.MustCompile(`Created new HttpListener on\s*([^\r\n]+)`)
	reAppInstalled        = regexp.MustCompile(`Success!\s*App '1690800' fully installed`)
	reEngineStart         = regexp.MustCompile(`Running engine for game:\s*FactoryGame`)
	reCrashGUID           = regexp.MustCompile(`Session CrashGUID >\s*([A-Za-z0-9-]+)`)
	reMatchState          = regexp.MustCompile(`Match State Changed from (\w+) to (\w+)`)
	reSessionRestart      = regexp.MustCompile(`Session Restart Timer elapsed, rebooting the session`)
	reReliableMessaging   = regexp.MustCompile(`(?:Server streaming socket bound to port|Reliable socket listen port has been explicitly remapped to)\s*(\d+)`)
	reAutoPause           = regexp.MustCompile(`auto-pause is allowed to proceed from now on`)
	reAcceptConnection    = regexp.MustCompile(`NotifyAcceptingConnection accepted (?:from|aggregation):\s*([^\s\r\n]+)`)
	reProcessServerTravel = regexp.MustCompile(`ProcessServerTravel:`)
	reLoadGameParam       = regexp.MustCompile(`[?&]loadgame=([^?&\s]+)`)
	reSessionParam        = regexp.MustCompile(`[?&]sessionName=([^?&\s]+)`)

	// Warnings & errors
	reRootMismatch       = regexp.MustCompile(`New/Old Root size mismatch!`)
	reStaticMeshOverride = regexp.MustCompile(`Loading a staticmesh component that is flag with override vertex color buffer`)
	reAudioMissing       = regexp.MustCompile(`UAkGameplayStatics::StopActor: Could not retrieve audio device`)
	reNavQueueFull       = regexp.MustCompile(`Navigation System: registration queue full!`)
	reNavBounds          = regexp.MustCompile(`Navmesh bounds are too large! Limiting requested tiles count.*?for (\w+)`)
	reNavBoundsGeneric   = regexp.MustCompile(`Navmesh bounds are too large! Limiting requested tiles count`)
	reNavAgentInvalid    = regexp.MustCompile(`NavData RegistrationFailed_AgentNotValid`)
	reNavWaterVolume     = regexp.MustCompile(`AddNode: Empty bounds, ignoring FGWaterVolume`)
	reCreatureNavMissing = regexp.MustCompile(`Nav Data for agent (\w+) was not found`)
	reOptionMissing      = regexp.MustCompile(`Could not find user setting with id '([^']+)'`)
	reSaveCellError      = regexp.MustCompile(`LogSave: Error: Object Reference '([^']+)' is contained inside of the World Partition Cell`)
	reInventoryItemNull  = regexp.MustCompile(`Add failed cause InventoryItemClass was null`)
	reFactoryPutDown     = regexp.MustCompile(`Put down failed because we where never equipped`)
	reRemoteDestroyed    = regexp.MustCompile(`Remote function (\w+) called from actor .* while actor is being destroyed`)

	// Generic fallbacks
	reGenericError   = regexp.MustCompile(`^Log\w+:\s*Error:\s*(.*)`)
	reGenericWarning = regexp.MustCompile(`^Log\w+:\s*Warning:\s*(.*)`)

	// IP:Port extraction helper
	reIPPort = regexp.MustCompile(`([0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3})(?::(\d+))?`)
)

// NewLogParser creates a log parser with candidate log paths
func NewLogParser(customPaths []string, maxEvents int) *LogParser {
	if maxEvents <= 0 {
		maxEvents = 300
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

	// Attempt parsing Unreal Engine timestamp (supports container prefixes)
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
	} else if match := reContainerTimestamp.FindStringSubmatch(trimmed); len(match) == 7 {
		day, _ := strconv.Atoi(match[1])
		month, _ := strconv.Atoi(match[2])
		year, _ := strconv.Atoi(match[3])
		hour, _ := strconv.Atoi(match[4])
		min, _ := strconv.Atoi(match[5])
		sec, _ := strconv.Atoi(match[6])

		parsed := time.Date(year, time.Month(month), day, hour, min, sec, 0, time.Local)
		if !parsed.IsZero() {
			eventTime = parsed
		}
		// Strip container prefix if present
		if idx := strings.Index(msg, "] "); idx != -1 {
			msg = strings.TrimSpace(msg[idx+2:])
		}
	}

	// Skip load summary / decorative separator banners
	if strings.Contains(msg, "=========") {
		return nil
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

	// 2. Player leaves & disconnects
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
	if m := reNetRemoveClient.FindStringSubmatch(msg); len(m) > 1 {
		addr := cleanAddr(m[1])
		return &LogEvent{
			Timestamp:  eventTime,
			TimeString: timeStr,
			Category:   "player_leave",
			Message:    fmt.Sprintf("Client connection closed: %s", addr),
			Raw:        line,
		}
	}
	if m := reChannelCleanUp.FindStringSubmatch(msg); len(m) > 1 {
		addr := cleanAddr(m[1])
		return &LogEvent{
			Timestamp:  eventTime,
			TimeString: timeStr,
			Category:   "player_leave",
			Message:    fmt.Sprintf("Client channel closed: %s", addr),
			Raw:        line,
		}
	}

	// 3. Game Saves & Serialization
	if m := reWorldSave.FindStringSubmatch(msg); len(m) > 1 {
		sec, _ := strconv.ParseFloat(m[1], 64)
		p.lastSave.LastSaveTime = eventTime
		p.lastSave.DurationSec = sec
		return &LogEvent{
			Timestamp:  eventTime,
			TimeString: timeStr,
			Category:   "save",
			Message:    fmt.Sprintf("World save completed in %.2fs", sec),
			Raw:        line,
		}
	}
	if m := reSaveSerialization.FindStringSubmatch(msg); len(m) > 1 {
		sec, _ := strconv.ParseFloat(m[1], 64)
		p.lastSave.LastSaveTime = eventTime
		p.lastSave.DurationSec = sec
		return &LogEvent{
			Timestamp:  eventTime,
			TimeString: timeStr,
			Category:   "save",
			Message:    fmt.Sprintf("World save serialized in %.2fs", sec),
			Raw:        line,
		}
	}
	if m := reLoadSerialization.FindStringSubmatch(msg); len(m) > 1 {
		sec, _ := strconv.ParseFloat(m[1], 64)
		return &LogEvent{
			Timestamp:  eventTime,
			TimeString: timeStr,
			Category:   "save",
			Message:    fmt.Sprintf("World save deserialized and loaded in %.2fs", sec),
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

	// 4. Server Lifecycle & Match State
	if reProcessServerTravel.MatchString(msg) {
		loadGame := ""
		if lm := reLoadGameParam.FindStringSubmatch(msg); len(lm) > 1 {
			loadGame = lm[1]
			p.lastSave.SaveName = loadGame
		}
		sessionName := ""
		if sm := reSessionParam.FindStringSubmatch(msg); len(sm) > 1 {
			sessionName = sm[1]
		}
		msgText := "Server travel initiated"
		if sessionName != "" && loadGame != "" {
			msgText = fmt.Sprintf("Server travel: Session '%s' (save: %s)", sessionName, loadGame)
		} else if sessionName != "" {
			msgText = fmt.Sprintf("Server travel: Session '%s'", sessionName)
		} else if loadGame != "" {
			msgText = fmt.Sprintf("Server travel loading save '%s'", loadGame)
		}
		return &LogEvent{
			Timestamp:  eventTime,
			TimeString: timeStr,
			Category:   "server",
			Message:    msgText,
			Raw:        line,
		}
	}
	if m := reMatchState.FindStringSubmatch(msg); len(m) > 2 {
		return &LogEvent{
			Timestamp:  eventTime,
			TimeString: timeStr,
			Category:   "server",
			Message:    fmt.Sprintf("Match state changed: %s -> %s", m[1], m[2]),
			Raw:        line,
		}
	}
	if reSessionRestart.MatchString(msg) {
		return &LogEvent{
			Timestamp:  eventTime,
			TimeString: timeStr,
			Category:   "server",
			Message:    "Session restart timer elapsed; rebooting server session",
			Raw:        line,
		}
	}
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
	if m := reReliableMessaging.FindStringSubmatch(msg); len(m) > 1 {
		return &LogEvent{
			Timestamp:  eventTime,
			TimeString: timeStr,
			Category:   "server",
			Message:    fmt.Sprintf("Reliable messaging socket active on port %s", m[1]),
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
	if reAutoPause.MatchString(msg) {
		return &LogEvent{
			Timestamp:  eventTime,
			TimeString: timeStr,
			Category:   "server",
			Message:    "Server startup complete; auto-pause allowed to proceed",
			Raw:        line,
		}
	}
	if m := reAcceptConnection.FindStringSubmatch(msg); len(m) > 1 {
		addr := cleanAddr(m[1])
		return &LogEvent{
			Timestamp:  eventTime,
			TimeString: timeStr,
			Category:   "server",
			Message:    fmt.Sprintf("Incoming connection accepted from %s", addr),
			Raw:        line,
		}
	}

	// 5. Warnings & Errors
	if m := reCrashGUID.FindStringSubmatch(msg); len(m) > 1 {
		return &LogEvent{
			Timestamp:  eventTime,
			TimeString: timeStr,
			Category:   "warning",
			Message:    fmt.Sprintf("Server session crash recorded (GUID: %s)", m[1]),
			Raw:        line,
		}
	}
	if reRootMismatch.MatchString(msg) {
		return &LogEvent{
			Timestamp:  eventTime,
			TimeString: timeStr,
			Category:   "warning",
			Message:    "Save root size mismatch detected",
			Raw:        line,
		}
	}
	if reStaticMeshOverride.MatchString(msg) {
		return &LogEvent{
			Timestamp:  eventTime,
			TimeString: timeStr,
			Category:   "warning",
			Message:    "StaticMesh component override vertex color buffer is empty",
			Raw:        line,
		}
	}
	if reAudioMissing.MatchString(msg) {
		return &LogEvent{
			Timestamp:  eventTime,
			TimeString: timeStr,
			Category:   "warning",
			Message:    "Audio warning: Could not retrieve audio device",
			Raw:        line,
		}
	}
	if reNavQueueFull.MatchString(msg) {
		return &LogEvent{
			Timestamp:  eventTime,
			TimeString: timeStr,
			Category:   "warning",
			Message:    "Navigation System registration queue is full",
			Raw:        line,
		}
	}
	if m := reNavBounds.FindStringSubmatch(msg); len(m) > 1 {
		return &LogEvent{
			Timestamp:  eventTime,
			TimeString: timeStr,
			Category:   "error",
			Message:    fmt.Sprintf("Navmesh bounds too large; limited requested tiles for %s", m[1]),
			Raw:        line,
		}
	}
	if reNavBoundsGeneric.MatchString(msg) {
		return &LogEvent{
			Timestamp:  eventTime,
			TimeString: timeStr,
			Category:   "error",
			Message:    "Navmesh bounds too large; limiting requested tiles count",
			Raw:        line,
		}
	}
	if reNavAgentInvalid.MatchString(msg) {
		return &LogEvent{
			Timestamp:  eventTime,
			TimeString: timeStr,
			Category:   "warning",
			Message:    "NavData registration failed: agent not valid",
			Raw:        line,
		}
	}
	if reNavWaterVolume.MatchString(msg) {
		return &LogEvent{
			Timestamp:  eventTime,
			TimeString: timeStr,
			Category:   "warning",
			Message:    "Navigation: Empty bounds, ignoring water volume",
			Raw:        line,
		}
	}
	if m := reCreatureNavMissing.FindStringSubmatch(msg); len(m) > 1 {
		return &LogEvent{
			Timestamp:  eventTime,
			TimeString: timeStr,
			Category:   "error",
			Message:    fmt.Sprintf("Creature Nav Data for agent '%s' not found", m[1]),
			Raw:        line,
		}
	}
	if m := reOptionMissing.FindStringSubmatch(msg); len(m) > 1 {
		return &LogEvent{
			Timestamp:  eventTime,
			TimeString: timeStr,
			Category:   "warning",
			Message:    fmt.Sprintf("Game option setting '%s' not found", m[1]),
			Raw:        line,
		}
	}
	if m := reSaveCellError.FindStringSubmatch(msg); len(m) > 1 {
		return &LogEvent{
			Timestamp:  eventTime,
			TimeString: timeStr,
			Category:   "error",
			Message:    fmt.Sprintf("World Partition unloaded cell reference error (%s)", cleanCellRef(m[1])),
			Raw:        line,
		}
	}
	if reInventoryItemNull.MatchString(msg) {
		return &LogEvent{
			Timestamp:  eventTime,
			TimeString: timeStr,
			Category:   "warning",
			Message:    "Inventory item add failed: ItemClass was null",
			Raw:        line,
		}
	}
	if reFactoryPutDown.MatchString(msg) {
		return &LogEvent{
			Timestamp:  eventTime,
			TimeString: timeStr,
			Category:   "warning",
			Message:    "Factory item put down failed: actor was never equipped",
			Raw:        line,
		}
	}
	if m := reRemoteDestroyed.FindStringSubmatch(msg); len(m) > 1 {
		return &LogEvent{
			Timestamp:  eventTime,
			TimeString: timeStr,
			Category:   "warning",
			Message:    fmt.Sprintf("Remote function '%s' called while actor being destroyed", m[1]),
			Raw:        line,
		}
	}

	// 6. Generic Warnings / Errors fallback
	if m := reGenericError.FindStringSubmatch(msg); len(m) > 1 {
		text := strings.TrimSpace(m[1])
		if len(text) > 110 {
			text = text[:107] + "..."
		}
		return &LogEvent{
			Timestamp:  eventTime,
			TimeString: timeStr,
			Category:   "error",
			Message:    text,
			Raw:        line,
		}
	}
	if m := reGenericWarning.FindStringSubmatch(msg); len(m) > 1 {
		text := strings.TrimSpace(m[1])
		if len(text) > 110 {
			text = text[:107] + "..."
		}
		return &LogEvent{
			Timestamp:  eventTime,
			TimeString: timeStr,
			Category:   "warning",
			Message:    text,
			Raw:        line,
		}
	}

	return nil
}

func (p *LogParser) addEventLocked(evt LogEvent) {
	if evt.Count <= 0 {
		evt.Count = 1
	}

	// Collapse repeating events within recent lookback window (last 30 events)
	lookback := 30
	if len(p.events) < lookback {
		lookback = len(p.events)
	}

	for i := len(p.events) - 1; i >= len(p.events)-lookback; i-- {
		existing := &p.events[i]
		if existing.Category == evt.Category && existing.Message == evt.Message {
			// If within 15 minutes or immediate burst, collapse into counter
			if evt.Timestamp.Sub(existing.Timestamp) <= 15*time.Minute || existing.Timestamp.IsZero() {
				if i == len(p.events)-1 {
					existing.Count++
					existing.Timestamp = evt.Timestamp
					existing.TimeString = evt.TimeString
					existing.Raw = evt.Raw
					return
				}
				// If not the very last event, update and bump to end of slice
				updated := *existing
				updated.Count++
				updated.Timestamp = evt.Timestamp
				updated.TimeString = evt.TimeString
				updated.Raw = evt.Raw
				p.events = append(p.events[:i], p.events[i+1:]...)
				p.events = append(p.events, updated)
				return
			}
		}
	}

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

func cleanAddr(raw string) string {
	if m := reIPPort.FindStringSubmatch(raw); len(m) > 1 {
		if len(m) > 2 && m[2] != "" {
			return fmt.Sprintf("%s:%s", m[1], m[2])
		}
		return m[1]
	}
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "[::ffff:")
	s = strings.TrimPrefix(s, "::ffff:")
	s = strings.Trim(s, "[]")
	if idx := strings.Index(s, " "); idx != -1 {
		s = s[:idx]
	}
	return s
}

func cleanCellRef(raw string) string {
	s := strings.TrimSpace(raw)
	if idx := strings.LastIndex(s, "."); idx != -1 {
		return s[idx+1:]
	}
	return s
}

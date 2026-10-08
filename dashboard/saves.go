package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf16"
)

// SaveFileInfo represents metadata about a saved game file
type SaveFileInfo struct {
	Name            string    `json:"name"`
	SessionName     string    `json:"sessionName,omitempty"`
	PlayDurationSec int       `json:"playDurationSec,omitempty"`
	SizeMB          float64   `json:"sizeMB"`
	SizeHuman       string    `json:"sizeHuman"`
	ModTime         time.Time `json:"modTime"`
	ModTimeString   string    `json:"modTimeString"`
	TimeAgo         string    `json:"timeAgo"`
	IsBackup        bool      `json:"isBackup"`
	Path            string    `json:"path"`
}

// SavesScanner inspects disk saves and backups
type SavesScanner struct {
	searchRoots []string
}

// NewSavesScanner creates a new scanner with root directories to search for saves
func NewSavesScanner() *SavesScanner {
	return &SavesScanner{
		searchRoots: []string{
			"/config/saved",
			"/config/backups",
			"/config",
			"./satisfactory-server/saved",
			"./satisfactory-server/backups",
			"./satisfactory-server",
		},
	}
}

// ListSaves returns sorted list of all save files (newest first)
func (s *SavesScanner) ListSaves(limit int) []SaveFileInfo {
	if limit <= 0 {
		limit = 10
	}

	resultsMap := make(map[string]SaveFileInfo)

	for _, root := range s.searchRoots {
		if _, err := os.Stat(root); err != nil {
			continue
		}

		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d == nil || d.IsDir() {
				return nil
			}

			nameLower := strings.ToLower(d.Name())
			if !strings.HasSuffix(nameLower, ".sav") && !strings.HasSuffix(nameLower, ".zip") {
				return nil
			}

			info, err := d.Info()
			if err != nil {
				return nil
			}

			isBackup := strings.Contains(strings.ToLower(path), "backup")
			sizeMB := round2(float64(info.Size()) / (1024 * 1024))

			var sessionName string
			var playDurSec int
			if strings.HasSuffix(nameLower, ".sav") && !isBackup {
				sessionName, playDurSec, _ = readSaveHeader(path)
			}

			resultsMap[path] = SaveFileInfo{
				Name:            d.Name(),
				SessionName:     sessionName,
				PlayDurationSec: playDurSec,
				SizeMB:          sizeMB,
				SizeHuman:       formatBytes(info.Size()),
				ModTime:         info.ModTime(),
				ModTimeString:   info.ModTime().Format("2006-01-02 15:04:05"),
				TimeAgo:         timeAgo(info.ModTime()),
				IsBackup:        isBackup,
				Path:            path,
			}
			return nil
		})

		// If saves were found in primary /config/saved or /config, stop to avoid duplicate scanning
		if len(resultsMap) > 0 && strings.HasPrefix(root, "/config") {
			break
		}
	}

	var results []SaveFileInfo
	for _, item := range resultsMap {
		results = append(results, item)
	}

	// Sort newest first
	sort.Slice(results, func(i, j int) bool {
		return results[i].ModTime.After(results[j].ModTime)
	})

	if len(results) > limit {
		results = results[:limit]
	}

	return results
}

// GetLatestSave returns the most recently modified save file on disk
func (s *SavesScanner) GetLatestSave() *SaveFileInfo {
	all := s.ListSaves(10)
	for _, item := range all {
		if !item.IsBackup && strings.HasSuffix(strings.ToLower(item.Name), ".sav") {
			return &item
		}
	}
	if len(all) > 0 {
		return &all[0]
	}
	return nil
}

func formatBytes(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}

func timeAgo(t time.Time) string {
	diff := time.Since(t)
	if diff < time.Minute {
		return "just now"
	}
	if diff < time.Hour {
		m := int(diff.Minutes())
		return fmt.Sprintf("%dm ago", m)
	}
	if diff < 24*time.Hour {
		h := int(diff.Hours())
		return fmt.Sprintf("%dh ago", h)
	}
	d := int(diff.Hours() / 24)
	return fmt.Sprintf("%dd ago", d)
}

// readSaveHeader attempts to extract sessionName and playDurationSec from a Satisfactory save file
func readSaveHeader(path string) (string, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()

	var headerVersion, saveVersion, buildVersion int32
	if err := binary.Read(f, binary.LittleEndian, &headerVersion); err != nil {
		return "", 0, err
	}
	if headerVersion < 5 || headerVersion > 25 {
		return "", 0, fmt.Errorf("unsupported header version: %d", headerVersion)
	}

	if err := binary.Read(f, binary.LittleEndian, &saveVersion); err != nil {
		return "", 0, err
	}
	if err := binary.Read(f, binary.LittleEndian, &buildVersion); err != nil {
		return "", 0, err
	}

	// MapName
	if _, err := readFString(f); err != nil {
		return "", 0, err
	}
	// MapOptions
	if _, err := readFString(f); err != nil {
		return "", 0, err
	}
	// SessionName
	sessionName, err := readFString(f)
	if err != nil {
		return "", 0, err
	}

	// PlayDurationSeconds
	var playDurationSec int32
	if err := binary.Read(f, binary.LittleEndian, &playDurationSec); err != nil {
		return sessionName, 0, err
	}

	return sessionName, int(playDurationSec), nil
}

func readFString(r io.Reader) (string, error) {
	var length int32
	if err := binary.Read(r, binary.LittleEndian, &length); err != nil {
		return "", err
	}
	if length == 0 {
		return "", nil
	}
	if length > 0 {
		if length > 1024 {
			return "", fmt.Errorf("string length exceeds sanity limit")
		}
		buf := make([]byte, length)
		if _, err := io.ReadFull(r, buf); err != nil {
			return "", err
		}
		if len(buf) > 0 && buf[len(buf)-1] == 0 {
			buf = buf[:len(buf)-1]
		}
		return string(buf), nil
	}
	// UTF-16
	utf16Len := -length
	if utf16Len > 1024 {
		return "", fmt.Errorf("utf16 string length exceeds sanity limit")
	}
	buf := make([]byte, utf16Len*2)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", err
	}
	u16s := make([]uint16, utf16Len)
	for i := 0; i < int(utf16Len); i++ {
		u16s[i] = binary.LittleEndian.Uint16(buf[i*2:])
	}
	if len(u16s) > 0 && u16s[len(u16s)-1] == 0 {
		u16s = u16s[:len(u16s)-1]
	}
	return string(utf16.Decode(u16s)), nil
}

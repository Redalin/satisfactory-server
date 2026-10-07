package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// SaveFileInfo represents metadata about a saved game file
type SaveFileInfo struct {
	Name         string    `json:"name"`
	SizeMB       float64   `json:"sizeMB"`
	SizeHuman    string    `json:"sizeHuman"`
	ModTime      time.Time `json:"modTime"`
	ModTimeString string   `json:"modTimeString"`
	TimeAgo      string    `json:"timeAgo"`
	IsBackup     bool      `json:"isBackup"`
}

// SavesScanner inspects disk saves and backups
type SavesScanner struct {
	saveDirs   []string
	backupDirs []string
}

// NewSavesScanner creates a new scanner with default Satisfactory save paths
func NewSavesScanner() *SavesScanner {
	return &SavesScanner{
		saveDirs: []string{
			"/config/saved/server",
			"/config/saved",
			"./satisfactory-server/saved/server",
			"./satisfactory-server/saved",
		},
		backupDirs: []string{
			"/config/backups",
			"./satisfactory-server/backups",
		},
	}
}

// ListSaves returns sorted list of save files (newest first)
func (s *SavesScanner) ListSaves(limit int) []SaveFileInfo {
	if limit <= 0 {
		limit = 10
	}

	var results []SaveFileInfo

	// Scan primary save dirs
	for _, dir := range s.saveDirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}

		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".sav") {
				continue
			}

			info, err := entry.Info()
			if err != nil {
				continue
			}

			sizeMB := round2(float64(info.Size()) / (1024 * 1024))
			results = append(results, SaveFileInfo{
				Name:          entry.Name(),
				SizeMB:        sizeMB,
				SizeHuman:     formatBytes(info.Size()),
				ModTime:       info.ModTime(),
				ModTimeString: info.ModTime().Format("2006-01-02 15:04:05"),
				TimeAgo:       timeAgo(info.ModTime()),
				IsBackup:      false,
			})
		}

		if len(results) > 0 {
			break // found active directory
		}
	}

	// Scan backups
	for _, dir := range s.backupDirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}

		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				continue
			}

			sizeMB := round2(float64(info.Size()) / (1024 * 1024))
			results = append(results, SaveFileInfo{
				Name:          filepath.Base(entry.Name()),
				SizeMB:        sizeMB,
				SizeHuman:     formatBytes(info.Size()),
				ModTime:       info.ModTime(),
				ModTimeString: info.ModTime().Format("2006-01-02 15:04:05"),
				TimeAgo:       timeAgo(info.ModTime()),
				IsBackup:      true,
			})
		}
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


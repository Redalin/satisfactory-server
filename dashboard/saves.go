package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// SaveFileInfo represents metadata about a saved game file
type SaveFileInfo struct {
	Name          string    `json:"name"`
	SizeMB        float64   `json:"sizeMB"`
	SizeHuman     string    `json:"sizeHuman"`
	ModTime       time.Time `json:"modTime"`
	ModTimeString string    `json:"modTimeString"`
	TimeAgo       string    `json:"timeAgo"`
	IsBackup      bool      `json:"isBackup"`
	Path          string    `json:"path"`
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

			resultsMap[path] = SaveFileInfo{
				Name:          d.Name(),
				SizeMB:        sizeMB,
				SizeHuman:     formatBytes(info.Size()),
				ModTime:       info.ModTime(),
				ModTimeString: info.ModTime().Format("2006-01-02 15:04:05"),
				TimeAgo:       timeAgo(info.ModTime()),
				IsBackup:      isBackup,
				Path:          path,
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

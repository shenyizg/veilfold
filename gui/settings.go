package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"github.com/shenyizg/veilfold/internal/core"
	"github.com/shenyizg/veilfold/internal/fsx"
)

const projectURL = "https://github.com/shenyizg/veilfold"

// Settings are the desktop app's preferences. They contain nothing
// secret and are stored in plain JSON next to the vaults.
type Settings struct {
	// DefaultMode preselects the protection mode for new folders.
	DefaultMode string `json:"defaultMode"`
	// AutoLockMinutes locks every folder and closes the vault after this
	// many minutes without user activity; 0 disables it.
	AutoLockMinutes int `json:"autoLockMinutes"`
}

func defaultSettings() Settings {
	return Settings{DefaultMode: string(core.ModeEncrypt), AutoLockMinutes: 0}
}

type settingsStore struct {
	path string
	mu   sync.Mutex
}

func newSettingsStore(dataDir string) *settingsStore {
	return &settingsStore{path: filepath.Join(dataDir, "gui-settings.json")}
}

func (s *settingsStore) get() Settings {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := defaultSettings()
	if data, err := os.ReadFile(s.path); err == nil {
		_ = json.Unmarshal(data, &st)
	}
	return sanitize(st)
}

func (s *settingsStore) set(st Settings) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := json.MarshalIndent(sanitize(st), "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	return fsx.WriteFileAtomic(s.path, append(data, '\n'), 0o600)
}

func sanitize(st Settings) Settings {
	if _, err := core.ParseMode(st.DefaultMode); err != nil {
		st.DefaultMode = string(core.ModeEncrypt)
	}
	st.AutoLockMinutes = max(0, min(st.AutoLockMinutes, 24*60))
	return st
}

func joinPath(dir, name string) string { return filepath.Join(dir, name) }

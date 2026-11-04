package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
)

// appSettings are preferences changed from the web page that must hold while
// the page is closed and across restarts. They are stored next to
// profile-notes.json. An unset field means its default.
type appSettings struct {
	WakeRecovery *bool `json:"wake_recovery,omitempty"`
}

func (s appSettings) wakeRecoveryOn() bool {
	return s.WakeRecovery == nil || *s.WakeRecovery
}

func defaultSettingsPath() (string, error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate settings directory: %w", err)
	}
	return filepath.Join(configDir, "DJOneHub", "settings.json"), nil
}

// loadSettings reads path. A missing file gives the defaults.
func loadSettings(path string) (appSettings, error) {
	var settings appSettings
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return settings, nil
	}
	if err != nil {
		return settings, fmt.Errorf("read settings: %w", err)
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		return appSettings{}, fmt.Errorf("parse settings %s: %w", path, err)
	}
	return settings, nil
}

func saveSettings(path string, settings appSettings) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create settings directory: %w", err)
	}
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("encode settings: %w", err)
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return fmt.Errorf("write settings: %w", err)
	}
	if err := os.Rename(temporary, path); err != nil {
		return fmt.Errorf("replace settings: %w", err)
	}
	return nil
}

// initSettings loads the saved settings. If they cannot be read the defaults
// apply, and changes made on the page still take effect but may not be saved.
func (a *app) initSettings() {
	path, err := defaultSettingsPath()
	if err != nil {
		log.Printf("settings unavailable, using defaults: %v", err)
		return
	}
	settings, err := loadSettings(path)
	if err != nil {
		log.Printf("settings unreadable, using defaults: %v", err)
	}
	a.settingsMu.Lock()
	a.settingsPath = path
	a.settings = settings
	a.settingsMu.Unlock()
}

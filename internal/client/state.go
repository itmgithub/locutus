package client

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// SaveSettings keeps the settings of the running container for "apply",
// which runs through sudo without the environment of the container.
func (s *System) SaveSettings(set *Settings) error {
	data, err := json.MarshalIndent(set, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(s.path(SettingsPath), string(data)+"\n", 0o600)
}

// LoadSettings reads the settings that the container saved at its start.
func (s *System) LoadSettings() (*Settings, error) {
	data, err := os.ReadFile(s.path(SettingsPath))
	if err != nil {
		return nil, err
	}
	var set Settings
	if err := json.Unmarshal(data, &set); err != nil {
		return nil, err
	}
	return &set, nil
}

func (s *System) statePath(set *Settings) string {
	return s.path(filepath.Join(set.ConfigDir, StateFile))
}

// LoadState returns the configuration that Locutus pushed last, or an empty
// one when nothing has been pushed yet.
func (s *System) LoadState(set *Settings) (*State, error) {
	data, err := os.ReadFile(s.statePath(set))
	if errors.Is(err, fs.ErrNotExist) {
		return &State{}, nil
	} else if err != nil {
		return nil, err
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// SaveState keeps a pushed configuration (only readable by root).
func (s *System) SaveState(set *Settings, st *State) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.statePath(set)), 0o755); err != nil {
		return err
	}
	return writeFile(s.statePath(set), string(data)+"\n", 0o600)
}

// OnDisk returns the directories in the repositories directory.
func (s *System) OnDisk(set *Settings) ([]string, error) {
	entries, err := os.ReadDir(s.path(set.ReposDir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	sort.Strings(dirs)
	return dirs, nil
}

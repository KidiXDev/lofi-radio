package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

const defaultConfigFileName = "config.yaml"

type Manager struct {
	mu         sync.Mutex
	path       string
	serializer Serializer
}

func NewManager(path string, serializer Serializer) (*Manager, error) {
	if serializer == nil {
		return nil, errors.New("serializer is required")
	}
	if path == "" {
		return nil, errors.New("config path is required")
	}
	return &Manager{
		path:       path,
		serializer: serializer,
	}, nil
}

func NewDefaultManager() (*Manager, error) {
	workingDir, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("resolve working directory: %w", err)
	}
	configPath := filepath.Join(workingDir, defaultConfigFileName)
	return NewManager(configPath, YAMLSerializer{})
}

func (m *Manager) Load() (Settings, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.loadLocked()
}

func (m *Manager) Save(settings Settings) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.saveLocked(NormalizeSettings(settings))
}

func (m *Manager) Update(mutator func(*Settings)) (Settings, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	settings, err := m.loadLocked()
	if err != nil {
		return Settings{}, err
	}

	mutator(&settings)
	settings = NormalizeSettings(settings)

	if err := m.saveLocked(settings); err != nil {
		return Settings{}, err
	}
	return settings, nil
}

func (m *Manager) loadLocked() (Settings, error) {
	data, err := os.ReadFile(m.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return DefaultSettings(), nil
		}
		return Settings{}, fmt.Errorf("read config: %w", err)
	}

	var settings Settings
	if err := m.serializer.Unmarshal(data, &settings); err != nil {
		return Settings{}, fmt.Errorf("decode config: %w", err)
	}
	return NormalizeSettings(settings), nil
}

func (m *Manager) saveLocked(settings Settings) error {
	dir := filepath.Dir(m.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}

	data, err := m.serializer.Marshal(settings)
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	data = append(data, '\n')

	tmpPath := m.path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		return fmt.Errorf("write temp config: %w", err)
	}
	if err := os.Rename(tmpPath, m.path); err != nil {
		return fmt.Errorf("replace config: %w", err)
	}
	return nil
}

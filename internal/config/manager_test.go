package config

import (
	"path/filepath"
	"testing"
)

func TestManagerLoadMissingReturnsDefaults(t *testing.T) {
	t.Parallel()

	manager, err := NewManager(filepath.Join(t.TempDir(), "config.yaml"), YAMLSerializer{})
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}

	settings, err := manager.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	defaults := DefaultSettings()
	if settings.Audio.Volume != defaults.Audio.Volume {
		t.Fatalf("expected default volume %d, got %d", defaults.Audio.Volume, settings.Audio.Volume)
	}
}

func TestManagerSaveThenLoad(t *testing.T) {
	t.Parallel()

	manager, err := NewManager(filepath.Join(t.TempDir(), "config.yaml"), YAMLSerializer{})
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}

	settings := DefaultSettings()
	settings.Audio.Volume = 80
	if err := manager.Save(settings); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	loaded, err := manager.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if loaded.Audio.Volume != 80 {
		t.Fatalf("expected volume 80, got %d", loaded.Audio.Volume)
	}
}

func TestManagerUpdateClampsValues(t *testing.T) {
	t.Parallel()

	manager, err := NewManager(filepath.Join(t.TempDir(), "config.yaml"), YAMLSerializer{})
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}

	updated, err := manager.Update(func(settings *Settings) {
		settings.Audio.Volume = 999
	})
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if updated.Audio.Volume != 100 {
		t.Fatalf("expected clamped volume 100, got %d", updated.Audio.Volume)
	}
}

package radio

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type CategoryCacheEntry struct {
	Key        string     `json:"key"`
	CachedAt   time.Time  `json:"cached_at"`
	Categories []Category `json:"categories"`
}

type CategoryCacheLookup struct {
	Found      bool
	Fresh      bool
	CachedAt   time.Time
	Categories []Category
}

func ReadCategoryCache(key string, ttl time.Duration) (CategoryCacheLookup, error) {
	lookup := CategoryCacheLookup{}
	cachePath, err := categoryCachePath(key)
	if err != nil {
		return lookup, err
	}

	payload, err := os.ReadFile(cachePath)
	if err != nil {
		if os.IsNotExist(err) {
			return lookup, nil
		}
		return lookup, err
	}

	var entry CategoryCacheEntry
	if err := json.Unmarshal(payload, &entry); err != nil {
		return lookup, nil
	}

	if len(entry.Categories) == 0 || entry.CachedAt.IsZero() {
		return lookup, nil
	}

	lookup.Found = true
	lookup.CachedAt = entry.CachedAt
	lookup.Categories = entry.Categories
	lookup.Fresh = time.Since(entry.CachedAt) <= ttl
	return lookup, nil
}

func WriteCategoryCache(key string, categories []Category) error {
	if len(categories) == 0 {
		return nil
	}

	cachePath, err := categoryCachePath(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o755); err != nil {
		return err
	}

	entry := CategoryCacheEntry{
		Key:        key,
		CachedAt:   time.Now().UTC(),
		Categories: categories,
	}

	encoded, err := json.Marshal(entry)
	if err != nil {
		return err
	}

	tempPath := cachePath + ".tmp"
	if err := os.WriteFile(tempPath, encoded, 0o644); err != nil {
		return err
	}
	return os.Rename(tempPath, cachePath)
}

func categoryCachePath(key string) (string, error) {
	executablePath, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve executable path: %w", err)
	}

	executableDir := filepath.Dir(executablePath)
	hash := sha1.Sum([]byte(key))
	fileName := hex.EncodeToString(hash[:]) + ".json"
	return filepath.Join(executableDir, ".cache", "categories", fileName), nil
}

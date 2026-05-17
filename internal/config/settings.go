package config

const (
	configVersion = 1
	defaultVolume = 55
)

type Settings struct {
	Version int           `json:"version"`
	Audio   AudioSettings `json:"audio"`
}

type AudioSettings struct {
	Volume int `json:"volume"`
}

func DefaultSettings() Settings {
	return Settings{
		Version: configVersion,
		Audio: AudioSettings{
			Volume: defaultVolume,
		},
	}
}

func NormalizeSettings(settings Settings) Settings {
	defaults := DefaultSettings()
	settings.Version = defaults.Version
	settings.Audio.Volume = clamp(settings.Audio.Volume, 0, 100)
	return settings
}

func clamp(value, minValue, maxValue int) int {
	if value < minValue {
		return minValue
	}
	if value > maxValue {
		return maxValue
	}
	return value
}

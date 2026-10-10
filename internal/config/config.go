// Package config loads the recorder's YAML configuration.
package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
	"maunium.net/go/mautrix/id"
)

type Config struct {
	Homeserver string    `yaml:"homeserver"`
	UserID     id.UserID `yaml:"user_id"`
	// Password logs the bot in at every start, reusing the device stored in the database.
	Password   string `yaml:"password"`
	DeviceName string `yaml:"device_name"`

	// Database holds the Olm/Megolm state and the room state cache (SQLite).
	Database string `yaml:"database"`
	// PickleKey encrypts the Olm account in the database. Keep it secret and never change it.
	PickleKey string `yaml:"pickle_key"`

	RecordingsDir string `yaml:"recordings_dir"`
	CommandPrefix string `yaml:"command_prefix"`
	// MinPowerLevel is what a user needs to start or stop a recording (50 = moderator).
	MinPowerLevel int `yaml:"min_power_level"`
	// LivekitServiceURL is used when the homeserver advertises no MatrixRTC transport.
	LivekitServiceURL string `yaml:"livekit_service_url"`

	LogLevel string `yaml:"log_level"`
}

func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg := Config{
		DeviceName:    "MatrixRTC recorder",
		Database:      "recorder.db",
		RecordingsDir: "recordings",
		CommandPrefix: "!record",
		MinPowerLevel: 50,
		LogLevel:      "info",
	}
	if err = yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	switch {
	case cfg.Homeserver == "":
		return nil, fmt.Errorf("homeserver is required")
	case cfg.UserID == "":
		return nil, fmt.Errorf("user_id is required")
	case cfg.Password == "":
		return nil, fmt.Errorf("password is required")
	case len(cfg.PickleKey) < 16:
		return nil, fmt.Errorf("pickle_key is required (at least 16 characters)")
	}
	return &cfg, nil
}

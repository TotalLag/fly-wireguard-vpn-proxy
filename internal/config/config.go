package config

import (
	"os"
	"path/filepath"
)

type Config struct {
	Port           string
	BootstrapToken string
	PeerName       string
	ConfigDir      string
	WGInterface    string
	EndpointHost   string
	EndpointPort   string
}

func Load() Config {
	peer := Getenv("BOOTSTRAP_PEER_NAME", "peer1")
	configDir := "/config"

	return Config{
		Port:           Getenv("BOOTSTRAP_PORT", "8081"),
		BootstrapToken: os.Getenv("BOOTSTRAP_TOKEN"),
		PeerName:       peer,
		ConfigDir:      configDir,
		WGInterface:    Getenv("WG_INTERFACE", "wg0"),
		EndpointHost:   os.Getenv("FLY_APP_NAME"),
		EndpointPort:   Getenv("BOOTSTRAP_ENDPOINT_PORT", Getenv("SERVERPORT", "51820")),
	}
}

func (c Config) PeerConfigPath() string {
	return filepath.Join(c.ConfigDir, c.PeerName, c.PeerName+".conf")
}

func (c Config) BootstrapDonePath() string {
	return filepath.Join(c.ConfigDir, "bootstrap_done")
}

func Getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
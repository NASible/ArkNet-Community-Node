package config

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	defaultBindAddr         = "127.0.0.1:8080"
	defaultAnnounceInterval = 30 * time.Minute
	defaultPeerTTL          = 45 * time.Minute
	defaultShutdownTimeout  = 10 * time.Second
)

type Config struct {
	BindAddr         string
	AnnounceInterval time.Duration
	PeerTTL          time.Duration
	ShutdownTimeout  time.Duration
	VerifyKey        ed25519.PublicKey
	RelayVerifyKeys  []ed25519.PublicKey
}

func Load() (Config, error) {
	var result Config
	keyFile := strings.TrimSpace(os.Getenv("ARKNET_TRACKER_VERIFY_KEY_FILE"))
	if keyFile == "" {
		return result, fmt.Errorf("ARKNET_TRACKER_VERIFY_KEY_FILE is required")
	}
	encoded, err := os.ReadFile(keyFile)
	if err != nil {
		return result, fmt.Errorf("read verification key: %w", err)
	}
	text := strings.Trim(string(encoded), " 	\r\n")
	decoded, err := base64.StdEncoding.DecodeString(text)
	if err != nil || base64.StdEncoding.EncodeToString(decoded) != text || len(decoded) != ed25519.PublicKeySize {
		return result, fmt.Errorf("verification key must be canonical padded base64 for exactly 32 bytes")
	}
	result.VerifyKey = append(ed25519.PublicKey(nil), decoded...)
	if result.RelayVerifyKeys, err = loadRelayKeys(strings.TrimSpace(os.Getenv("ARKNET_TRACKER_RELAY_VERIFY_KEYS_FILE")), result.VerifyKey); err != nil {
		return Config{}, err
	}

	result.BindAddr = envOrDefault("ARKNET_TRACKER_BIND_ADDR", defaultBindAddr)
	if err := validateAddress(result.BindAddr); err != nil {
		return Config{}, fmt.Errorf("ARKNET_TRACKER_BIND_ADDR: %w", err)
	}
	if result.AnnounceInterval, err = durationEnv("ARKNET_TRACKER_ANNOUNCE_INTERVAL", defaultAnnounceInterval); err != nil {
		return Config{}, err
	}
	if result.PeerTTL, err = durationEnv("ARKNET_TRACKER_PEER_TTL", defaultPeerTTL); err != nil {
		return Config{}, err
	}
	if result.ShutdownTimeout, err = durationEnv("ARKNET_TRACKER_SHUTDOWN_TIMEOUT", defaultShutdownTimeout); err != nil {
		return Config{}, err
	}
	return result, nil
}

func envOrDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func durationEnv(name string, fallback time.Duration) (time.Duration, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration", name)
	}
	return parsed, nil
}

func validateAddress(address string) error {
	_, portText, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("invalid host:port")
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil {
		return fmt.Errorf("invalid port")
	}
	if port == 0 && portText != "0" {
		return fmt.Errorf("invalid port")
	}
	return nil
}

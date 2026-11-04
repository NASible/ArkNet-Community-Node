package config

import (
	"bytes"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	BindAddr           string
	ControlBindAddr    string
	RelayID            string
	GrantKey           ed25519.PublicKey
	EndpointSigningKey ed25519.PrivateKey
	RevocationKey      ed25519.PublicKey
	Certificate        tls.Certificate
	MaxConnections     int
	MaxGrantLifetime   time.Duration
	ShutdownTimeout    time.Duration
}

var relayIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,63}$`)

// Load reads canonical padded base64 Ed25519 keys: 32-byte public verify
// keys and a dedicated, internally consistent 64-byte endpoint private key.
func Load(getenv func(string) string) (Config, error) {
	get := func(name string) string { return strings.TrimSpace(getenv(name)) }
	var cfg Config
	var err error
	if cfg.GrantKey, err = publicKey(get("ARKNET_RELAY_GRANT_VERIFY_KEY_FILE"), "ARKNET_RELAY_GRANT_VERIFY_KEY_FILE"); err != nil {
		return Config{}, err
	}
	if cfg.RevocationKey, err = publicKey(get("ARKNET_RELAY_REVOCATION_VERIFY_KEY_FILE"), "ARKNET_RELAY_REVOCATION_VERIFY_KEY_FILE"); err != nil {
		return Config{}, err
	}
	if bytes.Equal(cfg.GrantKey, cfg.RevocationKey) {
		return Config{}, fmt.Errorf("the revocation key must not be the grant key")
	}
	if cfg.EndpointSigningKey, err = endpointSigningKey(get("ARKNET_RELAY_ENDPOINT_SIGNING_KEY_FILE")); err != nil {
		return Config{}, err
	}
	endpointPublic := cfg.EndpointSigningKey.Public().(ed25519.PublicKey)
	if bytes.Equal(endpointPublic, cfg.GrantKey) || bytes.Equal(endpointPublic, cfg.RevocationKey) {
		return Config{}, fmt.Errorf("endpoint signing key must be distinct from grant and revocation keys")
	}
	cfg.RelayID = get("ARKNET_RELAY_ID")
	if !relayIDPattern.MatchString(cfg.RelayID) {
		return Config{}, fmt.Errorf("ARKNET_RELAY_ID must be 1-64 characters of a-z, 0-9, '.' and '-'")
	}
	certFile, keyFile := get("ARKNET_RELAY_TLS_CERT_FILE"), get("ARKNET_RELAY_TLS_KEY_FILE")
	if certFile == "" || keyFile == "" {
		return Config{}, fmt.Errorf("ARKNET_RELAY_TLS_CERT_FILE and ARKNET_RELAY_TLS_KEY_FILE are required")
	}
	if cfg.Certificate, err = tls.LoadX509KeyPair(certFile, keyFile); err != nil {
		return Config{}, fmt.Errorf("load TLS certificate: %w", err)
	}
	cfg.BindAddr = orDefault(get("ARKNET_RELAY_BIND_ADDR"), "127.0.0.1:7443")
	cfg.ControlBindAddr = orDefault(get("ARKNET_RELAY_CONTROL_BIND_ADDR"), "127.0.0.1:7080")
	for name, address := range map[string]string{"ARKNET_RELAY_BIND_ADDR": cfg.BindAddr, "ARKNET_RELAY_CONTROL_BIND_ADDR": cfg.ControlBindAddr} {
		if _, port, err := net.SplitHostPort(address); err != nil || !validPort(port) {
			return Config{}, fmt.Errorf("%s must be host:port", name)
		}
	}
	if _, port, _ := net.SplitHostPort(cfg.BindAddr); cfg.BindAddr == cfg.ControlBindAddr && port != "0" {
		return Config{}, fmt.Errorf("the relay and control listeners need different addresses")
	}
	cfg.MaxConnections = 4096
	if value := get("ARKNET_RELAY_MAX_CONNECTIONS"); value != "" {
		if cfg.MaxConnections, err = strconv.Atoi(value); err != nil || cfg.MaxConnections < 1 || cfg.MaxConnections > 65536 {
			return Config{}, fmt.Errorf("ARKNET_RELAY_MAX_CONNECTIONS must be between 1 and 65536")
		}
	}
	if cfg.MaxGrantLifetime, err = duration(get("ARKNET_RELAY_MAX_GRANT_LIFETIME"), 5*time.Minute, time.Minute, time.Hour, "ARKNET_RELAY_MAX_GRANT_LIFETIME"); err != nil {
		return Config{}, err
	}
	if cfg.ShutdownTimeout, err = duration(get("ARKNET_RELAY_SHUTDOWN_TIMEOUT"), 10*time.Second, time.Second, 5*time.Minute, "ARKNET_RELAY_SHUTDOWN_TIMEOUT"); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func endpointSigningKey(path string) (ed25519.PrivateKey, error) {
	const name = "ARKNET_RELAY_ENDPOINT_SIGNING_KEY_FILE"
	if path == "" {
		return nil, fmt.Errorf("%s is required", name)
	}
	encoded, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	text := strings.Trim(string(encoded), " \t\r\n")
	decoded, err := base64.StdEncoding.DecodeString(text)
	if err != nil || len(decoded) != ed25519.PrivateKeySize || base64.StdEncoding.EncodeToString(decoded) != text {
		return nil, fmt.Errorf("%s must hold canonical padded base64 for exactly 64 bytes", name)
	}
	if !bytes.Equal(decoded, ed25519.NewKeyFromSeed(decoded[:ed25519.SeedSize])) {
		return nil, fmt.Errorf("%s contains an inconsistent private key", name)
	}
	return ed25519.PrivateKey(decoded), nil
}

func publicKey(path, name string) (ed25519.PublicKey, error) {
	if path == "" {
		return nil, fmt.Errorf("%s is required", name)
	}
	encoded, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	text := strings.Trim(string(encoded), " \t\r\n")
	decoded, err := base64.StdEncoding.DecodeString(text)
	if err != nil || base64.StdEncoding.EncodeToString(decoded) != text || len(decoded) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("%s must hold canonical padded base64 for exactly 32 bytes", name)
	}
	return ed25519.PublicKey(decoded), nil
}

func orDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func validPort(text string) bool {
	port, err := strconv.ParseUint(text, 10, 16)
	return err == nil && (port > 0 || text == "0")
}

func duration(value string, fallback, minimum, maximum time.Duration, name string) (time.Duration, error) {
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed < minimum || parsed > maximum {
		return 0, fmt.Errorf("%s must be between %s and %s", name, minimum, maximum)
	}
	return parsed, nil
}

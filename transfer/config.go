package main

import (
	"errors"
	"net"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
)

type config struct {
	addr              string
	token             string
	trackerOrigin     string
	trackerAllowHTTP  bool
	webSeedOrigin     string
	webSeedAllowHTTP  bool
	stagingDir        string
	stateDir          string
	workerPath        string
	listenInterfaces  string
	downloadRateLimit int64
}

func loadConfig(get func(string) string) (config, error) {
	cfg := config{
		addr:             get("ARKNET_TRANSFER_ADDR"),
		token:            get("ARKNET_TRANSFER_TOKEN"),
		trackerOrigin:    get("ARKNET_TRANSFER_TRACKER_ORIGIN"),
		webSeedOrigin:    get("ARKNET_TRANSFER_WEB_SEED_ORIGIN"),
		stagingDir:       get("ARKNET_TRANSFER_STAGING_DIR"),
		stateDir:         get("ARKNET_TRANSFER_STATE_DIR"),
		workerPath:       get("ARKNET_TRANSFER_WORKER"),
		listenInterfaces: get("ARKNET_TRANSFER_LISTEN_INTERFACES"),
	}
	if len(cfg.token) < 32 || len(cfg.token) > 256 {
		return config{}, errors.New("invalid service token")
	}
	for _, b := range []byte(cfg.token) {
		if b < 33 || b > 126 {
			return config{}, errors.New("invalid service token")
		}
	}
	if cfg.addr == "" {
		cfg.addr = "127.0.0.1:8084"
	}
	if err := numericAddress(cfg.addr); err != nil {
		return config{}, errors.New("invalid listener address")
	}
	if cfg.listenInterfaces == "" {
		cfg.listenInterfaces = "0.0.0.0:0"
	}
	if err := numericAddress(cfg.listenInterfaces); err != nil {
		return config{}, errors.New("invalid peer listener address")
	}
	allowHTTP := get("ARKNET_TRANSFER_TRACKER_ALLOW_HTTP")
	if allowHTTP != "" && allowHTTP != "false" && allowHTTP != "true" {
		return config{}, errors.New("invalid tracker HTTP policy")
	}
	cfg.trackerAllowHTTP = allowHTTP == "true"
	if err := validateWebSeedOrigin(cfg.trackerOrigin, cfg.trackerAllowHTTP); err != nil {
		return config{}, errors.New("invalid tracker origin policy")
	}
	webSeedAllowHTTP := get("ARKNET_TRANSFER_WEB_SEED_ALLOW_HTTP")
	if webSeedAllowHTTP != "" && webSeedAllowHTTP != "false" && webSeedAllowHTTP != "true" {
		return config{}, errors.New("invalid web-seed HTTP policy")
	}
	cfg.webSeedAllowHTTP = webSeedAllowHTTP == "true"
	if cfg.webSeedOrigin == "" {
		if webSeedAllowHTTP != "" {
			return config{}, errors.New("invalid web-seed origin policy")
		}
	} else if err := validateWebSeedOrigin(cfg.webSeedOrigin, cfg.webSeedAllowHTTP); err != nil {
		return config{}, errors.New("invalid web-seed origin policy")
	}
	for label, value := range map[string]string{"staging directory": cfg.stagingDir, "state directory": cfg.stateDir, "worker path": cfg.workerPath} {
		if value == "" || !filepath.IsAbs(value) || filepath.Clean(value) != value || value == string(filepath.Separator) {
			return config{}, errors.New("invalid " + label)
		}
	}
	if cfg.stagingDir == cfg.stateDir {
		return config{}, errors.New("staging and state directories must differ")
	}
	if value := get("ARKNET_TRANSFER_DOWNLOAD_RATE_LIMIT"); value != "" {
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil || n < 0 {
			return config{}, errors.New("invalid download rate limit")
		}
		cfg.downloadRateLimit = n
	}
	return cfg, nil
}

func numericAddress(value string) error {
	host, port, err := net.SplitHostPort(value)
	if err != nil || net.ParseIP(host) == nil {
		return errors.New("invalid address")
	}
	n, err := strconv.ParseUint(port, 10, 16)
	if err != nil || n > 65535 {
		return errors.New("invalid address")
	}
	return nil
}

func validateWebSeedOrigin(value string, allowHTTP bool) error {
	u, err := url.Parse(value)
	if err != nil || u.Hostname() == "" || u.User != nil || u.Opaque != "" || u.Path != "" || u.RawPath != "" ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.ContainsAny(value, "%\\#?") ||
		value != u.Scheme+"://"+u.Host || strings.HasSuffix(u.Host, ":") {
		return errors.New("invalid origin")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && allowHTTP) {
		return errors.New("invalid origin scheme")
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return errors.New("invalid origin port")
		}
	}
	return nil
}

func validateTrackerURL(value string, allowHTTP bool) error {
	u, err := url.Parse(value)
	if err != nil || u.Hostname() == "" || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(value, "#") || u.EscapedPath() == "" || u.EscapedPath() == "/" || strings.HasSuffix(u.Host, ":") {
		return errors.New("invalid tracker")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && allowHTTP) {
		return errors.New("invalid tracker scheme")
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return errors.New("invalid tracker port")
		}
	}
	return nil
}

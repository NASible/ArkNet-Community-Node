package main

import (
	"arknet/core/internal/media"
	"arknet/core/internal/store"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type webSeedConfig struct {
	origin *url.URL
	root   string
}

func loadWebSeedConfig(originValue, rootPath string, issuer *trackerGrantIssuer) (*webSeedConfig, error) {
	if originValue == "" && rootPath == "" {
		return nil, nil
	}
	if originValue == "" || rootPath == "" {
		return nil, fmt.Errorf("ARKNET_CORE_WEB_SEED_ORIGIN and ARKNET_CORE_WEB_SEED_ROOT must be configured together")
	}
	if issuer == nil {
		return nil, fmt.Errorf("web seed requires the dedicated tracker signing configuration")
	}
	origin, err := url.Parse(originValue)
	if err != nil || origin.Host == "" || origin.Hostname() == "" || origin.User != nil || origin.Opaque != "" || origin.RawPath != "" || (origin.Path != "" && origin.Path != "/") || origin.RawQuery != "" || origin.ForceQuery || origin.Fragment != "" {
		return nil, fmt.Errorf("ARKNET_CORE_WEB_SEED_ORIGIN must be an origin URL without userinfo, path, query, or fragment")
	}
	if err := validateTrackerAuthority(origin); err != nil {
		return nil, fmt.Errorf("invalid ARKNET_CORE_WEB_SEED_ORIGIN authority")
	}
	if origin.Scheme != "https" {
		host := origin.Hostname()
		ip := net.ParseIP(host)
		allowedHTTP := origin.Scheme == "http" && (strings.EqualFold(host, "localhost") || strings.EqualFold(host, "arknet-core") || ip != nil && ip.IsLoopback())
		if !allowedHTTP {
			return nil, fmt.Errorf("ARKNET_CORE_WEB_SEED_ORIGIN must use HTTPS except for loopback or arknet-core")
		}
	}
	origin.Path = ""
	if !filepath.IsAbs(rootPath) || filepath.Clean(rootPath) != rootPath || rootPath == filepath.VolumeName(rootPath)+string(filepath.Separator) {
		return nil, fmt.Errorf("ARKNET_CORE_WEB_SEED_ROOT must be an absolute, clean, non-root directory")
	}
	info, err := os.Stat(rootPath)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("ARKNET_CORE_WEB_SEED_ROOT must be an accessible directory")
	}
	return &webSeedConfig{origin: origin, root: rootPath}, nil
}

func webSeedHandler(repo store.MediaCatalogRepository, issuer *trackerGrantIssuer, cfg *webSeedConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Type", "application/octet-stream")
		id := r.PathValue("id")
		name := r.PathValue("name")
		if !media.ValidMediaID(id) || (name != "" && name != id) || r.URL.RawQuery != "" || r.URL.ForceQuery {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		authorizedHash, authorized := issuer.verifyGrantClaims(r.PathValue("grant"), trackerGrantActionDownload)
		if !authorized {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		obj, err := repo.GetMediaObjectByPublicID(r.Context(), id)
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, "web seed unavailable", http.StatusServiceUnavailable)
			return
		}
		if obj.State != media.ObjectActive || !validInfoHashV2(obj.InfoHashV2) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		expectedHash, decodeErr := hex.DecodeString(obj.InfoHashV2)
		if decodeErr != nil || subtle.ConstantTimeCompare(authorizedHash, expectedHash) != 1 {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		root, err := os.OpenRoot(cfg.root)
		if err != nil {
			http.Error(w, "web seed unavailable", http.StatusServiceUnavailable)
			return
		}
		defer root.Close()
		info, err := root.Lstat(id)
		if errors.Is(err, os.ErrNotExist) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, "web seed unavailable", http.StatusServiceUnavailable)
			return
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			http.Error(w, "web seed unavailable", http.StatusServiceUnavailable)
			return
		}
		file, err := root.Open(id)
		if errors.Is(err, os.ErrNotExist) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, "web seed unavailable", http.StatusServiceUnavailable)
			return
		}
		defer file.Close()
		openedInfo, err := file.Stat()
		if err != nil || !openedInfo.Mode().IsRegular() || openedInfo.Size() != obj.Size {
			http.Error(w, "web seed unavailable", http.StatusServiceUnavailable)
			return
		}
		http.ServeContent(w, r, id, time.Time{}, file)
	}
}

const maxTorrentMetadataBytes = 16 << 20

func torrentMetadataHandler(repo store.MediaCatalogRepository, issuer *trackerGrantIssuer, cfg *webSeedConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Type", "application/x-bittorrent")
		id := r.PathValue("id")
		if !media.ValidMediaID(id) || r.URL.RawQuery != "" || r.URL.ForceQuery {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		authorizedHash, authorized := issuer.verifyGrantClaims(r.PathValue("grant"), trackerGrantActionDownload)
		if !authorized {
			// Seed grants are issued only to Core-eligible sources; they may read
			// metadata to verify their local copy but never web-seed payload.
			authorizedHash, authorized = issuer.verifyGrantClaims(r.PathValue("grant"), trackerGrantActionSeed)
		}
		if !authorized {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		obj, err := repo.GetMediaObjectByPublicID(r.Context(), id)
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, "torrent metadata unavailable", http.StatusServiceUnavailable)
			return
		}
		if obj.State != media.ObjectActive || !validInfoHashV2(obj.InfoHashV2) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		expectedHash, decodeErr := hex.DecodeString(obj.InfoHashV2)
		if decodeErr != nil || subtle.ConstantTimeCompare(authorizedHash, expectedHash) != 1 {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		root, err := os.OpenRoot(cfg.root)
		if err != nil {
			http.Error(w, "torrent metadata unavailable", http.StatusServiceUnavailable)
			return
		}
		defer root.Close()
		name := id + ".torrent"
		info, err := root.Lstat(name)
		if errors.Is(err, os.ErrNotExist) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxTorrentMetadataBytes {
			http.Error(w, "torrent metadata unavailable", http.StatusServiceUnavailable)
			return
		}
		file, err := root.Open(name)
		if err != nil {
			http.Error(w, "torrent metadata unavailable", http.StatusServiceUnavailable)
			return
		}
		defer file.Close()
		openedInfo, err := file.Stat()
		if err != nil || !openedInfo.Mode().IsRegular() || openedInfo.Size() != info.Size() {
			http.Error(w, "torrent metadata unavailable", http.StatusServiceUnavailable)
			return
		}
		http.ServeContent(w, r, name, time.Time{}, file)
	}
}

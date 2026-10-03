// Package server exposes the frozen public surface of VaultGrid.
//
// In addition to process health, it serves the storage-pool, capacity
// reservation and volume lifecycle HTTP API described in README.md. The
// exported surface (Handler, Version, VAULTGRID_ADDR handling) stays
// backward compatible.
package server

import (
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
)

// Version is the release identifier.
const Version = "0.1.0"

// idPattern restricts every client-supplied id: 1..64 letters, digits,
// dots, underscores or hyphens.
var idPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

type health struct {
	Status  string `json:"status"`
	Service string `json:"service"`
	Version string `json:"version"`
}

// Handler returns the HTTP surface served by VaultGrid.
func Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, `{"error":{"code":"method_not_allowed"}}`, http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(health{Status: "ok", Service: "vaultgrid", Version: Version})
	})
	mux.HandleFunc("/", newStore().route)
	return mux
}

const (
	storagePoolPath = "/v1/storage-pools"
	volumePath      = "/v1/volumes"
	snapshotPath    = "/v1/snapshots"
	bucketPath      = "/v1/buckets"
)

// route dispatches every non-healthz request. Paths it does not recognise
// answer 404/not_found; recognised paths with the wrong verb answer 405.
func (s *store) route(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == volumePath || strings.HasPrefix(r.URL.Path, volumePath+"/"):
		s.routeVolumes(w, r)
	case r.URL.Path == snapshotPath || strings.HasPrefix(r.URL.Path, snapshotPath+"/"):
		s.routeSnapshots(w, r)
	case r.URL.Path == bucketPath || strings.HasPrefix(r.URL.Path, bucketPath+"/"):
		s.routeBuckets(w, r)
	case r.URL.Path == storagePoolPath || strings.HasPrefix(r.URL.Path, storagePoolPath+"/"):
		s.routeStoragePools(w, r)
	default:
		writeError(w, http.StatusNotFound, "not_found")
	}
}

// routeStoragePools dispatches /v1/storage-pools and its sub-paths.
func (s *store) routeStoragePools(w http.ResponseWriter, r *http.Request) {
	parts := splitPath(r.URL.Path, storagePoolPath)
	for _, seg := range parts {
		if seg == "" {
			writeError(w, http.StatusNotFound, "not_found")
			return
		}
	}
	switch {
	case len(parts) == 0:
		switch r.Method {
		case http.MethodGet:
			s.listPools(w)
		case http.MethodPost:
			s.createPool(w, r)
		default:
			methodNotAllowed(w, http.MethodGet, http.MethodPost)
		}
	case len(parts) == 1:
		switch r.Method {
		case http.MethodGet:
			s.getPool(w, parts[0])
		case http.MethodDelete:
			s.deletePool(w, parts[0])
		default:
			methodNotAllowed(w, http.MethodGet, http.MethodDelete)
		}
	case len(parts) == 2 && parts[1] == "reservations":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		s.createReservation(w, r, parts[0])
	case len(parts) == 3 && parts[1] == "reservations":
		if r.Method != http.MethodDelete {
			methodNotAllowed(w, http.MethodDelete)
			return
		}
		s.deleteReservation(w, parts[0], parts[2])
	default:
		writeError(w, http.StatusNotFound, "not_found")
	}
}

// splitPath splits the tail after prefix into path segments.
func splitPath(path, prefix string) []string {
	rest := strings.TrimPrefix(path, prefix)
	if rest == "" {
		return nil
	}
	segments := strings.Split(rest, "/")
	// Drop the single empty segment introduced by the leading slash, but
	// keep trailing/inner empty segments so the caller can reject them 404.
	if segments[0] == "" {
		segments = segments[1:]
	}
	return segments
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code}})
}

func methodNotAllowed(w http.ResponseWriter, allowed ...string) {
	w.Header().Set("Allow", strings.Join(allowed, ", "))
	writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
}

// decodeRequest parses exactly one JSON value, rejecting unknown fields and
// any trailing content so malformed bodies cannot partially apply.
func decodeRequest(r *http.Request, v any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return false
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		return false
	}
	return true
}

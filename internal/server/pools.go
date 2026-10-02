package server

import (
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// idPattern constrains pool, device and request identifiers.
var idPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

func validID(s string) bool { return idPattern.MatchString(s) }

// deviceIn is the wire representation of a device in a create-pool request.
type deviceIn struct {
	ID            string          `json:"id"`
	CapacityBytes json.RawMessage `json:"capacityBytes"`
	FaultDomain   string          `json:"faultDomain"`
}

// deviceView is the stored form of a device.
type deviceView struct {
	ID            string `json:"id"`
	CapacityBytes int64  `json:"capacityBytes"`
	FaultDomain   string `json:"faultDomain"`
}

// poolView is the wire representation of a storage pool.
type poolView struct {
	ID               string       `json:"id"`
	Devices          []deviceView `json:"devices"`
	RawCapacityBytes int64        `json:"rawCapacityBytes"`
	AllocatedBytes   int64        `json:"allocatedBytes"`
	AvailableBytes   int64        `json:"availableBytes"`
}

type reservation struct {
	RequestID string
	Bytes     int64
}

type pool struct {
	id           string
	devices      []deviceView
	rawCapacity  int64
	reservations map[string]int64 // requestId -> bytes
	allocated    int64
}

func (p *pool) view() poolView {
	devices := make([]deviceView, len(p.devices))
	copy(devices, p.devices)
	return poolView{
		ID:               p.id,
		Devices:          devices,
		RawCapacityBytes: p.rawCapacity,
		AllocatedBytes:   p.allocated,
		AvailableBytes:   p.rawCapacity - p.allocated,
	}
}

// store holds all mutable state. State is process-local: a restart clears it.
type store struct {
	mu      sync.Mutex
	pools   map[string]*pool
	devices map[string]string // device id -> owning pool id
}

func newStore() *store {
	return &store{
		pools:   make(map[string]*pool),
		devices: make(map[string]string),
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code}})
}

func writeMethodNotAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
}

func invalidRequest(w http.ResponseWriter) {
	writeError(w, http.StatusBadRequest, "invalid_request")
}

// decodeBody strictly decodes a single JSON object from the request body.
// Unknown fields, trailing data and malformed JSON are rejected.
func decodeBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		invalidRequest(w)
		return false
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		invalidRequest(w)
		return false
	}
	return true
}

// parsePositiveInt validates that raw is a JSON number token holding a
// base-10 positive integer. Strings, fractions, exponents and non-positive
// values are rejected.
func parsePositiveInt(raw json.RawMessage) (int64, bool) {
	s := strings.TrimSpace(string(raw))
	if s == "" {
		return 0, false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil || v <= 0 {
		return 0, false
	}
	return v, true
}

// route dispatches the /v1 API surface.
func (s *store) route(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case path == "/v1/storage-pools":
		s.handlePools(w, r)
	case strings.HasPrefix(path, "/v1/storage-pools/"):
		s.handlePoolMember(w, r, strings.TrimPrefix(path, "/v1/storage-pools/"))
	default:
		writeError(w, http.StatusNotFound, "not_found")
	}
}

func (s *store) handlePools(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listPools(w)
	case http.MethodPost:
		s.createPool(w, r)
	default:
		writeMethodNotAllowed(w, "GET, POST")
	}
}

func (s *store) handlePoolMember(w http.ResponseWriter, r *http.Request, rest string) {
	parts := strings.Split(rest, "/")
	switch {
	case len(parts) == 1 && parts[0] != "":
		switch r.Method {
		case http.MethodGet:
			s.getPool(w, parts[0])
		case http.MethodDelete:
			s.deletePool(w, parts[0])
		default:
			writeMethodNotAllowed(w, "GET, DELETE")
		}
	case len(parts) == 2 && parts[0] != "" && parts[1] == "reservations":
		if r.Method != http.MethodPost {
			writeMethodNotAllowed(w, "POST")
			return
		}
		s.createReservation(w, r, parts[0])
	case len(parts) == 3 && parts[0] != "" && parts[1] == "reservations" && parts[2] != "":
		if r.Method != http.MethodDelete {
			writeMethodNotAllowed(w, "DELETE")
			return
		}
		s.deleteReservation(w, parts[0], parts[2])
	default:
		writeError(w, http.StatusNotFound, "not_found")
	}
}

func (s *store) listPools(w http.ResponseWriter) {
	s.mu.Lock()
	views := make([]poolView, 0, len(s.pools))
	for _, p := range s.pools {
		views = append(views, p.view())
	}
	s.mu.Unlock()
	sort.Slice(views, func(i, j int) bool { return views[i].ID < views[j].ID })
	writeJSON(w, http.StatusOK, map[string]any{"items": views})
}

type createPoolRequest struct {
	ID      string     `json:"id"`
	Devices []deviceIn `json:"devices"`
}

func (s *store) createPool(w http.ResponseWriter, r *http.Request) {
	var req createPoolRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if !validID(req.ID) || len(req.Devices) == 0 {
		invalidRequest(w)
		return
	}
	devices := make([]deviceView, 0, len(req.Devices))
	var raw int64
	for _, d := range req.Devices {
		if !validID(d.ID) || d.FaultDomain == "" {
			invalidRequest(w)
			return
		}
		cap, ok := parsePositiveInt(d.CapacityBytes)
		if !ok {
			invalidRequest(w)
			return
		}
		devices = append(devices, deviceView{ID: d.ID, CapacityBytes: cap, FaultDomain: d.FaultDomain})
		raw += cap
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.pools[req.ID]; exists {
		writeError(w, http.StatusConflict, "pool_exists")
		return
	}
	seen := make(map[string]struct{}, len(devices))
	for _, d := range devices {
		if _, dup := seen[d.ID]; dup {
			writeError(w, http.StatusConflict, "device_in_use")
			return
		}
		seen[d.ID] = struct{}{}
		if _, taken := s.devices[d.ID]; taken {
			writeError(w, http.StatusConflict, "device_in_use")
			return
		}
	}
	p := &pool{
		id:           req.ID,
		devices:      devices,
		rawCapacity:  raw,
		reservations: make(map[string]int64),
	}
	s.pools[p.id] = p
	for _, d := range devices {
		s.devices[d.ID] = p.id
	}
	writeJSON(w, http.StatusCreated, p.view())
}

func (s *store) getPool(w http.ResponseWriter, id string) {
	s.mu.Lock()
	p, ok := s.pools[id]
	s.mu.Unlock()
	if !ok {
		writeError(w, http.StatusNotFound, "pool_not_found")
		return
	}
	writeJSON(w, http.StatusOK, p.view())
}

func (s *store) deletePool(w http.ResponseWriter, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.pools[id]
	if !ok {
		writeError(w, http.StatusNotFound, "pool_not_found")
		return
	}
	if len(p.reservations) > 0 {
		writeError(w, http.StatusConflict, "pool_not_empty")
		return
	}
	delete(s.pools, id)
	for _, d := range p.devices {
		delete(s.devices, d.ID)
	}
	w.WriteHeader(http.StatusNoContent)
}

type createReservationRequest struct {
	RequestID string          `json:"requestId"`
	Bytes     json.RawMessage `json:"bytes"`
}

type reservationView struct {
	PoolID    string `json:"poolId"`
	RequestID string `json:"requestId"`
	Bytes     int64  `json:"bytes"`
}

func (s *store) createReservation(w http.ResponseWriter, r *http.Request, poolID string) {
	var req createReservationRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if !validID(req.RequestID) {
		invalidRequest(w)
		return
	}
	bytes, ok := parsePositiveInt(req.Bytes)
	if !ok {
		invalidRequest(w)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.pools[poolID]
	if !ok {
		writeError(w, http.StatusNotFound, "pool_not_found")
		return
	}
	if existing, dup := p.reservations[req.RequestID]; dup {
		if existing != bytes {
			writeError(w, http.StatusConflict, "idempotency_conflict")
			return
		}
		writeJSON(w, http.StatusOK, reservationView{PoolID: p.id, RequestID: req.RequestID, Bytes: bytes})
		return
	}
	if p.rawCapacity-p.allocated < bytes {
		writeError(w, http.StatusConflict, "insufficient_capacity")
		return
	}
	p.reservations[req.RequestID] = bytes
	p.allocated += bytes
	writeJSON(w, http.StatusCreated, reservationView{PoolID: p.id, RequestID: req.RequestID, Bytes: bytes})
}

func (s *store) deleteReservation(w http.ResponseWriter, poolID, requestID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.pools[poolID]
	if !ok {
		writeError(w, http.StatusNotFound, "pool_not_found")
		return
	}
	if bytes, ok := p.reservations[requestID]; ok {
		delete(p.reservations, requestID)
		p.allocated -= bytes
	}
	w.WriteHeader(http.StatusNoContent)
}

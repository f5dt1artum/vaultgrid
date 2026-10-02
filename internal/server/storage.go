package server

import (
	"math"
	"net/http"
	"sort"
	"sync"
)

// deviceInput is one member of a create-pool request.
type deviceInput struct {
	ID            string `json:"id"`
	CapacityBytes int64  `json:"capacityBytes"`
	FaultDomain   string `json:"faultDomain"`
}

type createPoolInput struct {
	ID      string        `json:"id"`
	Devices []deviceInput `json:"devices"`
}

type reservationInput struct {
	RequestID string `json:"requestId"`
	Bytes     int64  `json:"bytes"`
}

// deviceView is the public representation of a pool device.
type deviceView struct {
	ID            string `json:"id"`
	CapacityBytes int64  `json:"capacityBytes"`
	FaultDomain   string `json:"faultDomain"`
}

// poolView is the public representation of a storage pool.
type poolView struct {
	ID               string       `json:"id"`
	Devices          []deviceView `json:"devices"`
	RawCapacityBytes int64        `json:"rawCapacityBytes"`
	AllocatedBytes   int64        `json:"allocatedBytes"`
	AvailableBytes   int64        `json:"availableBytes"`
}

// reservationView is the result of a successful reservation request.
type reservationView struct {
	PoolID    string `json:"poolId"`
	RequestID string `json:"requestId"`
	Bytes     int64  `json:"bytes"`
}

type reservation struct {
	requestID string
	bytes     int64
}

type pool struct {
	id           string
	devices      []deviceInput
	rawCapacity  int64
	reservations map[string]reservation
	volumeBytes  int64 // total sizeBytes of volumes in this pool
}

// volume is a capacity allocation inside a pool with an optional exclusive
// node binding. generation is bumped on every binding state change.
type volume struct {
	id         string
	poolID     string
	sizeBytes  int64
	generation int64
	binding    string // "" when unbound
}

func (p *pool) allocated() int64 {
	var total int64 = p.volumeBytes
	for _, r := range p.reservations {
		total += r.bytes
	}
	return total
}

func (p *pool) view() poolView {
	devices := make([]deviceView, len(p.devices))
	for i, d := range p.devices {
		devices[i] = deviceView{ID: d.ID, CapacityBytes: d.CapacityBytes, FaultDomain: d.FaultDomain}
	}
	allocated := p.allocated()
	return poolView{
		ID:               p.id,
		Devices:          devices,
		RawCapacityBytes: p.rawCapacity,
		AllocatedBytes:   allocated,
		AvailableBytes:   p.rawCapacity - allocated,
	}
}

// store is the in-memory state of the service. It is wiped on restart.
type store struct {
	mu          sync.RWMutex
	pools       map[string]*pool
	volumes     map[string]*volume
	deviceOwner map[string]string
}

func newStore() *store {
	return &store{
		pools:       make(map[string]*pool),
		volumes:     make(map[string]*volume),
		deviceOwner: make(map[string]string),
	}
}

func validID(id string) bool {
	return idPattern.MatchString(id)
}

func (s *store) listPools(w http.ResponseWriter) {
	s.mu.RLock()
	ids := make([]string, 0, len(s.pools))
	for id := range s.pools {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	items := make([]poolView, 0, len(ids))
	for _, id := range ids {
		items = append(items, s.pools[id].view())
	}
	s.mu.RUnlock()
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *store) getPool(w http.ResponseWriter, id string) {
	if !validID(id) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	s.mu.RLock()
	p, ok := s.pools[id]
	var v poolView
	if ok {
		v = p.view()
	}
	s.mu.RUnlock()
	if !ok {
		writeError(w, http.StatusNotFound, "pool_not_found")
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *store) createPool(w http.ResponseWriter, r *http.Request) {
	var in createPoolInput
	if !decodeRequest(r, &in) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	// Validate the whole payload before touching any state so a failure
	// never leaves a partial pool behind.
	if !validID(in.ID) || len(in.Devices) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	seen := make(map[string]bool, len(in.Devices))
	var rawCapacity int64
	for _, d := range in.Devices {
		if !validID(d.ID) || d.CapacityBytes <= 0 || d.FaultDomain == "" || seen[d.ID] {
			writeError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		if rawCapacity > math.MaxInt64-d.CapacityBytes {
			writeError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		seen[d.ID] = true
		rawCapacity += d.CapacityBytes
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.pools[in.ID]; ok {
		writeError(w, http.StatusConflict, "pool_exists")
		return
	}
	for _, d := range in.Devices {
		if _, busy := s.deviceOwner[d.ID]; busy {
			writeError(w, http.StatusConflict, "device_in_use")
			return
		}
	}

	devices := append([]deviceInput(nil), in.Devices...)
	p := &pool{
		id:           in.ID,
		devices:      devices,
		rawCapacity:  rawCapacity,
		reservations: make(map[string]reservation),
	}
	s.pools[in.ID] = p
	for _, d := range devices {
		s.deviceOwner[d.ID] = in.ID
	}
	writeJSON(w, http.StatusCreated, p.view())
}

func (s *store) deletePool(w http.ResponseWriter, id string) {
	if !validID(id) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.pools[id]
	if !ok {
		writeError(w, http.StatusNotFound, "pool_not_found")
		return
	}
	if len(p.reservations) > 0 || p.volumeBytes > 0 {
		writeError(w, http.StatusConflict, "pool_not_empty")
		return
	}
	for _, d := range p.devices {
		delete(s.deviceOwner, d.ID)
	}
	delete(s.pools, id)
	w.WriteHeader(http.StatusNoContent)
}

func (s *store) createReservation(w http.ResponseWriter, r *http.Request, poolID string) {
	var in reservationInput
	if !decodeRequest(r, &in) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if !validID(poolID) || !validID(in.RequestID) || in.Bytes <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.pools[poolID]
	if !ok {
		writeError(w, http.StatusNotFound, "pool_not_found")
		return
	}
	if existing, dup := p.reservations[in.RequestID]; dup {
		if existing.bytes != in.Bytes {
			writeError(w, http.StatusConflict, "idempotency_conflict")
			return
		}
		writeJSON(w, http.StatusOK, reservationView{PoolID: poolID, RequestID: existing.requestID, Bytes: existing.bytes})
		return
	}
	allocated := p.allocated()
	if allocated > p.rawCapacity-in.Bytes {
		writeError(w, http.StatusConflict, "insufficient_capacity")
		return
	}
	p.reservations[in.RequestID] = reservation{requestID: in.RequestID, bytes: in.Bytes}
	writeJSON(w, http.StatusCreated, reservationView{PoolID: poolID, RequestID: in.RequestID, Bytes: in.Bytes})
}

func (s *store) deleteReservation(w http.ResponseWriter, poolID, requestID string) {
	if !validID(poolID) || !validID(requestID) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.pools[poolID]
	if !ok {
		writeError(w, http.StatusNotFound, "pool_not_found")
		return
	}
	// Deleting an unknown or already-deleted reservation is a success.
	delete(p.reservations, requestID)
	w.WriteHeader(http.StatusNoContent)
}

package server

import (
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
)

// volumeInput is the create-volume request body.
type volumeInput struct {
	ID        string `json:"id"`
	PoolID    string `json:"poolId"`
	SizeBytes int64  `json:"sizeBytes"`
}

// bindingInput is the put-binding request body. ExpectedGeneration is a
// pointer so a missing field is rejected instead of defaulting to 0.
type bindingInput struct {
	NodeID             string `json:"nodeId"`
	ExpectedGeneration *int64 `json:"expectedGeneration"`
}

// bindingView is the public representation of a volume's node binding.
type bindingView struct {
	NodeID string `json:"nodeId"`
}

// volumeView is the public representation of a volume.
type volumeView struct {
	ID         string       `json:"id"`
	PoolID     string       `json:"poolId"`
	SizeBytes  int64        `json:"sizeBytes"`
	Generation int64        `json:"generation"`
	Binding    *bindingView `json:"binding"`
}

type volume struct {
	id         string
	poolID     string
	sizeBytes  int64
	generation int64
	bound      bool
	boundTo    string
}

func (v *volume) view() volumeView {
	var b *bindingView
	if v.bound {
		b = &bindingView{NodeID: v.boundTo}
	}
	return volumeView{
		ID:         v.id,
		PoolID:     v.poolID,
		SizeBytes:  v.sizeBytes,
		Generation: v.generation,
		Binding:    b,
	}
}

// volumeBytes sums the capacity held by volumes of one pool. Callers must
// hold s.mu.
func (s *store) volumeBytes(poolID string) int64 {
	var total int64
	for _, v := range s.volumes {
		if v.poolID == poolID {
			total += v.sizeBytes
		}
	}
	return total
}

func (s *store) listVolumes(w http.ResponseWriter) {
	s.mu.RLock()
	ids := make([]string, 0, len(s.volumes))
	for id := range s.volumes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	items := make([]volumeView, 0, len(ids))
	for _, id := range ids {
		items = append(items, s.volumes[id].view())
	}
	s.mu.RUnlock()
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *store) getVolume(w http.ResponseWriter, id string) {
	if !validID(id) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	s.mu.RLock()
	v, ok := s.volumes[id]
	var view volumeView
	if ok {
		view = v.view()
	}
	s.mu.RUnlock()
	if !ok {
		writeError(w, http.StatusNotFound, "volume_not_found")
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *store) createVolume(w http.ResponseWriter, r *http.Request) {
	var in volumeInput
	if !decodeRequest(r, &in) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	// Validate the whole payload before touching any state so a failure
	// never leaves a partial volume or allocation behind.
	if !validID(in.ID) || !validID(in.PoolID) || in.SizeBytes <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.pools[in.PoolID]
	if !ok {
		writeError(w, http.StatusNotFound, "pool_not_found")
		return
	}
	if existing, dup := s.volumes[in.ID]; dup {
		if existing.poolID != in.PoolID || existing.sizeBytes != in.SizeBytes {
			writeError(w, http.StatusConflict, "volume_exists")
			return
		}
		writeJSON(w, http.StatusOK, existing.view())
		return
	}
	allocated := p.allocated() + s.volumeBytes(p.id)
	if allocated > p.rawCapacity-in.SizeBytes {
		writeError(w, http.StatusConflict, "insufficient_capacity")
		return
	}
	v := &volume{id: in.ID, poolID: in.PoolID, sizeBytes: in.SizeBytes}
	s.volumes[in.ID] = v
	writeJSON(w, http.StatusCreated, v.view())
}

func (s *store) deleteVolume(w http.ResponseWriter, id string) {
	if !validID(id) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.volumes[id]
	if !ok {
		writeError(w, http.StatusNotFound, "volume_not_found")
		return
	}
	if v.bound {
		writeError(w, http.StatusConflict, "volume_in_use")
		return
	}
	delete(s.volumes, id)
	w.WriteHeader(http.StatusNoContent)
}

func (s *store) putBinding(w http.ResponseWriter, r *http.Request, id string) {
	var in bindingInput
	if !decodeRequest(r, &in) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if !validID(id) || !validID(in.NodeID) || in.ExpectedGeneration == nil || *in.ExpectedGeneration < 0 {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.volumes[id]
	if !ok {
		writeError(w, http.StatusNotFound, "volume_not_found")
		return
	}
	if *in.ExpectedGeneration != v.generation {
		writeError(w, http.StatusConflict, "stale_generation")
		return
	}
	if v.bound {
		if v.boundTo != in.NodeID {
			writeError(w, http.StatusConflict, "volume_already_bound")
			return
		}
		// Idempotent retry: same node, matching generation — report the
		// current state without advancing it.
		writeJSON(w, http.StatusOK, v.view())
		return
	}
	v.bound = true
	v.boundTo = in.NodeID
	v.generation++
	writeJSON(w, http.StatusOK, v.view())
}

// generationPattern restricts the expectedGeneration query parameter to a
// non-negative decimal integer.
var generationPattern = regexp.MustCompile(`^[0-9]+$`)

// parseExpectedGeneration requires exactly one query parameter,
// expectedGeneration, holding a single non-negative integer.
func parseExpectedGeneration(q url.Values) (int64, bool) {
	if len(q) != 1 {
		return 0, false
	}
	vals, ok := q["expectedGeneration"]
	if !ok || len(vals) != 1 || !generationPattern.MatchString(vals[0]) {
		return 0, false
	}
	n, err := strconv.ParseInt(vals[0], 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

func (s *store) deleteBinding(w http.ResponseWriter, r *http.Request, id string) {
	if !validID(id) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	gen, ok := parseExpectedGeneration(r.URL.Query())
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.volumes[id]
	if !ok {
		writeError(w, http.StatusNotFound, "volume_not_found")
		return
	}
	if gen != v.generation {
		writeError(w, http.StatusConflict, "stale_generation")
		return
	}
	if v.bound {
		v.bound = false
		v.boundTo = ""
		v.generation++
	}
	w.WriteHeader(http.StatusNoContent)
}

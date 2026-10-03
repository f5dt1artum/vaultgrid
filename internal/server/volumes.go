package server

import (
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

type createVolumeInput struct {
	ID        string `json:"id"`
	PoolID    string `json:"poolId"`
	SizeBytes int64  `json:"sizeBytes"`
}

type bindingInput struct {
	NodeID             string `json:"nodeId"`
	ExpectedGeneration *int64 `json:"expectedGeneration"`
}

type resizeVolumeInput struct {
	SizeBytes          int64  `json:"sizeBytes"`
	ExpectedGeneration *int64 `json:"expectedGeneration"`
}

type restoreVolumeInput struct {
	SnapshotID         string `json:"snapshotId"`
	ExpectedGeneration *int64 `json:"expectedGeneration"`
}

// bindingView is the public representation of a volume binding.
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

func (v *volume) view() volumeView {
	view := volumeView{
		ID:         v.id,
		PoolID:     v.poolID,
		SizeBytes:  v.sizeBytes,
		Generation: v.generation,
		Binding:    nil,
	}
	if v.binding != "" {
		view.Binding = &bindingView{NodeID: v.binding}
	}
	return view
}

// routeVolumes dispatches /v1/volumes and its sub-paths.
func (s *store) routeVolumes(w http.ResponseWriter, r *http.Request) {
	parts := splitPath(r.URL.Path, volumePath)
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
			s.listVolumes(w)
		case http.MethodPost:
			s.createVolume(w, r)
		default:
			methodNotAllowed(w, http.MethodGet, http.MethodPost)
		}
	case len(parts) == 1:
		switch r.Method {
		case http.MethodGet:
			s.getVolume(w, parts[0])
		case http.MethodDelete:
			s.deleteVolume(w, parts[0])
		default:
			methodNotAllowed(w, http.MethodGet, http.MethodDelete)
		}
	case len(parts) == 2 && parts[1] == "binding":
		switch r.Method {
		case http.MethodPut:
			s.putBinding(w, r, parts[0])
		case http.MethodDelete:
			s.deleteBinding(w, r, parts[0])
		default:
			methodNotAllowed(w, http.MethodPut, http.MethodDelete)
		}
	case len(parts) == 2 && parts[1] == "size":
		if r.Method != http.MethodPut {
			methodNotAllowed(w, http.MethodPut)
			return
		}
		s.putVolumeSize(w, r, parts[0])
	case len(parts) == 2 && parts[1] == "snapshots":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		s.createSnapshot(w, r, parts[0])
	case len(parts) == 2 && parts[1] == "restore":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		s.restoreVolume(w, r, parts[0])
	default:
		writeError(w, http.StatusNotFound, "not_found")
	}
}

func (s *store) listVolumes(w http.ResponseWriter) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids := make([]string, 0, len(s.volumes))
	for id := range s.volumes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	items := make([]volumeView, 0, len(ids))
	for _, id := range ids {
		items = append(items, s.volumes[id].view())
	}
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
	var in createVolumeInput
	if !decodeRequest(r, &in) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	tenant, tok := extractTenant(r.Header)
	if !tok || !validID(in.ID) || !validID(in.PoolID) || in.SizeBytes <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, dup := s.volumes[in.ID]; dup {
		if existing.poolID == in.PoolID && existing.createdSizeBytes == in.SizeBytes && existing.tenant == tenant {
			writeJSON(w, http.StatusOK, existing.view())
			return
		}
		writeError(w, http.StatusConflict, "volume_exists")
		return
	}
	p, ok := s.pools[in.PoolID]
	if !ok {
		writeError(w, http.StatusNotFound, "pool_not_found")
		return
	}
	// Holding the store lock makes the quota and capacity checks plus the
	// charge atomic, so concurrent creates can never exceed either limit.
	if p.tenantQuotaExceeded(tenant, in.SizeBytes) {
		writeError(w, http.StatusConflict, "tenant_quota_exceeded")
		return
	}
	if p.allocated() > p.rawCapacity-in.SizeBytes {
		writeError(w, http.StatusConflict, "insufficient_capacity")
		return
	}
	v := &volume{id: in.ID, poolID: in.PoolID, sizeBytes: in.SizeBytes, createdSizeBytes: in.SizeBytes, tenant: tenant}
	s.volumeInstanceSeq++
	v.instanceID = s.volumeInstanceSeq
	s.volumes[in.ID] = v
	p.volumeBytes += in.SizeBytes
	p.tenantUsed[tenant] += in.SizeBytes
	s.appendAudit(auditVolumeCreated, volumePath+"/"+in.ID, in.PoolID, in.SizeBytes)
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
	if v.binding != "" {
		writeError(w, http.StatusConflict, "volume_in_use")
		return
	}
	s.pools[v.poolID].volumeBytes -= v.sizeBytes
	s.pools[v.poolID].tenantUsed[v.tenant] -= v.sizeBytes
	delete(s.volumes, id)
	s.appendAudit(auditVolumeDeleted, volumePath+"/"+id, v.poolID, -v.sizeBytes)
	w.WriteHeader(http.StatusNoContent)
}

func (s *store) putBinding(w http.ResponseWriter, r *http.Request, volumeID string) {
	var in bindingInput
	if !decodeRequest(r, &in) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if !validID(volumeID) || !validID(in.NodeID) || in.ExpectedGeneration == nil || *in.ExpectedGeneration < 0 {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	expected := *in.ExpectedGeneration

	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.volumes[volumeID]
	if !ok {
		writeError(w, http.StatusNotFound, "volume_not_found")
		return
	}
	if v.generation != expected {
		writeError(w, http.StatusConflict, "stale_generation")
		return
	}
	switch {
	case v.binding == "":
		v.binding = in.NodeID
		v.generation++
		s.appendAudit(auditVolumeBound, volumePath+"/"+volumeID+"/binding", v.poolID, 0)
	case v.binding != in.NodeID:
		writeError(w, http.StatusConflict, "volume_already_bound")
		return
	}
	// A same-node, same-generation PUT is an idempotent no-op that returns
	// the current representation without bumping the generation.
	writeJSON(w, http.StatusOK, v.view())
}

// putVolumeSize handles PUT /v1/volumes/{id}/size. It changes an existing
// volume's size under optimistic concurrency: expectedGeneration must name the
// volume's current generation, and the check plus the commit run inside one
// store-lock critical section, so concurrent requests using the same
// generation can have at most one winner.
//
// A bound volume is rejected with volume_in_use before the generation is
// compared. Growth is charged to the owning tenant's quota first and then to
// pool capacity; shrinkage is subject to neither. A request naming the current
// size is a successful no-op: no generation bump, no accounting and no audit
// event.
func (s *store) putVolumeSize(w http.ResponseWriter, r *http.Request, volumeID string) {
	// The sub-resource accepts no query string at all (ForceQuery catches a
	// bare trailing "?" whose RawQuery is empty).
	if r.URL.ForceQuery || r.URL.RawQuery != "" {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	var in resizeVolumeInput
	if !decodeRequest(r, &in) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if !validID(volumeID) || in.SizeBytes <= 0 || in.ExpectedGeneration == nil || *in.ExpectedGeneration < 0 {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	expected := *in.ExpectedGeneration

	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.volumes[volumeID]
	if !ok {
		writeError(w, http.StatusNotFound, "volume_not_found")
		return
	}
	if v.binding != "" {
		writeError(w, http.StatusConflict, "volume_in_use")
		return
	}
	if v.generation != expected {
		writeError(w, http.StatusConflict, "stale_generation")
		return
	}
	p := s.pools[v.poolID]
	delta := in.SizeBytes - v.sizeBytes
	if delta > 0 {
		// Growth is charged to the owning tenant's quota before pool capacity,
		// matching volume creation; shrinkage is subject to neither.
		if p.tenantQuotaExceeded(v.tenant, delta) {
			writeError(w, http.StatusConflict, "tenant_quota_exceeded")
			return
		}
		if p.allocated() > p.rawCapacity-delta {
			writeError(w, http.StatusConflict, "insufficient_capacity")
			return
		}
	}
	if delta != 0 {
		// delta is positive for growth and negative for shrinkage, so a single
		// signed adjustment covers the pool and the tenant's usage.
		p.volumeBytes += delta
		p.tenantUsed[v.tenant] += delta
		v.sizeBytes = in.SizeBytes
		v.generation++
		s.appendAudit(auditVolumeResized, volumePath+"/"+volumeID+"/size", v.poolID, delta)
	}
	// delta == 0 is an idempotent no-op: return the current representation
	// without bumping the generation, touching accounting or appending an event.
	writeJSON(w, http.StatusOK, v.view())
}

// restoreVolume handles POST /v1/volumes/{id}/restore. It rolls the target
// volume back to the size captured by one of its own snapshots, in place: no
// new volume is created and the snapshot is kept. expectedGeneration must name
// the volume's current generation, and the check plus the commit run inside
// one store-lock critical section, so a restore racing a bind, resize, delete
// or snapshot delete is equivalent to some serial order of the two.
//
// The snapshot must have been produced by the volume's current instance: a
// snapshot of a different volume, or of a deleted volume whose id was later
// reused, is rejected with snapshot_source_mismatch. A bound volume is
// rejected with volume_in_use before the generation is compared. Restoring to
// a larger snapshot is charged to the volume's tenant quota first and then to
// pool capacity; shrinking is subject to neither. Unlike a same-size resize, a
// restore always commits a new state: the generation bumps even when the size
// does not change, and a volume.restored event is appended with the signed
// byte delta (zero included).
func (s *store) restoreVolume(w http.ResponseWriter, r *http.Request, volumeID string) {
	// The sub-resource accepts no query string at all (ForceQuery catches a
	// bare trailing "?" whose RawQuery is empty).
	if r.URL.ForceQuery || r.URL.RawQuery != "" {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	var in restoreVolumeInput
	if !decodeRequest(r, &in) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if !validID(volumeID) || !validID(in.SnapshotID) || in.ExpectedGeneration == nil || *in.ExpectedGeneration < 0 {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	expected := *in.ExpectedGeneration

	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.volumes[volumeID]
	if !ok {
		writeError(w, http.StatusNotFound, "volume_not_found")
		return
	}
	snap, ok := s.snapshots[in.SnapshotID]
	if !ok {
		writeError(w, http.StatusNotFound, "snapshot_not_found")
		return
	}
	if v.binding != "" {
		writeError(w, http.StatusConflict, "volume_in_use")
		return
	}
	if snap.sourceVolumeID != volumeID || snap.sourceInstanceID != v.instanceID {
		writeError(w, http.StatusConflict, "snapshot_source_mismatch")
		return
	}
	if v.generation != expected {
		writeError(w, http.StatusConflict, "stale_generation")
		return
	}
	p := s.pools[v.poolID]
	delta := snap.sizeBytes - v.sizeBytes
	if delta > 0 {
		// Growth is charged to the volume's tenant quota before pool capacity,
		// matching resize; shrinkage is subject to neither.
		if p.tenantQuotaExceeded(v.tenant, delta) {
			writeError(w, http.StatusConflict, "tenant_quota_exceeded")
			return
		}
		if p.allocated() > p.rawCapacity-delta {
			writeError(w, http.StatusConflict, "insufficient_capacity")
			return
		}
	}
	// delta is positive for growth and negative for shrinkage, so a single
	// signed adjustment covers the pool and the tenant's usage. The snapshot
	// itself is untouched and keeps charging the pool.
	p.volumeBytes += delta
	p.tenantUsed[v.tenant] += delta
	v.sizeBytes = snap.sizeBytes
	v.generation++
	s.appendAudit(auditVolumeRestored, volumePath+"/"+volumeID+"/restore", v.poolID, delta)
	writeJSON(w, http.StatusOK, v.view())
}

func (s *store) deleteBinding(w http.ResponseWriter, r *http.Request, volumeID string) {
	// expectedGeneration is the single accepted query parameter; it must be
	// present exactly once. Parse the raw string manually so malformed or
	// duplicated parameters cannot be silently discarded.
	gen, ok := parseGenerationQuery(r.URL.RawQuery)
	if !ok || !validID(volumeID) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	v, exists := s.volumes[volumeID]
	if !exists {
		writeError(w, http.StatusNotFound, "volume_not_found")
		return
	}
	if v.generation != gen {
		writeError(w, http.StatusConflict, "stale_generation")
		return
	}
	wasBound := v.binding != ""
	v.binding = ""
	if wasBound {
		v.generation++
		// Unbinding an already-unbound volume is a successful no-op: no
		// generation bump and no audit event.
		s.appendAudit(auditVolumeUnbound, volumePath+"/"+volumeID+"/binding", v.poolID, 0)
	}
	w.WriteHeader(http.StatusNoContent)
}

// parseGenerationQuery accepts exactly one "expectedGeneration=<non-neg-int>"
// pair. Missing, empty, duplicated or extra parameters yield ok=false.
func parseGenerationQuery(raw string) (int64, bool) {
	if raw == "" {
		return 0, false
	}
	var gen int64
	found := false
	for _, pair := range strings.Split(raw, "&") {
		key, value, ok := strings.Cut(pair, "=")
		if !ok {
			return 0, false
		}
		decodedKey, err := url.QueryUnescape(key)
		if err != nil {
			return 0, false
		}
		if decodedKey != "expectedGeneration" {
			return 0, false
		}
		if found {
			return 0, false
		}
		decodedValue, err := url.QueryUnescape(value)
		if err != nil {
			return 0, false
		}
		parsed, err := strconv.ParseInt(decodedValue, 10, 64)
		if err != nil || parsed < 0 {
			return 0, false
		}
		gen, found = parsed, true
	}
	return gen, found
}

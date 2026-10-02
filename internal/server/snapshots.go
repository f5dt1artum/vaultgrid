package server

import (
	"net/http"
	"sort"
)

type snapshotInput struct {
	ID                 string `json:"id"`
	ExpectedGeneration *int64 `json:"expectedGeneration"`
}

type cloneInput struct {
	ID     string `json:"id"`
	PoolID string `json:"poolId"`
}

// snapshot is an immutable, in-memory copy of an unbound volume at a given
// generation. It reserves sizeBytes in the source pool independently of the
// source volume's later lifecycle.
type snapshot struct {
	id               string
	sourceVolumeID   string
	poolID           string
	sizeBytes        int64
	sourceGeneration int64
}

// snapshotView is the public representation of a snapshot.
type snapshotView struct {
	ID               string `json:"id"`
	SourceVolumeID   string `json:"sourceVolumeId"`
	PoolID           string `json:"poolId"`
	SizeBytes        int64  `json:"sizeBytes"`
	SourceGeneration int64  `json:"sourceGeneration"`
}

func (s *snapshot) view() snapshotView {
	return snapshotView{
		ID:               s.id,
		SourceVolumeID:   s.sourceVolumeID,
		PoolID:           s.poolID,
		SizeBytes:        s.sizeBytes,
		SourceGeneration: s.sourceGeneration,
	}
}

// routeSnapshots dispatches /v1/snapshots and its sub-paths.
func (s *store) routeSnapshots(w http.ResponseWriter, r *http.Request) {
	parts := splitPath(r.URL.Path, snapshotPath)
	for _, seg := range parts {
		if seg == "" {
			writeError(w, http.StatusNotFound, "not_found")
			return
		}
	}
	switch {
	case len(parts) == 0:
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		s.listSnapshots(w)
	case len(parts) == 1:
		switch r.Method {
		case http.MethodGet:
			s.getSnapshot(w, parts[0])
		case http.MethodDelete:
			s.deleteSnapshot(w, parts[0])
		default:
			methodNotAllowed(w, http.MethodGet, http.MethodDelete)
		}
	case len(parts) == 2 && parts[1] == "clones":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		s.createClone(w, r, parts[0])
	default:
		writeError(w, http.StatusNotFound, "not_found")
	}
}

func (s *store) listSnapshots(w http.ResponseWriter) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids := make([]string, 0, len(s.snapshots))
	for id := range s.snapshots {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	items := make([]snapshotView, 0, len(ids))
	for _, id := range ids {
		items = append(items, s.snapshots[id].view())
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *store) getSnapshot(w http.ResponseWriter, id string) {
	if !validID(id) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	s.mu.RLock()
	snap, ok := s.snapshots[id]
	var view snapshotView
	if ok {
		view = snap.view()
	}
	s.mu.RUnlock()
	if !ok {
		writeError(w, http.StatusNotFound, "snapshot_not_found")
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *store) createSnapshot(w http.ResponseWriter, r *http.Request, volumeID string) {
	var in snapshotInput
	if !decodeRequest(r, &in) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if !validID(volumeID) || !validID(in.ID) || in.ExpectedGeneration == nil || *in.ExpectedGeneration < 0 {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	expected := *in.ExpectedGeneration

	s.mu.Lock()
	defer s.mu.Unlock()
	// A snapshot id is globally unique: resolve same-id retries before any
	// source-volume lookup so a deleted or rebound source cannot turn a valid
	// retry into an error.
	if existing, dup := s.snapshots[in.ID]; dup {
		if existing.sourceVolumeID == volumeID && existing.sourceGeneration == expected {
			writeJSON(w, http.StatusOK, existing.view())
			return
		}
		writeError(w, http.StatusConflict, "snapshot_exists")
		return
	}
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
	// Check and reserve under the store lock so concurrent snapshots and
	// clones can never overcommit the source pool.
	if p.allocated() > p.rawCapacity-v.sizeBytes {
		writeError(w, http.StatusConflict, "insufficient_capacity")
		return
	}
	snap := &snapshot{
		id:               in.ID,
		sourceVolumeID:   volumeID,
		poolID:           v.poolID,
		sizeBytes:        v.sizeBytes,
		sourceGeneration: v.generation,
	}
	s.snapshots[in.ID] = snap
	p.snapshotBytes += snap.sizeBytes
	writeJSON(w, http.StatusCreated, snap.view())
}

func (s *store) deleteSnapshot(w http.ResponseWriter, id string) {
	if !validID(id) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	snap, ok := s.snapshots[id]
	if !ok {
		writeError(w, http.StatusNotFound, "snapshot_not_found")
		return
	}
	// The source pool always outlives its snapshots: deleting it is refused
	// while a snapshot still charges capacity to it.
	s.pools[snap.poolID].snapshotBytes -= snap.sizeBytes
	delete(s.snapshots, id)
	w.WriteHeader(http.StatusNoContent)
}

func (s *store) createClone(w http.ResponseWriter, r *http.Request, snapshotID string) {
	var in cloneInput
	if !decodeRequest(r, &in) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if !validID(snapshotID) || !validID(in.ID) || !validID(in.PoolID) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	// Resolve same-volume-id retries first, matching the create-volume rule:
	// a retry of this exact clone is 200 with no extra charge, while an id
	// minted by a normal create or another clone is 409 volume_exists.
	if existing, dup := s.volumes[in.ID]; dup {
		if snap, fromSnap := s.snapshots[snapshotID]; fromSnap &&
			existing.sourceSnapshotID == snapshotID && existing.poolID == in.PoolID &&
			existing.sizeBytes == snap.sizeBytes {
			writeJSON(w, http.StatusOK, existing.view())
			return
		}
		writeError(w, http.StatusConflict, "volume_exists")
		return
	}
	snap, ok := s.snapshots[snapshotID]
	if !ok {
		writeError(w, http.StatusNotFound, "snapshot_not_found")
		return
	}
	p, ok := s.pools[in.PoolID]
	if !ok {
		writeError(w, http.StatusNotFound, "pool_not_found")
		return
	}
	if p.allocated() > p.rawCapacity-snap.sizeBytes {
		writeError(w, http.StatusConflict, "insufficient_capacity")
		return
	}
	v := &volume{
		id:               in.ID,
		poolID:           in.PoolID,
		sizeBytes:        snap.sizeBytes,
		sourceSnapshotID: snapshotID,
	}
	s.volumes[in.ID] = v
	p.volumeBytes += v.sizeBytes
	writeJSON(w, http.StatusCreated, v.view())
}

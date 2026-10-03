package server

import (
	"net/http"
	"sort"
)

type createSnapshotInput struct {
	ID                 string `json:"id"`
	ExpectedGeneration *int64 `json:"expectedGeneration"`
}

type createCloneInput struct {
	ID     string `json:"id"`
	PoolID string `json:"poolId"`
}

// snapshotView is the public representation of an in-memory volume snapshot.
type snapshotView struct {
	ID               string `json:"id"`
	SourceVolumeID   string `json:"sourceVolumeId"`
	PoolID           string `json:"poolId"`
	SizeBytes        int64  `json:"sizeBytes"`
	SourceGeneration int64  `json:"sourceGeneration"`
}

// snapshot is an immutable, in-memory copy of a volume's size at a given
// generation. It deliberately does not reference the live source volume: once
// taken it is independent, so later changes to or deletion of the source do
// not affect it. Its size charges the source pool until the snapshot is
// deleted. tenant is inherited from the source volume. sourceInstanceID pins
// the snapshot to the exact volume instance that produced it, so a snapshot
// can only be restored into that instance — never into a later volume that
// happens to reuse the same id.
type snapshot struct {
	id               string
	sourceVolumeID   string
	poolID           string
	sizeBytes        int64
	sourceGeneration int64
	tenant           string
	sourceInstanceID int64
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

// createSnapshot handles POST /v1/volumes/{volumeId}/snapshots.
func (s *store) createSnapshot(w http.ResponseWriter, r *http.Request, volumeID string) {
	var in createSnapshotInput
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
	// A retry of the same id wins its idempotency/conflict check before the
	// source is looked up, so deleting the source after a successful snapshot
	// keeps the retry returning 200.
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
	// The store lock makes the quota and capacity checks plus the charge
	// atomic, so concurrent snapshots can never exceed either limit. The
	// snapshot inherits the source volume's tenant.
	if p.tenantQuotaExceeded(v.tenant, v.sizeBytes) {
		writeError(w, http.StatusConflict, "tenant_quota_exceeded")
		return
	}
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
		tenant:           v.tenant,
		sourceInstanceID: v.instanceID,
	}
	s.snapshots[in.ID] = snap
	p.snapshotBytes += v.sizeBytes
	p.tenantUsed[v.tenant] += v.sizeBytes
	s.appendAudit(auditSnapshotCreated, snapshotPath+"/"+in.ID, v.poolID, v.sizeBytes)
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
	s.pools[snap.poolID].snapshotBytes -= snap.sizeBytes
	s.pools[snap.poolID].tenantUsed[snap.tenant] -= snap.sizeBytes
	delete(s.snapshots, id)
	s.appendAudit(auditSnapshotDeleted, snapshotPath+"/"+id, snap.poolID, -snap.sizeBytes)
	w.WriteHeader(http.StatusNoContent)
}

// createClone handles POST /v1/snapshots/{id}/clones.
func (s *store) createClone(w http.ResponseWriter, r *http.Request, snapshotID string) {
	var in createCloneInput
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
	snap, ok := s.snapshots[snapshotID]
	if !ok {
		writeError(w, http.StatusNotFound, "snapshot_not_found")
		return
	}
	// A clone is an ordinary volume, so its id shares the global volume
	// namespace. Once the snapshot is confirmed, the volume-id conflict check
	// decides: a retry from the same snapshot into the same pool is 200, while
	// an id owned by any other creation or a different pool is 409.
	if existing, dup := s.volumes[in.ID]; dup {
		if existing.cloneSource == snapshotID && existing.poolID == in.PoolID {
			writeJSON(w, http.StatusOK, existing.view())
			return
		}
		writeError(w, http.StatusConflict, "volume_exists")
		return
	}
	target, ok := s.pools[in.PoolID]
	if !ok {
		writeError(w, http.StatusNotFound, "pool_not_found")
		return
	}
	// The clone inherits the snapshot's tenant and charges the target pool.
	if target.tenantQuotaExceeded(snap.tenant, snap.sizeBytes) {
		writeError(w, http.StatusConflict, "tenant_quota_exceeded")
		return
	}
	if target.allocated() > target.rawCapacity-snap.sizeBytes {
		writeError(w, http.StatusConflict, "insufficient_capacity")
		return
	}
	v := &volume{id: in.ID, poolID: in.PoolID, sizeBytes: snap.sizeBytes, createdSizeBytes: snap.sizeBytes, cloneSource: snapshotID, tenant: snap.tenant}
	s.volumeInstanceSeq++
	v.instanceID = s.volumeInstanceSeq
	s.volumes[in.ID] = v
	target.volumeBytes += snap.sizeBytes
	target.tenantUsed[snap.tenant] += snap.sizeBytes
	s.appendAudit(auditCloneCreated, volumePath+"/"+in.ID, in.PoolID, snap.sizeBytes)
	writeJSON(w, http.StatusCreated, v.view())
}

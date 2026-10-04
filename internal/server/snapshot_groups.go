package server

import (
	"net/http"
	"sort"
)

// snapshotGroupMemberInput is one member of a create-group request.
type snapshotGroupMemberInput struct {
	VolumeID           string `json:"volumeId"`
	SnapshotID         string `json:"snapshotId"`
	ExpectedGeneration *int64 `json:"expectedGeneration"`
}

type createSnapshotGroupInput struct {
	ID      string                     `json:"id"`
	Members []snapshotGroupMemberInput `json:"members"`
}

// snapshotGroupView is the public representation of a snapshot group: its id,
// the common pool and the member snapshots ordered by snapshot id.
type snapshotGroupView struct {
	ID        string         `json:"id"`
	PoolID    string         `json:"poolId"`
	Snapshots []snapshotView `json:"snapshots"`
}

// snapshotGroupMember records one committed member so the group can be
// compared against retries and torn down without consulting the live volumes.
type snapshotGroupMember struct {
	volumeID   string
	snapshotID string
	generation int64
	sizeBytes  int64
	tenant     string
}

// snapshotGroup is an atomic set of snapshots taken from same-pool volumes.
// Its members keep charging the common pool until the whole group is deleted;
// individual member snapshots cannot be deleted directly.
type snapshotGroup struct {
	id         string
	poolID     string
	members    []snapshotGroupMember
	totalBytes int64
}

// view renders the group with its member snapshots sorted by snapshot id. The
// caller must hold s.mu.
func (g *snapshotGroup) view(s *store) snapshotGroupView {
	ids := make([]string, 0, len(g.members))
	for _, m := range g.members {
		ids = append(ids, m.snapshotID)
	}
	sort.Strings(ids)
	snaps := make([]snapshotView, 0, len(ids))
	for _, id := range ids {
		snaps = append(snaps, s.snapshots[id].view())
	}
	return snapshotGroupView{ID: g.id, PoolID: g.poolID, Snapshots: snaps}
}

// matchesRequest reports whether members name the same volume→snapshot
// mapping at the same generations as the group, regardless of order.
func (g *snapshotGroup) matchesRequest(members []snapshotGroupMemberInput) bool {
	if len(g.members) != len(members) {
		return false
	}
	type snapshotPin struct {
		snapshotID string
		generation int64
	}
	committed := make(map[string]snapshotPin, len(g.members))
	for _, m := range g.members {
		committed[m.volumeID] = snapshotPin{snapshotID: m.snapshotID, generation: m.generation}
	}
	for _, m := range members {
		pin, ok := committed[m.VolumeID]
		if !ok || pin.snapshotID != m.SnapshotID || pin.generation != *m.ExpectedGeneration {
			return false
		}
	}
	return true
}

// routeSnapshotGroups dispatches /v1/snapshot-groups and its sub-paths.
func (s *store) routeSnapshotGroups(w http.ResponseWriter, r *http.Request) {
	parts := splitPath(r.URL.Path, snapshotGroupPath)
	for _, seg := range parts {
		if seg == "" {
			writeError(w, http.StatusNotFound, "not_found")
			return
		}
	}
	switch {
	case len(parts) == 0:
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		s.createSnapshotGroup(w, r)
	case len(parts) == 1:
		if r.Method != http.MethodDelete {
			methodNotAllowed(w, http.MethodDelete)
			return
		}
		s.deleteSnapshotGroup(w, parts[0])
	default:
		writeError(w, http.StatusNotFound, "not_found")
	}
}

// createSnapshotGroup handles POST /v1/snapshot-groups. It validates the whole
// request, then checks and commits the entire group inside one store-lock
// critical section, so the group is equivalent to some serial order against
// every other volume and snapshot write, and a failure leaves no group,
// snapshot, metering or audit change behind.
func (s *store) createSnapshotGroup(w http.ResponseWriter, r *http.Request) {
	// The collection accepts no query string at all (ForceQuery catches a
	// bare trailing "?" whose RawQuery is empty).
	if r.URL.ForceQuery || r.URL.RawQuery != "" {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	var in createSnapshotGroupInput
	if !decodeRequest(r, &in) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if !validID(in.ID) || len(in.Members) < 2 || len(in.Members) > 64 {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	seenVolumes := make(map[string]bool, len(in.Members))
	seenSnapshots := make(map[string]bool, len(in.Members))
	for _, m := range in.Members {
		if !validID(m.VolumeID) || !validID(m.SnapshotID) || m.ExpectedGeneration == nil || *m.ExpectedGeneration < 0 {
			writeError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		if seenVolumes[m.VolumeID] || seenSnapshots[m.SnapshotID] {
			writeError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		seenVolumes[m.VolumeID] = true
		seenSnapshots[m.SnapshotID] = true
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	// A retry of the same group id wins its idempotency/conflict check before
	// any member is looked up, so deleting a source volume after a successful
	// create keeps the faithful retry returning 200.
	if existing, dup := s.snapshotGroups[in.ID]; dup {
		if existing.matchesRequest(in.Members) {
			writeJSON(w, http.StatusOK, existing.view(s))
			return
		}
		writeError(w, http.StatusConflict, "snapshot_group_exists")
		return
	}
	// Per-member checks run in member order: existence, then binding, then
	// generation.
	vols := make([]*volume, len(in.Members))
	for i, m := range in.Members {
		v, ok := s.volumes[m.VolumeID]
		if !ok {
			writeError(w, http.StatusNotFound, "volume_not_found")
			return
		}
		if v.binding != "" {
			writeError(w, http.StatusConflict, "volume_in_use")
			return
		}
		if v.generation != *m.ExpectedGeneration {
			writeError(w, http.StatusConflict, "stale_generation")
			return
		}
		vols[i] = v
	}
	poolID := vols[0].poolID
	for _, v := range vols[1:] {
		if v.poolID != poolID {
			writeError(w, http.StatusConflict, "cross_pool_snapshot_group")
			return
		}
	}
	for _, m := range in.Members {
		if _, exists := s.snapshots[m.SnapshotID]; exists {
			writeError(w, http.StatusConflict, "snapshot_exists")
			return
		}
	}
	p := s.pools[poolID]
	// Member sizes are aggregated per tenant and checked against the tenant
	// quota before the pool capacity. Member volumes are distinct and all live
	// in this pool, so their sizes already fit in the pool's budget and the
	// sums cannot overflow.
	tenantDelta := make(map[string]int64, len(vols))
	var total int64
	for _, v := range vols {
		tenantDelta[v.tenant] += v.sizeBytes
		total += v.sizeBytes
	}
	for tenant, delta := range tenantDelta {
		if p.tenantQuotaExceeded(tenant, delta) {
			writeError(w, http.StatusConflict, "tenant_quota_exceeded")
			return
		}
	}
	if p.allocated() > p.rawCapacity-total {
		writeError(w, http.StatusConflict, "insufficient_capacity")
		return
	}

	members := make([]snapshotGroupMember, len(in.Members))
	for i, m := range in.Members {
		v := vols[i]
		s.snapshots[m.SnapshotID] = &snapshot{
			id:               m.SnapshotID,
			sourceVolumeID:   m.VolumeID,
			poolID:           poolID,
			sizeBytes:        v.sizeBytes,
			sourceGeneration: v.generation,
			tenant:           v.tenant,
			sourceInstanceID: v.instanceID,
			groupID:          in.ID,
		}
		p.snapshotBytes += v.sizeBytes
		p.tenantUsed[v.tenant] += v.sizeBytes
		members[i] = snapshotGroupMember{
			volumeID:   m.VolumeID,
			snapshotID: m.SnapshotID,
			generation: v.generation,
			sizeBytes:  v.sizeBytes,
			tenant:     v.tenant,
		}
	}
	g := &snapshotGroup{id: in.ID, poolID: poolID, members: members, totalBytes: total}
	s.snapshotGroups[in.ID] = g
	s.appendAudit(auditSnapshotGroupCreated, snapshotGroupPath+"/"+in.ID, poolID, total)
	writeJSON(w, http.StatusCreated, g.view(s))
}

// deleteSnapshotGroup handles DELETE /v1/snapshot-groups/{id}. It atomically
// deletes every member snapshot and releases the pool capacity and tenant
// usage; clones already made from member snapshots are ordinary volumes and
// are unaffected.
func (s *store) deleteSnapshotGroup(w http.ResponseWriter, id string) {
	if !validID(id) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.snapshotGroups[id]
	if !ok {
		writeError(w, http.StatusNotFound, "snapshot_group_not_found")
		return
	}
	p := s.pools[g.poolID]
	for _, m := range g.members {
		delete(s.snapshots, m.snapshotID)
		p.snapshotBytes -= m.sizeBytes
		p.tenantUsed[m.tenant] -= m.sizeBytes
	}
	delete(s.snapshotGroups, id)
	s.appendAudit(auditSnapshotGroupDeleted, snapshotGroupPath+"/"+id, g.poolID, -g.totalBytes)
	w.WriteHeader(http.StatusNoContent)
}

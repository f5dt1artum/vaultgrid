package server

import (
	"net/http"
	"sort"
)

// snapshotGroupMemberInput is one member of a create-snapshot-group request.
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

// snapshotGroupMember records one member mapping as committed: which snapshot
// id was taken from which volume at which generation. It is the idempotency
// fingerprint of the group.
type snapshotGroupMember struct {
	volumeID         string
	snapshotID       string
	sourceGeneration int64
}

// snapshotGroup is an atomic set of same-pool snapshots created together.
// Every member snapshot lives in the store-wide snapshots map and carries this
// group's id, so it can be queried, restored and cloned like any snapshot but
// cannot be deleted individually. totalBytes is the sum of member sizes,
// charged to the common pool while the group exists.
type snapshotGroup struct {
	id         string
	poolID     string
	members    []snapshotGroupMember
	totalBytes int64
}

// matches reports whether members is the same volume→snapshot mapping with
// the same generations the group was committed with. Comparison is
// order-independent: the mapping, not the request's member order, defines
// identity.
func (g *snapshotGroup) matches(members []snapshotGroupMemberInput) bool {
	if len(g.members) != len(members) {
		return false
	}
	byVolume := make(map[string]snapshotGroupMember, len(g.members))
	for _, m := range g.members {
		byVolume[m.volumeID] = m
	}
	for _, in := range members {
		m, ok := byVolume[in.VolumeID]
		if !ok || m.snapshotID != in.SnapshotID || m.sourceGeneration != *in.ExpectedGeneration {
			return false
		}
	}
	return true
}

// view renders the group with its member snapshots sorted by snapshot id.
// The caller must hold s.mu.
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
		s.deleteSnapshotGroup(w, r, parts[0])
	default:
		writeError(w, http.StatusNotFound, "not_found")
	}
}

// createSnapshotGroup handles POST /v1/snapshot-groups. It validates the whole
// request, then checks every member (volume exists, unbound, generation
// matches, in request order), the common pool, snapshot id collisions and the
// aggregate quota and capacity before committing anything, so a failure never
// leaves a partial group, snapshot, metering or audit change behind. The
// entire check-and-commit runs inside one store-lock critical section, making
// the group commit equivalent to some serial order against any other volume
// or snapshot write.
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
		if !validID(m.VolumeID) || !validID(m.SnapshotID) || m.ExpectedGeneration == nil || *m.ExpectedGeneration < 0 ||
			seenVolumes[m.VolumeID] || seenSnapshots[m.SnapshotID] {
			writeError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		seenVolumes[m.VolumeID] = true
		seenSnapshots[m.SnapshotID] = true
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	// A retry of the same group id wins its idempotency/conflict check before
	// any member is looked up, so deleting the source volumes after a
	// successful group keeps the retry returning 200.
	if existing, dup := s.snapshotGroups[in.ID]; dup {
		if existing.matches(in.Members) {
			writeJSON(w, http.StatusOK, existing.view(s))
			return
		}
		writeError(w, http.StatusConflict, "snapshot_group_exists")
		return
	}
	// Per-member checks in request order: existence, binding, generation.
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
	// Every member must live in one common pool.
	poolID := vols[0].poolID
	for _, v := range vols[1:] {
		if v.poolID != poolID {
			writeError(w, http.StatusConflict, "cross_pool_snapshot_group")
			return
		}
	}
	// None of the member snapshot ids may already exist, whether standalone
	// or owned by another group.
	for _, m := range in.Members {
		if _, dup := s.snapshots[m.SnapshotID]; dup {
			writeError(w, http.StatusConflict, "snapshot_exists")
			return
		}
	}
	// Member sizes are aggregated per tenant; the quota check for the whole
	// group precedes the pool capacity check, matching single-snapshot order.
	p := s.pools[poolID]
	perTenant := make(map[string]int64)
	var totalBytes int64
	for _, v := range vols {
		perTenant[v.tenant] += v.sizeBytes
		totalBytes += v.sizeBytes
	}
	for tenant, delta := range perTenant {
		if p.tenantQuotaExceeded(tenant, delta) {
			writeError(w, http.StatusConflict, "tenant_quota_exceeded")
			return
		}
	}
	if p.allocated() > p.rawCapacity-totalBytes {
		writeError(w, http.StatusConflict, "insufficient_capacity")
		return
	}

	group := &snapshotGroup{id: in.ID, poolID: poolID, totalBytes: totalBytes}
	for i, m := range in.Members {
		v := vols[i]
		snap := &snapshot{
			id:               m.SnapshotID,
			sourceVolumeID:   m.VolumeID,
			poolID:           poolID,
			sizeBytes:        v.sizeBytes,
			sourceGeneration: v.generation,
			tenant:           v.tenant,
			sourceInstanceID: v.instanceID,
			groupID:          in.ID,
		}
		s.snapshots[m.SnapshotID] = snap
		p.snapshotBytes += v.sizeBytes
		p.tenantUsed[v.tenant] += v.sizeBytes
		group.members = append(group.members, snapshotGroupMember{
			volumeID:         m.VolumeID,
			snapshotID:       m.SnapshotID,
			sourceGeneration: v.generation,
		})
	}
	s.snapshotGroups[in.ID] = group
	s.appendAudit(auditSnapshotGroupCreated, snapshotGroupPath+"/"+in.ID, poolID, totalBytes)
	writeJSON(w, http.StatusCreated, group.view(s))
}

// deleteSnapshotGroup handles DELETE /v1/snapshot-groups/{id}. It atomically
// removes every member snapshot and the group itself, returning the group's
// total bytes to the common pool and each member's tenant. Clones already
// derived from member snapshots are ordinary volumes and are unaffected.
func (s *store) deleteSnapshotGroup(w http.ResponseWriter, r *http.Request, id string) {
	if r.URL.ForceQuery || r.URL.RawQuery != "" {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if !validID(id) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	group, ok := s.snapshotGroups[id]
	if !ok {
		writeError(w, http.StatusNotFound, "snapshot_group_not_found")
		return
	}
	p := s.pools[group.poolID]
	for _, m := range group.members {
		snap := s.snapshots[m.snapshotID]
		p.snapshotBytes -= snap.sizeBytes
		p.tenantUsed[snap.tenant] -= snap.sizeBytes
		delete(s.snapshots, m.snapshotID)
	}
	delete(s.snapshotGroups, id)
	s.appendAudit(auditSnapshotGroupDeleted, snapshotGroupPath+"/"+id, group.poolID, -group.totalBytes)
	w.WriteHeader(http.StatusNoContent)
}

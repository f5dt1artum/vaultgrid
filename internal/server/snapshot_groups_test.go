package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

const groupPool = `{"id":"gp","devices":[{"id":"gdev","capacityBytes":1000,"faultDomain":"rack1"}]}`

func createGroup(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := do(h, http.MethodPost, "/v1/snapshot-groups", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create snapshot group status = %d body = %s", rec.Code, rec.Body.String())
	}
	return rec
}

// twoVolGroup sets up a 1000-byte pool with vol1 (100 bytes) and vol2 (200
// bytes) and returns the body of a two-member group request over them.
func twoVolGroup(t *testing.T, h http.Handler) string {
	t.Helper()
	createPool(t, h, groupPool)
	createVol(t, h, `{"id":"vol1","poolId":"gp","sizeBytes":100}`)
	createVol(t, h, `{"id":"vol2","poolId":"gp","sizeBytes":200}`)
	return `{"id":"grp1","members":[` +
		`{"volumeId":"vol1","snapshotId":"snap-b","expectedGeneration":0},` +
		`{"volumeId":"vol2","snapshotId":"snap-a","expectedGeneration":0}]}`
}

func TestCreateSnapshotGroupRepresentationAndCharging(t *testing.T) {
	h := Handler()
	body := twoVolGroup(t, h)

	rec := createGroup(t, h, body)
	g := decodeBody(t, rec)
	if g["id"] != "grp1" || g["poolId"] != "gp" {
		t.Fatalf("group identity fields = %v", g)
	}
	// Members are returned as snapshot items sorted by snapshotId, not by
	// request order.
	snaps := g["snapshots"].([]any)
	if len(snaps) != 2 {
		t.Fatalf("group snapshots = %v", snaps)
	}
	first := snaps[0].(map[string]any)
	second := snaps[1].(map[string]any)
	if first["id"] != "snap-a" || second["id"] != "snap-b" {
		t.Fatalf("snapshots not sorted by snapshotId: %v", snaps)
	}
	if first["sourceVolumeId"] != "vol2" || first["sizeBytes"].(float64) != 200 ||
		first["poolId"] != "gp" || first["sourceGeneration"].(float64) != 0 {
		t.Fatalf("member snapshot view = %v", first)
	}

	// The whole group charges the common pool on top of the source volumes:
	// 300 volume bytes + 300 snapshot bytes.
	p := getPoolView(t, h, "gp")
	if p["allocatedBytes"].(float64) != 600 || p["availableBytes"].(float64) != 400 {
		t.Fatalf("pool after group = %v", p)
	}

	// Member snapshots appear in the existing snapshot listing and lookup.
	items := decodeBody(t, do(h, http.MethodGet, "/v1/snapshots", ""))["items"].([]any)
	if len(items) != 2 || items[0].(map[string]any)["id"] != "snap-a" || items[1].(map[string]any)["id"] != "snap-b" {
		t.Fatalf("snapshot list = %v", items)
	}
	s := decodeBody(t, do(h, http.MethodGet, "/v1/snapshots/snap-b", ""))
	if s["sourceVolumeId"] != "vol1" || s["sizeBytes"].(float64) != 100 {
		t.Fatalf("get member snapshot = %v", s)
	}

	// One snapshot-group.created event with the group's total bytes.
	events := getAuditEvents(t, h, "/v1/audit-events?poolId=gp")["items"].([]any)
	last := events[len(events)-1].(map[string]any)
	if last["action"] != "snapshot-group.created" || last["resource"] != "/v1/snapshot-groups/grp1" ||
		last["poolId"] != "gp" || last["bytesDelta"].(float64) != 300 {
		t.Fatalf("group created event = %v", last)
	}
}

func TestSnapshotGroupIdempotency(t *testing.T) {
	h := Handler()
	body := twoVolGroup(t, h)
	createGroup(t, h, body)

	// Identical retry: 200, same representation, no double metering or audit.
	rec := do(h, http.MethodPost, "/v1/snapshot-groups", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("retry status = %d %s", rec.Code, rec.Body.String())
	}
	if g := decodeBody(t, rec); g["id"] != "grp1" || len(g["snapshots"].([]any)) != 2 {
		t.Fatalf("retry view = %v", g)
	}
	// Member order does not define identity: the same mapping in another
	// order is still a faithful retry.
	reordered := `{"id":"grp1","members":[` +
		`{"volumeId":"vol2","snapshotId":"snap-a","expectedGeneration":0},` +
		`{"volumeId":"vol1","snapshotId":"snap-b","expectedGeneration":0}]}`
	if rec := do(h, http.MethodPost, "/v1/snapshot-groups", reordered); rec.Code != http.StatusOK {
		t.Fatalf("reordered retry = %d %s", rec.Code, rec.Body.String())
	}
	if p := getPoolView(t, h, "gp"); p["allocatedBytes"].(float64) != 600 {
		t.Fatalf("allocated after retries = %v", p["allocatedBytes"])
	}
	events := getAuditEvents(t, h, "/v1/audit-events")["items"].([]any)
	if len(events) != 4 { // pool, vol1, vol2, group
		t.Fatalf("audit events after retries = %v", events)
	}

	// Same group id with any difference in the mapping or generations is a
	// conflict.
	for name, conflict := range map[string]string{
		"different snapshot id": `{"id":"grp1","members":[` +
			`{"volumeId":"vol1","snapshotId":"snap-b","expectedGeneration":0},` +
			`{"volumeId":"vol2","snapshotId":"snap-c","expectedGeneration":0}]}`,
		"different generation": `{"id":"grp1","members":[` +
			`{"volumeId":"vol1","snapshotId":"snap-b","expectedGeneration":0},` +
			`{"volumeId":"vol2","snapshotId":"snap-a","expectedGeneration":1}]}`,
		"swapped mapping": `{"id":"grp1","members":[` +
			`{"volumeId":"vol1","snapshotId":"snap-a","expectedGeneration":0},` +
			`{"volumeId":"vol2","snapshotId":"snap-b","expectedGeneration":0}]}`,
		"extra member": `{"id":"grp1","members":[` +
			`{"volumeId":"vol1","snapshotId":"snap-b","expectedGeneration":0},` +
			`{"volumeId":"vol2","snapshotId":"snap-a","expectedGeneration":0},` +
			`{"volumeId":"vol3","snapshotId":"snap-d","expectedGeneration":0}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			wantError(t, do(h, http.MethodPost, "/v1/snapshot-groups", conflict),
				http.StatusConflict, "snapshot_group_exists")
		})
	}
}

func TestSnapshotGroupMemberChecks(t *testing.T) {
	h := Handler()
	createPool(t, h, groupPool)
	createVol(t, h, `{"id":"vol1","poolId":"gp","sizeBytes":100}`)
	createVol(t, h, `{"id":"vol2","poolId":"gp","sizeBytes":100}`)

	// A missing volume is reported for the first offending member.
	wantError(t, do(h, http.MethodPost, "/v1/snapshot-groups",
		`{"id":"g1","members":[{"volumeId":"vol1","snapshotId":"s1","expectedGeneration":0},{"volumeId":"nope","snapshotId":"s2","expectedGeneration":0}]}`),
		http.StatusNotFound, "volume_not_found")

	// A bound member is rejected; binding state takes precedence over the
	// generation comparison for that member.
	if rec := do(h, http.MethodPut, "/v1/volumes/vol1/binding", `{"nodeId":"n1","expectedGeneration":0}`); rec.Code != http.StatusOK {
		t.Fatalf("bind = %d", rec.Code)
	}
	wantError(t, do(h, http.MethodPost, "/v1/snapshot-groups",
		`{"id":"g1","members":[{"volumeId":"vol1","snapshotId":"s1","expectedGeneration":0},{"volumeId":"vol2","snapshotId":"s2","expectedGeneration":0}]}`),
		http.StatusConflict, "volume_in_use")
	if rec := do(h, http.MethodDelete, "/v1/volumes/vol1/binding?expectedGeneration=1", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("unbind = %d", rec.Code)
	}

	// vol1 is now at generation 2; a stale member generation is rejected.
	wantError(t, do(h, http.MethodPost, "/v1/snapshot-groups",
		`{"id":"g1","members":[{"volumeId":"vol1","snapshotId":"s1","expectedGeneration":0},{"volumeId":"vol2","snapshotId":"s2","expectedGeneration":0}]}`),
		http.StatusConflict, "stale_generation")

	// Members from different pools cannot form a group.
	createPool(t, h, `{"id":"other","devices":[{"id":"odev","capacityBytes":1000,"faultDomain":"rack2"}]}`)
	createVol(t, h, `{"id":"vol3","poolId":"other","sizeBytes":100}`)
	wantError(t, do(h, http.MethodPost, "/v1/snapshot-groups",
		`{"id":"g1","members":[{"volumeId":"vol1","snapshotId":"s1","expectedGeneration":2},{"volumeId":"vol3","snapshotId":"s3","expectedGeneration":0}]}`),
		http.StatusConflict, "cross_pool_snapshot_group")

	// A member snapshot id that already exists is rejected, whether the
	// snapshot is standalone or owned by another group.
	createSnapshot(t, h, "vol2", `{"id":"solo","expectedGeneration":0}`)
	wantError(t, do(h, http.MethodPost, "/v1/snapshot-groups",
		`{"id":"g1","members":[{"volumeId":"vol1","snapshotId":"solo","expectedGeneration":2},{"volumeId":"vol2","snapshotId":"s2","expectedGeneration":0}]}`),
		http.StatusConflict, "snapshot_exists")
	createGroup(t, h, `{"id":"g2","members":[`+
		`{"volumeId":"vol1","snapshotId":"owned","expectedGeneration":2},`+
		`{"volumeId":"vol2","snapshotId":"s2","expectedGeneration":0}]}`)
	wantError(t, do(h, http.MethodPost, "/v1/snapshot-groups",
		`{"id":"g3","members":[{"volumeId":"vol1","snapshotId":"owned","expectedGeneration":2},{"volumeId":"vol2","snapshotId":"s9","expectedGeneration":0}]}`),
		http.StatusConflict, "snapshot_exists")

	// None of the failures left a group, snapshot, metering or audit change
	// behind: gp holds vol1+vol2 (200) + solo (100) + g2 (200) = 500.
	if p := getPoolView(t, h, "gp"); p["allocatedBytes"].(float64) != 500 {
		t.Fatalf("allocated after failures = %v", p["allocatedBytes"])
	}
	events := getAuditEvents(t, h, "/v1/audit-events")["items"].([]any)
	for _, raw := range events {
		e := raw.(map[string]any)
		if e["action"] == "snapshot-group.created" && e["resource"] != "/v1/snapshot-groups/g2" {
			t.Fatalf("unexpected group event = %v", e)
		}
	}
}

func TestSnapshotGroupQuotaAndCapacity(t *testing.T) {
	h := Handler()
	createPool(t, h, groupPool)
	createVol(t, h, `{"id":"vol1","poolId":"gp","sizeBytes":100}`)
	createVol(t, h, `{"id":"vol2","poolId":"gp","sizeBytes":100}`)

	// Member sizes aggregate per tenant: two 100-byte members of the default
	// tenant need 200 bytes of quota headroom, so a quota of 390 (used 200)
	// rejects the group even though each member alone would fit.
	mustQuota(t, h, "gp", "default", 390)
	wantError(t, do(h, http.MethodPost, "/v1/snapshot-groups",
		`{"id":"g1","members":[{"volumeId":"vol1","snapshotId":"s1","expectedGeneration":0},{"volumeId":"vol2","snapshotId":"s2","expectedGeneration":0}]}`),
		http.StatusConflict, "tenant_quota_exceeded")
	if rec := putQuota(t, h, "gp", "default", 400); rec.Code != http.StatusOK {
		t.Fatalf("raise quota = %d", rec.Code)
	}
	rec := do(h, http.MethodPost, "/v1/snapshot-groups",
		`{"id":"g1","members":[{"volumeId":"vol1","snapshotId":"s1","expectedGeneration":0},{"volumeId":"vol2","snapshotId":"s2","expectedGeneration":0}]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("group at quota limit = %d %s", rec.Code, rec.Body.String())
	}

	// Capacity: 200 volumes + 200 group snapshots = 400 of 1000 used. A
	// second group needs 200 more and fits; once vol3 takes the pool to 999
	// a third group can only fail on pool capacity, so the quota is lifted
	// first to keep it out of the way.
	if rec := putQuota(t, h, "gp", "default", 1000); rec.Code != http.StatusOK {
		t.Fatalf("raise quota = %d", rec.Code)
	}
	createGroup(t, h, `{"id":"g2","members":[`+
		`{"volumeId":"vol1","snapshotId":"s3","expectedGeneration":0},`+
		`{"volumeId":"vol2","snapshotId":"s4","expectedGeneration":0}]}`)
	createVol(t, h, `{"id":"vol3","poolId":"gp","sizeBytes":399}`)
	if rec := do(h, http.MethodDelete, "/v1/storage-pools/gp/tenant-quotas/default", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete quota = %d", rec.Code)
	}
	wantError(t, do(h, http.MethodPost, "/v1/snapshot-groups",
		`{"id":"g3","members":[{"volumeId":"vol1","snapshotId":"s5","expectedGeneration":0},{"volumeId":"vol2","snapshotId":"s6","expectedGeneration":0}]}`),
		http.StatusConflict, "insufficient_capacity")
	if p := getPoolView(t, h, "gp"); p["allocatedBytes"].(float64) != 999 {
		t.Fatalf("allocated after capacity failure = %v", p["allocatedBytes"])
	}
}

func TestSnapshotGroupInvalidRequests(t *testing.T) {
	h := Handler()
	createPool(t, h, groupPool)
	createVol(t, h, `{"id":"vol1","poolId":"gp","sizeBytes":10}`)
	createVol(t, h, `{"id":"vol2","poolId":"gp","sizeBytes":10}`)

	cases := map[string]string{
		"unknown field":        `{"id":"g","members":[],"x":1}`,
		"unknown member field": `{"id":"g","members":[{"volumeId":"vol1","snapshotId":"s1","expectedGeneration":0,"x":1},{"volumeId":"vol2","snapshotId":"s2","expectedGeneration":0}]}`,
		"missing id":           `{"members":[{"volumeId":"vol1","snapshotId":"s1","expectedGeneration":0},{"volumeId":"vol2","snapshotId":"s2","expectedGeneration":0}]}`,
		"bad id":               `{"id":"bad id","members":[{"volumeId":"vol1","snapshotId":"s1","expectedGeneration":0},{"volumeId":"vol2","snapshotId":"s2","expectedGeneration":0}]}`,
		"missing members":      `{"id":"g"}`,
		"null members":         `{"id":"g","members":null}`,
		"one member":           `{"id":"g","members":[{"volumeId":"vol1","snapshotId":"s1","expectedGeneration":0}]}`,
		"duplicate volume":     `{"id":"g","members":[{"volumeId":"vol1","snapshotId":"s1","expectedGeneration":0},{"volumeId":"vol1","snapshotId":"s2","expectedGeneration":0}]}`,
		"duplicate snapshot":   `{"id":"g","members":[{"volumeId":"vol1","snapshotId":"s1","expectedGeneration":0},{"volumeId":"vol2","snapshotId":"s1","expectedGeneration":0}]}`,
		"bad volume id":        `{"id":"g","members":[{"volumeId":"bad id","snapshotId":"s1","expectedGeneration":0},{"volumeId":"vol2","snapshotId":"s2","expectedGeneration":0}]}`,
		"bad snapshot id":      `{"id":"g","members":[{"volumeId":"vol1","snapshotId":"","expectedGeneration":0},{"volumeId":"vol2","snapshotId":"s2","expectedGeneration":0}]}`,
		"missing generation":   `{"id":"g","members":[{"volumeId":"vol1","snapshotId":"s1"},{"volumeId":"vol2","snapshotId":"s2","expectedGeneration":0}]}`,
		"negative generation":  `{"id":"g","members":[{"volumeId":"vol1","snapshotId":"s1","expectedGeneration":-1},{"volumeId":"vol2","snapshotId":"s2","expectedGeneration":0}]}`,
		"float generation":     `{"id":"g","members":[{"volumeId":"vol1","snapshotId":"s1","expectedGeneration":0.5},{"volumeId":"vol2","snapshotId":"s2","expectedGeneration":0}]}`,
		"malformed json":       `{`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			wantError(t, do(h, http.MethodPost, "/v1/snapshot-groups", body),
				http.StatusBadRequest, "invalid_request")
		})
	}

	// More than 64 members is invalid even though every id is unique.
	tooMany := `{"id":"g","members":[`
	for i := 0; i < 65; i++ {
		if i > 0 {
			tooMany += ","
		}
		tooMany += fmt.Sprintf(`{"volumeId":"v%d","snapshotId":"s%d","expectedGeneration":0}`, i, i)
	}
	tooMany += `]}`
	wantError(t, do(h, http.MethodPost, "/v1/snapshot-groups", tooMany),
		http.StatusBadRequest, "invalid_request")

	// Any query parameter is invalid on both the collection and the item.
	wantError(t, do(h, http.MethodPost, "/v1/snapshot-groups?x=1",
		`{"id":"g","members":[{"volumeId":"vol1","snapshotId":"s1","expectedGeneration":0},{"volumeId":"vol2","snapshotId":"s2","expectedGeneration":0}]}`),
		http.StatusBadRequest, "invalid_request")
	wantError(t, do(h, http.MethodDelete, "/v1/snapshot-groups/g?x=1", ""),
		http.StatusBadRequest, "invalid_request")
	wantError(t, do(h, http.MethodDelete, "/v1/snapshot-groups/bad%20id", ""),
		http.StatusBadRequest, "invalid_request")

	// No partial state: no group, no snapshot, no charge.
	if rec := do(h, http.MethodGet, "/v1/snapshots", ""); rec.Body.String() != "{\"items\":[]}\n" {
		t.Fatalf("partial snapshots: %s", rec.Body.String())
	}
	if p := getPoolView(t, h, "gp"); p["allocatedBytes"].(float64) != 20 {
		t.Fatalf("partial allocation: %v", p["allocatedBytes"])
	}
}

func TestSnapshotGroupMembersUsableLikeSnapshots(t *testing.T) {
	h := Handler()
	body := twoVolGroup(t, h)
	createGroup(t, h, body)

	// A member snapshot restores its source volume.
	if rec := do(h, http.MethodPut, "/v1/volumes/vol1/size", `{"sizeBytes":150,"expectedGeneration":0}`); rec.Code != http.StatusOK {
		t.Fatalf("resize = %d", rec.Code)
	}
	rec := do(h, http.MethodPost, "/v1/volumes/vol1/restore", `{"snapshotId":"snap-b","expectedGeneration":1}`)
	if rec.Code != http.StatusOK || decodeBody(t, rec)["sizeBytes"].(float64) != 100 {
		t.Fatalf("restore from group snapshot = %d %s", rec.Code, rec.Body.String())
	}

	// A member snapshot clones like any snapshot.
	rec = clone(t, h, "snap-a", `{"id":"vol-copy","poolId":"gp"}`)
	if rec.Code != http.StatusCreated || decodeBody(t, rec)["sizeBytes"].(float64) != 200 {
		t.Fatalf("clone from group snapshot = %d %s", rec.Code, rec.Body.String())
	}

	// But a member cannot be deleted individually.
	wantError(t, do(h, http.MethodDelete, "/v1/snapshots/snap-a", ""),
		http.StatusConflict, "snapshot_in_group")
	wantError(t, do(h, http.MethodDelete, "/v1/snapshots/snap-b", ""),
		http.StatusConflict, "snapshot_in_group")
	if rec := do(h, http.MethodGet, "/v1/snapshots/snap-a", ""); rec.Code != http.StatusOK {
		t.Fatalf("member gone after blocked delete = %d", rec.Code)
	}
}

func TestDeleteSnapshotGroup(t *testing.T) {
	h := Handler()
	body := twoVolGroup(t, h)
	createGroup(t, h, body)

	// A clone derived before the group deletion is unaffected by it.
	if rec := clone(t, h, "snap-a", `{"id":"vol-copy","poolId":"gp"}`); rec.Code != http.StatusCreated {
		t.Fatalf("clone = %d %s", rec.Code, rec.Body.String())
	}

	// Missing and already-deleted groups.
	wantError(t, do(h, http.MethodDelete, "/v1/snapshot-groups/nope", ""),
		http.StatusNotFound, "snapshot_group_not_found")

	// Deleting the group removes every member snapshot and returns the
	// group's bytes to the pool: 300 volumes + 200 clone remain of the 800.
	rec := do(h, http.MethodDelete, "/v1/snapshot-groups/grp1", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete group = %d %s", rec.Code, rec.Body.String())
	}
	wantError(t, do(h, http.MethodGet, "/v1/snapshots/snap-a", ""), http.StatusNotFound, "snapshot_not_found")
	wantError(t, do(h, http.MethodGet, "/v1/snapshots/snap-b", ""), http.StatusNotFound, "snapshot_not_found")
	if p := getPoolView(t, h, "gp"); p["allocatedBytes"].(float64) != 500 {
		t.Fatalf("pool after group delete = %v", p["allocatedBytes"])
	}
	if rec := do(h, http.MethodGet, "/v1/volumes/vol-copy", ""); rec.Code != http.StatusOK {
		t.Fatalf("clone affected by group delete = %d", rec.Code)
	}
	wantError(t, do(h, http.MethodDelete, "/v1/snapshot-groups/grp1", ""),
		http.StatusNotFound, "snapshot_group_not_found")

	// One snapshot-group.deleted event with the negated group total.
	events := getAuditEvents(t, h, "/v1/audit-events?poolId=gp")["items"].([]any)
	last := events[len(events)-1].(map[string]any)
	if last["action"] != "snapshot-group.deleted" || last["resource"] != "/v1/snapshot-groups/grp1" ||
		last["poolId"] != "gp" || last["bytesDelta"].(float64) != -300 {
		t.Fatalf("group deleted event = %v", last)
	}

	// The group id can be reused after deletion as a fresh creation.
	if rec := do(h, http.MethodPost, "/v1/snapshot-groups", body); rec.Code != http.StatusCreated {
		t.Fatalf("recreate after delete = %d %s", rec.Code, rec.Body.String())
	}
}

func TestSnapshotGroupCapacityReportAndPoolDelete(t *testing.T) {
	h := Handler()
	body := twoVolGroup(t, h)
	createGroup(t, h, body)

	// The capacity report counts the group's snapshots immediately.
	report := decodeBody(t, do(h, http.MethodGet, "/v1/capacity-report?poolId=gp", ""))
	item := report["items"].([]any)[0].(map[string]any)
	if item["snapshotBytes"].(float64) != 300 || item["allocatedBytes"].(float64) != 600 ||
		item["availableBytes"].(float64) != 400 {
		t.Fatalf("report with group = %v", item)
	}

	// The group keeps its pool non-empty even after the source volumes go.
	for _, v := range []string{"vol1", "vol2"} {
		if rec := do(h, http.MethodDelete, "/v1/volumes/"+v, ""); rec.Code != http.StatusNoContent {
			t.Fatalf("delete %s = %d", v, rec.Code)
		}
	}
	wantError(t, do(h, http.MethodDelete, "/v1/storage-pools/gp", ""), http.StatusConflict, "pool_not_empty")

	// Deleting the group frees the pool and updates the report at once.
	if rec := do(h, http.MethodDelete, "/v1/snapshot-groups/grp1", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete group = %d", rec.Code)
	}
	report = decodeBody(t, do(h, http.MethodGet, "/v1/capacity-report?poolId=gp", ""))
	item = report["items"].([]any)[0].(map[string]any)
	if item["snapshotBytes"].(float64) != 0 || item["allocatedBytes"].(float64) != 0 {
		t.Fatalf("report after group delete = %v", item)
	}
	if rec := do(h, http.MethodDelete, "/v1/storage-pools/gp", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete pool = %d", rec.Code)
	}
}

func TestSnapshotGroupTenantUsageReleased(t *testing.T) {
	h := Handler()
	createPool(t, h, groupPool)
	createVol(t, h, `{"id":"vol1","poolId":"gp","sizeBytes":100}`)
	createVol(t, h, `{"id":"vol2","poolId":"gp","sizeBytes":100}`)
	mustQuota(t, h, "gp", "default", 400)
	createGroup(t, h, `{"id":"g1","members":[`+
		`{"volumeId":"vol1","snapshotId":"s1","expectedGeneration":0},`+
		`{"volumeId":"vol2","snapshotId":"s2","expectedGeneration":0}]}`)

	// 200 volumes + 200 group snapshots exactly fill the 400-byte quota.
	q := quotaViewOf(t, do(h, http.MethodGet, "/v1/storage-pools/gp/tenant-quotas/default", ""))
	if q["usedBytes"].(float64) != 400 || q["availableBytes"].(float64) != 0 {
		t.Fatalf("quota with group = %v", q)
	}

	// Deleting the group releases the tenant's usage.
	if rec := do(h, http.MethodDelete, "/v1/snapshot-groups/g1", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete group = %d", rec.Code)
	}
	q = quotaViewOf(t, do(h, http.MethodGet, "/v1/storage-pools/gp/tenant-quotas/default", ""))
	if q["usedBytes"].(float64) != 200 || q["availableBytes"].(float64) != 200 {
		t.Fatalf("quota after group delete = %v", q)
	}
}

func TestSnapshotGroupRouting(t *testing.T) {
	h := Handler()

	rec := do(h, http.MethodGet, "/v1/snapshot-groups", "")
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "POST" {
		t.Fatalf("GET collection = %d allow=%q", rec.Code, rec.Header().Get("Allow"))
	}
	rec = do(h, http.MethodGet, "/v1/snapshot-groups/g1", "")
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "DELETE" {
		t.Fatalf("GET item = %d allow=%q", rec.Code, rec.Header().Get("Allow"))
	}
	wantError(t, do(h, http.MethodGet, "/v1/snapshot-groups/g1/extra", ""), http.StatusNotFound, "not_found")
	wantError(t, do(h, http.MethodGet, "/v1/snapshot-groups/", ""), http.StatusNotFound, "not_found")
}

func TestConcurrentSnapshotGroupRetriesMeteredOnce(t *testing.T) {
	h := Handler()
	body := twoVolGroup(t, h)

	const goroutines = 30
	var wg sync.WaitGroup
	statuses := make(chan int, goroutines)
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			statuses <- do(h, http.MethodPost, "/v1/snapshot-groups", body).Code
		}()
	}
	wg.Wait()
	close(statuses)
	var created, ok int
	for code := range statuses {
		switch code {
		case http.StatusCreated:
			created++
		case http.StatusOK:
			ok++
		default:
			t.Fatalf("unexpected status %d", code)
		}
	}
	if created != 1 || ok != goroutines-1 {
		t.Fatalf("created=%d ok=%d", created, ok)
	}
	// 300 volume bytes + exactly one 300-byte group charge.
	if p := getPoolView(t, h, "gp"); p["allocatedBytes"].(float64) != 600 {
		t.Fatalf("pool after concurrent retries = %v", p)
	}
	events := getAuditEvents(t, h, "/v1/audit-events")["items"].([]any)
	if len(events) != 4 { // pool, vol1, vol2, one group
		t.Fatalf("audit events after concurrent retries = %v", events)
	}
}

func TestConcurrentDistinctSnapshotGroupsNeverOvercommit(t *testing.T) {
	h := Handler()
	createPool(t, h, `{"id":"gp","devices":[{"id":"gdev","capacityBytes":100,"faultDomain":"fd"}]}`)
	createVol(t, h, `{"id":"vol1","poolId":"gp","sizeBytes":10}`)
	createVol(t, h, `{"id":"vol2","poolId":"gp","sizeBytes":10}`)

	// 20 bytes of volumes leave room for four 20-byte groups.
	const goroutines = 40
	var wg sync.WaitGroup
	var created, rejected int64
	var mu sync.Mutex
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(i int) {
			defer wg.Done()
			body := fmt.Sprintf(`{"id":"g%d","members":[`+
				`{"volumeId":"vol1","snapshotId":"s%da","expectedGeneration":0},`+
				`{"volumeId":"vol2","snapshotId":"s%db","expectedGeneration":0}]}`, i, i, i)
			rec := do(h, http.MethodPost, "/v1/snapshot-groups", body)
			mu.Lock()
			switch rec.Code {
			case http.StatusCreated:
				created++
			case http.StatusConflict:
				rejected++
			default:
				t.Errorf("unexpected status %d body=%s", rec.Code, rec.Body.String())
			}
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	if created != 4 || rejected != 36 {
		t.Fatalf("created=%d rejected=%d, want 4/36", created, rejected)
	}
	if p := getPoolView(t, h, "gp"); p["allocatedBytes"].(float64) != 100 || p["availableBytes"].(float64) != 0 {
		t.Fatalf("post-concurrency pool = %v", p)
	}
}

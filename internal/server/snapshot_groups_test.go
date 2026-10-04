package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func createSnapshotGroup(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := do(h, http.MethodPost, "/v1/snapshot-groups", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create snapshot group status = %d body = %s", rec.Code, rec.Body.String())
	}
	return rec
}

// groupViewOf extracts the snapshot ids of a group view in response order.
func groupSnapshotIDs(view map[string]any) []string {
	items := view["snapshots"].([]any)
	ids := make([]string, 0, len(items))
	for _, it := range items {
		ids = append(ids, it.(map[string]any)["id"].(string))
	}
	return ids
}

func TestCreateSnapshotGroupRepresentationAndCharging(t *testing.T) {
	h := Handler()
	createPool(t, h, volPool)
	createVol(t, h, `{"id":"vol1","poolId":"vp","sizeBytes":300}`)
	createVol(t, h, `{"id":"vol2","poolId":"vp","sizeBytes":200}`)

	// Members are listed out of snapshot-id order on purpose: the response
	// sorts the snapshot items by snapshotId.
	rec := createSnapshotGroup(t, h, `{"id":"grp1","members":[
		{"volumeId":"vol2","snapshotId":"snap-b","expectedGeneration":0},
		{"volumeId":"vol1","snapshotId":"snap-a","expectedGeneration":0}
	]}`)
	g := decodeBody(t, rec)
	if g["id"] != "grp1" || g["poolId"] != "vp" {
		t.Fatalf("group identity fields = %v", g)
	}
	ids := groupSnapshotIDs(g)
	if len(ids) != 2 || ids[0] != "snap-a" || ids[1] != "snap-b" {
		t.Fatalf("group snapshots not sorted by snapshotId: %v", ids)
	}
	first := g["snapshots"].([]any)[0].(map[string]any)
	if first["sourceVolumeId"] != "vol1" || first["sizeBytes"].(float64) != 300 ||
		first["sourceGeneration"].(float64) != 0 || first["poolId"] != "vp" {
		t.Fatalf("member snapshot view = %v", first)
	}

	// Both member snapshots charge the common pool on top of the volumes.
	p := getPoolView(t, h, "vp")
	if p["allocatedBytes"].(float64) != 1000 || p["availableBytes"].(float64) != 0 {
		t.Fatalf("pool after group create = %v", p)
	}
	report := decodeBody(t, do(h, http.MethodGet, "/v1/capacity-report?poolId=vp", ""))
	item := report["items"].([]any)[0].(map[string]any)
	if item["snapshotBytes"].(float64) != 500 || item["volumeBytes"].(float64) != 500 {
		t.Fatalf("capacity report after group create = %v", item)
	}

	// Exactly one snapshot-group.created event with the total member bytes.
	events := getAuditEvents(t, h, "/v1/audit-events?poolId=vp")["items"].([]any)
	checkEvent(t, events[len(events)-1], expectedEvent{
		sequence: 4, action: "snapshot-group.created",
		resource: "/v1/snapshot-groups/grp1", poolID: "vp", delta: 500,
	})
}

func TestSnapshotGroupIdempotency(t *testing.T) {
	h := Handler()
	createPool(t, h, volPool)
	createVol(t, h, `{"id":"vol1","poolId":"vp","sizeBytes":100}`)
	createVol(t, h, `{"id":"vol2","poolId":"vp","sizeBytes":100}`)
	createVol(t, h, `{"id":"vol3","poolId":"vp","sizeBytes":100}`)
	body := `{"id":"grp1","members":[
		{"volumeId":"vol1","snapshotId":"snap-a","expectedGeneration":0},
		{"volumeId":"vol2","snapshotId":"snap-b","expectedGeneration":0}
	]}`
	createSnapshotGroup(t, h, body)

	// Identical retry: 200, same representation, no double metering or audit.
	rec := do(h, http.MethodPost, "/v1/snapshot-groups", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("retry status = %d %s", rec.Code, rec.Body.String())
	}
	if ids := groupSnapshotIDs(decodeBody(t, rec)); ids[0] != "snap-a" || ids[1] != "snap-b" {
		t.Fatalf("retry view = %v", ids)
	}
	// Member order does not matter for a faithful retry.
	reordered := `{"id":"grp1","members":[
		{"volumeId":"vol2","snapshotId":"snap-b","expectedGeneration":0},
		{"volumeId":"vol1","snapshotId":"snap-a","expectedGeneration":0}
	]}`
	if rec := do(h, http.MethodPost, "/v1/snapshot-groups", reordered); rec.Code != http.StatusOK {
		t.Fatalf("reordered retry = %d %s", rec.Code, rec.Body.String())
	}
	if p := getPoolView(t, h, "vp"); p["allocatedBytes"].(float64) != 500 {
		t.Fatalf("allocated after retries = %v", p["allocatedBytes"])
	}
	events := getAuditEvents(t, h, "/v1/audit-events")["items"].([]any)
	if len(events) != 5 { // pool, 3 volumes, group
		t.Fatalf("audit events after retries = %v", events)
	}

	// Same group id with any different mapping is a conflict.
	for name, conflict := range map[string]string{
		"different snapshot id": `{"id":"grp1","members":[
			{"volumeId":"vol1","snapshotId":"snap-x","expectedGeneration":0},
			{"volumeId":"vol2","snapshotId":"snap-b","expectedGeneration":0}]}`,
		"different generation": `{"id":"grp1","members":[
			{"volumeId":"vol1","snapshotId":"snap-a","expectedGeneration":1},
			{"volumeId":"vol2","snapshotId":"snap-b","expectedGeneration":0}]}`,
		"different volume": `{"id":"grp1","members":[
			{"volumeId":"vol2","snapshotId":"snap-a","expectedGeneration":0},
			{"volumeId":"vol1","snapshotId":"snap-b","expectedGeneration":0}]}`,
		"extra member": `{"id":"grp1","members":[
			{"volumeId":"vol1","snapshotId":"snap-a","expectedGeneration":0},
			{"volumeId":"vol2","snapshotId":"snap-b","expectedGeneration":0},
			{"volumeId":"vol3","snapshotId":"snap-c","expectedGeneration":0}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			wantError(t, do(h, http.MethodPost, "/v1/snapshot-groups", conflict),
				http.StatusConflict, "snapshot_group_exists")
		})
	}
}

func TestSnapshotGroupIdempotentRetryAfterSourceDelete(t *testing.T) {
	h := Handler()
	createPool(t, h, volPool)
	createVol(t, h, `{"id":"vol1","poolId":"vp","sizeBytes":100}`)
	createVol(t, h, `{"id":"vol2","poolId":"vp","sizeBytes":100}`)
	body := `{"id":"grp1","members":[
		{"volumeId":"vol1","snapshotId":"snap-a","expectedGeneration":0},
		{"volumeId":"vol2","snapshotId":"snap-b","expectedGeneration":0}
	]}`
	createSnapshotGroup(t, h, body)

	// Deleting a source volume does not break the faithful retry.
	if rec := do(h, http.MethodDelete, "/v1/volumes/vol1", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete source = %d", rec.Code)
	}
	if rec := do(h, http.MethodPost, "/v1/snapshot-groups", body); rec.Code != http.StatusOK {
		t.Fatalf("retry after source delete = %d %s", rec.Code, rec.Body.String())
	}
}

func TestCreateSnapshotGroupInvalidRequests(t *testing.T) {
	h := Handler()
	createPool(t, h, volPool)
	createVol(t, h, `{"id":"vol1","poolId":"vp","sizeBytes":10}`)
	createVol(t, h, `{"id":"vol2","poolId":"vp","sizeBytes":10}`)

	member := `{"volumeId":"vol1","snapshotId":"snap-a","expectedGeneration":0}`
	other := `{"volumeId":"vol2","snapshotId":"snap-b","expectedGeneration":0}`
	cases := map[string]string{
		"unknown field":        `{"id":"g1","members":[` + member + `,` + other + `],"x":1}`,
		"unknown member field": `{"id":"g1","members":[` + member + `,{"volumeId":"vol2","snapshotId":"snap-b","expectedGeneration":0,"x":1}]}`,
		"missing id":           `{"members":[` + member + `,` + other + `]}`,
		"bad id":               `{"id":"bad id","members":[` + member + `,` + other + `]}`,
		"missing members":      `{"id":"g1"}`,
		"empty members":        `{"id":"g1","members":[]}`,
		"single member":        `{"id":"g1","members":[` + member + `]}`,
		"duplicate volume": `{"id":"g1","members":[` + member + `,
			{"volumeId":"vol1","snapshotId":"snap-c","expectedGeneration":0}]}`,
		"duplicate snapshot": `{"id":"g1","members":[` + member + `,
			{"volumeId":"vol2","snapshotId":"snap-a","expectedGeneration":0}]}`,
		"bad volume id":      `{"id":"g1","members":[{"volumeId":"bad id","snapshotId":"snap-a","expectedGeneration":0},` + other + `]}`,
		"bad snapshot id":    `{"id":"g1","members":[{"volumeId":"vol1","snapshotId":"","expectedGeneration":0},` + other + `]}`,
		"missing generation": `{"id":"g1","members":[{"volumeId":"vol1","snapshotId":"snap-a"},` + other + `]}`,
		"null generation":    `{"id":"g1","members":[{"volumeId":"vol1","snapshotId":"snap-a","expectedGeneration":null},` + other + `]}`,
		"negative generation": `{"id":"g1","members":[
			{"volumeId":"vol1","snapshotId":"snap-a","expectedGeneration":-1},` + other + `]}`,
		"float generation": `{"id":"g1","members":[
			{"volumeId":"vol1","snapshotId":"snap-a","expectedGeneration":0.5},` + other + `]}`,
		"malformed json": `{"id":"g1",`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			wantError(t, do(h, http.MethodPost, "/v1/snapshot-groups", body),
				http.StatusBadRequest, "invalid_request")
		})
	}

	// 65 members exceed the per-group limit.
	var b strings.Builder
	b.WriteString(`{"id":"g1","members":[`)
	for i := 0; i < 65; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"volumeId":"vol1","snapshotId":"s%d","expectedGeneration":0}`, i)
	}
	b.WriteString(`]}`)
	wantError(t, do(h, http.MethodPost, "/v1/snapshot-groups", b.String()),
		http.StatusBadRequest, "invalid_request")

	// Any query parameter is rejected.
	wantError(t, do(h, http.MethodPost, "/v1/snapshot-groups?x=1",
		`{"id":"g1","members":[`+member+`,`+other+`]}`), http.StatusBadRequest, "invalid_request")
	wantError(t, do(h, http.MethodPost, "/v1/snapshot-groups?",
		`{"id":"g1","members":[`+member+`,`+other+`]}`), http.StatusBadRequest, "invalid_request")

	// No partial state: no group, no snapshot, no charge, no audit event.
	if rec := do(h, http.MethodGet, "/v1/snapshots", ""); rec.Body.String() != "{\"items\":[]}\n" {
		t.Fatalf("partial snapshot state: %s", rec.Body.String())
	}
	if p := getPoolView(t, h, "vp"); p["allocatedBytes"].(float64) != 20 {
		t.Fatalf("partial allocation: %v", p["allocatedBytes"])
	}
	events := getAuditEvents(t, h, "/v1/audit-events")["items"].([]any)
	if len(events) != 3 { // pool + 2 volumes only
		t.Fatalf("audit events after invalid requests = %v", events)
	}
}

func TestCreateSnapshotGroupMemberErrors(t *testing.T) {
	h := Handler()
	createPool(t, h, volPool)
	createPool(t, h, `{"id":"vp2","devices":[{"id":"vdev2","capacityBytes":1000,"faultDomain":"rack2"}]}`)
	createVol(t, h, `{"id":"vol1","poolId":"vp","sizeBytes":100}`)
	createVol(t, h, `{"id":"vol2","poolId":"vp","sizeBytes":100}`)
	createVol(t, h, `{"id":"vol3","poolId":"vp2","sizeBytes":100}`)

	two := func(snapA, snapB string) string {
		return `{"id":"g1","members":[
			{"volumeId":"vol1","snapshotId":"` + snapA + `","expectedGeneration":0},
			{"volumeId":"vol2","snapshotId":"` + snapB + `","expectedGeneration":0}
		]}`
	}

	// Missing volume, checked in member order.
	wantError(t, do(h, http.MethodPost, "/v1/snapshot-groups", `{"id":"g1","members":[
		{"volumeId":"vol1","snapshotId":"snap-a","expectedGeneration":0},
		{"volumeId":"missing","snapshotId":"snap-b","expectedGeneration":0}
	]}`), http.StatusNotFound, "volume_not_found")

	// Bound member volume: in-use takes precedence over a stale generation.
	if rec := do(h, http.MethodPut, "/v1/volumes/vol2/binding", `{"nodeId":"n1","expectedGeneration":0}`); rec.Code != http.StatusOK {
		t.Fatalf("bind = %d", rec.Code)
	}
	wantError(t, do(h, http.MethodPost, "/v1/snapshot-groups", two("snap-a", "snap-b")),
		http.StatusConflict, "volume_in_use")
	if rec := do(h, http.MethodDelete, "/v1/volumes/vol2/binding?expectedGeneration=1", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("unbind = %d", rec.Code)
	}

	// vol2 is now at generation 2.
	wantError(t, do(h, http.MethodPost, "/v1/snapshot-groups", two("snap-a", "snap-b")),
		http.StatusConflict, "stale_generation")

	// Members spanning two pools.
	wantError(t, do(h, http.MethodPost, "/v1/snapshot-groups", `{"id":"g1","members":[
		{"volumeId":"vol1","snapshotId":"snap-a","expectedGeneration":0},
		{"volumeId":"vol3","snapshotId":"snap-b","expectedGeneration":0}
	]}`), http.StatusConflict, "cross_pool_snapshot_group")

	// A member snapshot id already taken by a standalone snapshot.
	createSnapshot(t, h, "vol1", `{"id":"taken","expectedGeneration":0}`)
	wantError(t, do(h, http.MethodPost, "/v1/snapshot-groups", `{"id":"g1","members":[
		{"volumeId":"vol1","snapshotId":"taken","expectedGeneration":0},
		{"volumeId":"vol2","snapshotId":"snap-b","expectedGeneration":2}
	]}`), http.StatusConflict, "snapshot_exists")

	// Every failure above left nothing behind: no group, no extra snapshot,
	// no charge beyond the standalone snapshot, and no audit event.
	if rec := do(h, http.MethodDelete, "/v1/snapshot-groups/g1", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("group g1 exists after failures: %d", rec.Code)
	}
	if p := getPoolView(t, h, "vp"); p["allocatedBytes"].(float64) != 300 {
		t.Fatalf("allocated after failures = %v", p["allocatedBytes"])
	}
	for _, e := range getAuditEvents(t, h, "/v1/audit-events")["items"].([]any) {
		action := e.(map[string]any)["action"].(string)
		if action == "snapshot-group.created" || action == "snapshot-group.deleted" {
			t.Fatalf("group audit event after failure: %v", e)
		}
	}
}

func TestCreateSnapshotGroupQuotaAndCapacity(t *testing.T) {
	h := Handler()
	createPool(t, h, `{"id":"qp","devices":[{"id":"qd","capacityBytes":1000,"faultDomain":"fd"}]}`)
	createVol(t, h, `{"id":"vol1","poolId":"qp","sizeBytes":100}`)
	doTenant(h, http.MethodPost, "/v1/volumes", `{"id":"vol2","poolId":"qp","sizeBytes":100}`, "team-a")
	createVol(t, h, `{"id":"filler","poolId":"qp","sizeBytes":750}`)

	// The member sizes aggregate per tenant: the group charges 100 to
	// default (vol1) and 100 to team-a (vol2).
	group := `{"id":"g1","members":[
		{"volumeId":"vol1","snapshotId":"snap-a","expectedGeneration":0},
		{"volumeId":"vol2","snapshotId":"snap-b","expectedGeneration":0}
	]}`

	// team-a's quota cannot fit its 100-byte share of the group.
	mustQuota(t, h, "qp", "team-a", 150)
	wantError(t, do(h, http.MethodPost, "/v1/snapshot-groups", group),
		http.StatusConflict, "tenant_quota_exceeded")

	// Quota is checked before pool capacity: with only 50 bytes free the
	// group would not fit either, but the quota error still wins.
	mustQuota(t, h, "qp", "default", 900)
	wantError(t, do(h, http.MethodPost, "/v1/snapshot-groups", group),
		http.StatusConflict, "tenant_quota_exceeded")

	// With quota headroom for both tenants, pool capacity is reported.
	if rec := putQuota(t, h, "qp", "team-a", 500); rec.Code != http.StatusOK {
		t.Fatalf("raise team-a quota = %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(h, http.MethodDelete, "/v1/storage-pools/qp/tenant-quotas/default", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete default quota = %d", rec.Code)
	}
	wantError(t, do(h, http.MethodPost, "/v1/snapshot-groups", group),
		http.StatusConflict, "insufficient_capacity")

	// Nothing was charged or audited by the failures.
	if used := decodeBody(t, do(h, http.MethodGet, "/v1/storage-pools/qp/tenant-quotas/team-a", ""))["usedBytes"].(float64); used != 100 {
		t.Fatalf("team-a usage after failures = %v", used)
	}
	for _, e := range getAuditEvents(t, h, "/v1/audit-events")["items"].([]any) {
		if e.(map[string]any)["action"].(string) == "snapshot-group.created" {
			t.Fatalf("group audit event after failure: %v", e)
		}
	}
}

func TestSnapshotGroupMembersBehaveLikeSnapshots(t *testing.T) {
	h := Handler()
	createPool(t, h, volPool)
	createVol(t, h, `{"id":"vol1","poolId":"vp","sizeBytes":100}`)
	createVol(t, h, `{"id":"vol2","poolId":"vp","sizeBytes":200}`)
	createSnapshotGroup(t, h, `{"id":"grp1","members":[
		{"volumeId":"vol1","snapshotId":"snap-a","expectedGeneration":0},
		{"volumeId":"vol2","snapshotId":"snap-b","expectedGeneration":0}
	]}`)

	// Members are listed and readable like any other snapshot.
	items := decodeBody(t, do(h, http.MethodGet, "/v1/snapshots", ""))["items"].([]any)
	if len(items) != 2 || items[0].(map[string]any)["id"] != "snap-a" || items[1].(map[string]any)["id"] != "snap-b" {
		t.Fatalf("snapshot list = %v", items)
	}
	s := decodeBody(t, do(h, http.MethodGet, "/v1/snapshots/snap-b", ""))
	if s["sourceVolumeId"] != "vol2" || s["sizeBytes"].(float64) != 200 {
		t.Fatalf("member snapshot view = %v", s)
	}

	// A member snapshot can be restored into its source volume.
	if rec := do(h, http.MethodPut, "/v1/volumes/vol2/size", `{"sizeBytes":150,"expectedGeneration":0}`); rec.Code != http.StatusOK {
		t.Fatalf("resize = %d", rec.Code)
	}
	if rec := do(h, http.MethodPost, "/v1/volumes/vol2/restore", `{"snapshotId":"snap-b","expectedGeneration":1}`); rec.Code != http.StatusOK {
		t.Fatalf("restore = %d %s", rec.Code, rec.Body.String())
	}
	if v := decodeBody(t, do(h, http.MethodGet, "/v1/volumes/vol2", "")); v["sizeBytes"].(float64) != 200 {
		t.Fatalf("volume after restore = %v", v)
	}

	// A member snapshot can be cloned; the clone is an ordinary volume.
	rec := clone(t, h, "snap-a", `{"id":"vol-copy","poolId":"vp"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("clone = %d %s", rec.Code, rec.Body.String())
	}

	// A member snapshot cannot be deleted directly.
	wantError(t, do(h, http.MethodDelete, "/v1/snapshots/snap-a", ""),
		http.StatusConflict, "snapshot_in_group")
	report := decodeBody(t, do(h, http.MethodGet, "/v1/capacity-report?poolId=vp", ""))
	item := report["items"].([]any)[0].(map[string]any)
	if item["snapshotBytes"].(float64) != 300 {
		t.Fatalf("snapshotBytes after rejected delete = %v", item["snapshotBytes"])
	}
}

func TestDeleteSnapshotGroup(t *testing.T) {
	h := Handler()
	createPool(t, h, volPool)
	doTenant(h, http.MethodPost, "/v1/volumes", `{"id":"vol1","poolId":"vp","sizeBytes":100}`, "team-a")
	createVol(t, h, `{"id":"vol2","poolId":"vp","sizeBytes":200}`)
	mustQuota(t, h, "vp", "team-a", 500)
	createSnapshotGroup(t, h, `{"id":"grp1","members":[
		{"volumeId":"vol1","snapshotId":"snap-a","expectedGeneration":0},
		{"volumeId":"vol2","snapshotId":"snap-b","expectedGeneration":0}
	]}`)
	// A clone of a member snapshot survives the group deletion.
	if rec := clone(t, h, "snap-a", `{"id":"vol-copy","poolId":"vp"}`); rec.Code != http.StatusCreated {
		t.Fatalf("clone = %d %s", rec.Code, rec.Body.String())
	}

	rec := do(h, http.MethodDelete, "/v1/snapshot-groups/grp1", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete group = %d %s", rec.Code, rec.Body.String())
	}
	// All member snapshots are gone and their charge is released.
	wantError(t, do(h, http.MethodGet, "/v1/snapshots/snap-a", ""), http.StatusNotFound, "snapshot_not_found")
	wantError(t, do(h, http.MethodGet, "/v1/snapshots/snap-b", ""), http.StatusNotFound, "snapshot_not_found")
	p := getPoolView(t, h, "vp")
	if p["allocatedBytes"].(float64) != 400 {
		t.Fatalf("pool after group delete = %v", p)
	}
	report := decodeBody(t, do(h, http.MethodGet, "/v1/capacity-report?poolId=vp", ""))
	if item := report["items"].([]any)[0].(map[string]any); item["snapshotBytes"].(float64) != 0 {
		t.Fatalf("snapshotBytes after group delete = %v", item["snapshotBytes"])
	}
	if used := decodeBody(t, do(h, http.MethodGet, "/v1/storage-pools/vp/tenant-quotas/team-a", ""))["usedBytes"].(float64); used != 200 {
		t.Fatalf("team-a usage after group delete = %v", used)
	}
	// The clone is unaffected.
	if rec := do(h, http.MethodGet, "/v1/volumes/vol-copy", ""); rec.Code != http.StatusOK {
		t.Fatalf("clone after group delete = %d", rec.Code)
	}
	// One snapshot-group.deleted event with the negative total bytes.
	events := getAuditEvents(t, h, "/v1/audit-events?poolId=vp")["items"].([]any)
	last := events[len(events)-1]
	checkEvent(t, last, expectedEvent{
		sequence: 7, action: "snapshot-group.deleted",
		resource: "/v1/snapshot-groups/grp1", poolID: "vp", delta: -300,
	})

	// Deleting again reports the missing group.
	wantError(t, do(h, http.MethodDelete, "/v1/snapshot-groups/grp1", ""),
		http.StatusNotFound, "snapshot_group_not_found")
	wantError(t, do(h, http.MethodDelete, "/v1/snapshot-groups/never", ""),
		http.StatusNotFound, "snapshot_group_not_found")
	wantError(t, do(h, http.MethodDelete, "/v1/snapshot-groups/bad%20id", ""),
		http.StatusBadRequest, "invalid_request")

	// The group id and snapshot ids can be reused after deletion.
	createSnapshotGroup(t, h, `{"id":"grp1","members":[
		{"volumeId":"vol1","snapshotId":"snap-a","expectedGeneration":0},
		{"volumeId":"vol2","snapshotId":"snap-b","expectedGeneration":0}
	]}`)
}

func TestSnapshotGroupRouting(t *testing.T) {
	h := Handler()
	rec := do(h, http.MethodGet, "/v1/snapshot-groups", "")
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "POST" {
		t.Fatalf("GET collection = %d allow=%q", rec.Code, rec.Header().Get("Allow"))
	}
	rec = do(h, http.MethodPut, "/v1/snapshot-groups/g1", "")
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "DELETE" {
		t.Fatalf("PUT member = %d allow=%q", rec.Code, rec.Header().Get("Allow"))
	}
	wantError(t, do(h, http.MethodGet, "/v1/snapshot-groups/g1/extra", ""), http.StatusNotFound, "not_found")
	wantError(t, do(h, http.MethodGet, "/v1/snapshot-groups/", ""), http.StatusNotFound, "not_found")
}

func TestConcurrentSnapshotGroupRetriesMeteredOnce(t *testing.T) {
	h := Handler()
	createPool(t, h, `{"id":"cp","devices":[{"id":"d","capacityBytes":1000,"faultDomain":"fd"}]}`)
	createVol(t, h, `{"id":"vol1","poolId":"cp","sizeBytes":100}`)
	createVol(t, h, `{"id":"vol2","poolId":"cp","sizeBytes":100}`)
	body := `{"id":"same","members":[
		{"volumeId":"vol1","snapshotId":"snap-a","expectedGeneration":0},
		{"volumeId":"vol2","snapshotId":"snap-b","expectedGeneration":0}
	]}`

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
	// Volumes 200 + one group charge 200, never more.
	if p := getPoolView(t, h, "cp"); p["allocatedBytes"].(float64) != 400 {
		t.Fatalf("pool after concurrent retries = %v", p)
	}
}

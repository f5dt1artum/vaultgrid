package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func restore(h http.Handler, id, body string) *httptest.ResponseRecorder {
	return do(h, http.MethodPost, "/v1/volumes/"+id+"/restore", body)
}

func snapshotVol(t *testing.T, h http.Handler, volumeID, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := do(h, http.MethodPost, "/v1/volumes/"+volumeID+"/snapshots", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create snapshot status = %d body = %s", rec.Code, rec.Body.String())
	}
	return rec
}

func TestRestoreVolumeGrowShrinkAndSameSize(t *testing.T) {
	h := Handler()
	createPool(t, h, volPool)
	createVol(t, h, `{"id":"vol1","poolId":"vp","sizeBytes":300}`)
	snapshotVol(t, h, "vol1", `{"id":"snap1","expectedGeneration":0}`)

	// Grow the volume past the snapshot, then restore: size returns to the
	// snapshot's 300, generation bumps, binding stays null.
	if rec := resize(h, "vol1", `{"sizeBytes":600,"expectedGeneration":0}`); rec.Code != http.StatusOK {
		t.Fatalf("resize = %d %s", rec.Code, rec.Body.String())
	}
	rec := restore(h, "vol1", `{"snapshotId":"snap1","expectedGeneration":1}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("restore shrink status = %d %s", rec.Code, rec.Body.String())
	}
	v := decodeBody(t, rec)
	if v["id"] != "vol1" || v["poolId"] != "vp" {
		t.Fatalf("identity changed: %v", v)
	}
	if v["sizeBytes"].(float64) != 300 || v["generation"].(float64) != 2 {
		t.Fatalf("restore shrink view = %v", v)
	}
	if binding, ok := v["binding"]; !ok || binding != nil {
		t.Fatalf("binding after restore = %v", binding)
	}
	// Pool holds the restored volume (300) plus the kept snapshot (300).
	p := decodeBody(t, do(h, http.MethodGet, "/v1/storage-pools/vp", ""))
	if p["allocatedBytes"].(float64) != 600 || p["availableBytes"].(float64) != 400 {
		t.Fatalf("pool after shrink restore = %v", p)
	}

	// Shrink the volume below the snapshot, then restore: a growth restore.
	if rec := resize(h, "vol1", `{"sizeBytes":100,"expectedGeneration":2}`); rec.Code != http.StatusOK {
		t.Fatalf("resize = %d %s", rec.Code, rec.Body.String())
	}
	rec = restore(h, "vol1", `{"snapshotId":"snap1","expectedGeneration":3}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("restore grow status = %d %s", rec.Code, rec.Body.String())
	}
	v = decodeBody(t, rec)
	if v["sizeBytes"].(float64) != 300 || v["generation"].(float64) != 4 {
		t.Fatalf("restore grow view = %v", v)
	}
	p = decodeBody(t, do(h, http.MethodGet, "/v1/storage-pools/vp", ""))
	if p["allocatedBytes"].(float64) != 600 {
		t.Fatalf("pool after grow restore = %v", p)
	}

	// Restoring at the snapshot's own size is still a real commit: generation
	// bumps and an event is recorded even though the size delta is zero.
	rec = restore(h, "vol1", `{"snapshotId":"snap1","expectedGeneration":4}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("same-size restore status = %d %s", rec.Code, rec.Body.String())
	}
	v = decodeBody(t, rec)
	if v["sizeBytes"].(float64) != 300 || v["generation"].(float64) != 5 {
		t.Fatalf("same-size restore view = %v", v)
	}

	// The snapshot itself is untouched and still listed.
	snap := decodeBody(t, do(h, http.MethodGet, "/v1/snapshots/snap1", ""))
	if snap["sizeBytes"].(float64) != 300 || snap["sourceGeneration"].(float64) != 0 {
		t.Fatalf("snapshot mutated by restore: %v", snap)
	}
}

func TestRestoreVolumeValidation(t *testing.T) {
	h := Handler()
	createPool(t, h, volPool)
	createVol(t, h, `{"id":"vol1","poolId":"vp","sizeBytes":10}`)
	snapshotVol(t, h, "vol1", `{"id":"snap1","expectedGeneration":0}`)

	for name, body := range map[string]string{
		"unknown field":       `{"snapshotId":"snap1","expectedGeneration":0,"extra":1}`,
		"missing snapshot":    `{"expectedGeneration":0}`,
		"missing generation":  `{"snapshotId":"snap1"}`,
		"null generation":     `{"snapshotId":"snap1","expectedGeneration":null}`,
		"empty snapshot id":   `{"snapshotId":"","expectedGeneration":0}`,
		"bad snapshot id":     `{"snapshotId":"bad id!","expectedGeneration":0}`,
		"numeric snapshot id": `{"snapshotId":7,"expectedGeneration":0}`,
		"negative generation": `{"snapshotId":"snap1","expectedGeneration":-1}`,
		"float generation":    `{"snapshotId":"snap1","expectedGeneration":0.5}`,
		"string generation":   `{"snapshotId":"snap1","expectedGeneration":"0"}`,
		"malformed json":      `{`,
	} {
		t.Run(name, func(t *testing.T) {
			wantError(t, restore(h, "vol1", body), http.StatusBadRequest, "invalid_request")
		})
	}

	// Illegal volume id and any query string are invalid_request.
	wantError(t, do(h, http.MethodPost, "/v1/volumes/bad%20id/restore", `{"snapshotId":"snap1","expectedGeneration":0}`),
		http.StatusBadRequest, "invalid_request")
	for _, target := range []string{
		"/v1/volumes/vol1/restore?",
		"/v1/volumes/vol1/restore?expectedGeneration=0",
		"/v1/volumes/vol1/restore?snapshotId=snap1",
		"/v1/volumes/vol1/restore?x=",
	} {
		wantError(t, do(h, http.MethodPost, target, `{"snapshotId":"snap1","expectedGeneration":0}`),
			http.StatusBadRequest, "invalid_request")
	}

	// Unknown volume and unknown snapshot: 404s.
	wantError(t, restore(h, "nope", `{"snapshotId":"snap1","expectedGeneration":0}`),
		http.StatusNotFound, "volume_not_found")
	wantError(t, restore(h, "vol1", `{"snapshotId":"nope","expectedGeneration":0}`),
		http.StatusNotFound, "snapshot_not_found")

	// No state changed by any failed request.
	v := decodeBody(t, do(h, http.MethodGet, "/v1/volumes/vol1", ""))
	if v["sizeBytes"].(float64) != 10 || v["generation"].(float64) != 0 {
		t.Fatalf("partial restore state: %v", v)
	}
}

func TestRestoreVolumeBindingAndGenerationPrecedence(t *testing.T) {
	h := Handler()
	createPool(t, h, volPool)
	createVol(t, h, `{"id":"vol1","poolId":"vp","sizeBytes":10}`)
	snapshotVol(t, h, "vol1", `{"id":"snap1","expectedGeneration":0}`)

	// Stale generation on an unbound volume.
	wantError(t, restore(h, "vol1", `{"snapshotId":"snap1","expectedGeneration":5}`),
		http.StatusConflict, "stale_generation")

	// Bind: generation moves 0 -> 1.
	if rec := do(h, http.MethodPut, "/v1/volumes/vol1/binding", `{"nodeId":"n1","expectedGeneration":0}`); rec.Code != http.StatusOK {
		t.Fatalf("bind = %d %s", rec.Code, rec.Body.String())
	}

	// A bound volume is rejected with volume_in_use regardless of whether the
	// generation matches: binding takes precedence over the generation check.
	wantError(t, restore(h, "vol1", `{"snapshotId":"snap1","expectedGeneration":0}`),
		http.StatusConflict, "volume_in_use")
	wantError(t, restore(h, "vol1", `{"snapshotId":"snap1","expectedGeneration":1}`),
		http.StatusConflict, "volume_in_use")

	// Unbind: generation 1 -> 2; restore is accepted at the new generation.
	if rec := do(h, http.MethodDelete, "/v1/volumes/vol1/binding?expectedGeneration=1", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("unbind = %d", rec.Code)
	}
	wantError(t, restore(h, "vol1", `{"snapshotId":"snap1","expectedGeneration":1}`),
		http.StatusConflict, "stale_generation")
	rec := restore(h, "vol1", `{"snapshotId":"snap1","expectedGeneration":2}`)
	if rec.Code != http.StatusOK || decodeBody(t, rec)["generation"].(float64) != 3 {
		t.Fatalf("restore after unbind = %d %s", rec.Code, rec.Body.String())
	}
}

func TestRestoreSnapshotSourceMismatch(t *testing.T) {
	h := Handler()
	createPool(t, h, volPool)
	createVol(t, h, `{"id":"vol1","poolId":"vp","sizeBytes":100}`)
	createVol(t, h, `{"id":"vol2","poolId":"vp","sizeBytes":100}`)
	snapshotVol(t, h, "vol1", `{"id":"snap1","expectedGeneration":0}`)
	snapshotVol(t, h, "vol2", `{"id":"snap2","expectedGeneration":0}`)

	// A snapshot of a different volume cannot restore vol1.
	wantError(t, restore(h, "vol1", `{"snapshotId":"snap2","expectedGeneration":0}`),
		http.StatusConflict, "snapshot_source_mismatch")

	// Delete vol1 and re-create it under the same id: the old snapshot is not
	// the new instance's history, even though the id and generation match.
	if rec := do(h, http.MethodDelete, "/v1/volumes/vol1", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d", rec.Code)
	}
	createVol(t, h, `{"id":"vol1","poolId":"vp","sizeBytes":100}`)
	wantError(t, restore(h, "vol1", `{"snapshotId":"snap1","expectedGeneration":0}`),
		http.StatusConflict, "snapshot_source_mismatch")

	// The snapshot itself survives and can still be cloned.
	createPool(t, h, `{"id":"pool-b","devices":[{"id":"db","capacityBytes":500,"faultDomain":"rack1"}]}`)
	if rec := do(h, http.MethodPost, "/v1/snapshots/snap1/clones", `{"id":"clone1","poolId":"pool-b"}`); rec.Code != http.StatusCreated {
		t.Fatalf("clone of old snapshot = %d %s", rec.Code, rec.Body.String())
	}

	// A snapshot taken from the new instance restores it fine.
	snapshotVol(t, h, "vol1", `{"id":"snap3","expectedGeneration":0}`)
	if rec := resize(h, "vol1", `{"sizeBytes":200,"expectedGeneration":0}`); rec.Code != http.StatusOK {
		t.Fatalf("resize = %d %s", rec.Code, rec.Body.String())
	}
	rec := restore(h, "vol1", `{"snapshotId":"snap3","expectedGeneration":1}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("restore from new instance = %d %s", rec.Code, rec.Body.String())
	}
	v := decodeBody(t, rec)
	if v["sizeBytes"].(float64) != 100 || v["generation"].(float64) != 2 {
		t.Fatalf("restore view = %v", v)
	}
}

func TestRestoreQuotaAndCapacityOrdering(t *testing.T) {
	h := Handler()
	createPool(t, h, `{"id":"qp","devices":[{"id":"d","capacityBytes":600,"faultDomain":"rack1"}]}`)
	putQuota(t, h, "qp", "team-a", 300)
	if rec := doTenant(h, http.MethodPost, "/v1/volumes",
		`{"id":"vol1","poolId":"qp","sizeBytes":100}`, "team-a"); rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
	}

	// Snapshot at 100 (charging the tenant another 100), then shrink the
	// volume to 10 so restoring is a growth of 90.
	snapshotVol(t, h, "vol1", `{"id":"snap1","expectedGeneration":0}`)
	if rec := doTenant(h, http.MethodPut, "/v1/volumes/vol1/size",
		`{"sizeBytes":10,"expectedGeneration":0}`, "team-a"); rec.Code != http.StatusOK {
		t.Fatalf("shrink = %d %s", rec.Code, rec.Body.String())
	}

	// Tighten the quota below what the restore growth needs: tenant usage is
	// 110 (10 volume + 100 snapshot); restoring adds 90 -> 200. A quota of 150
	// rejects it while the pool (600) would fit: quota is checked first.
	putQuota(t, h, "qp", "team-a", 150)
	wantError(t, doTenant(h, http.MethodPost, "/v1/volumes/vol1/restore",
		`{"snapshotId":"snap1","expectedGeneration":1}`, "team-a"),
		http.StatusConflict, "tenant_quota_exceeded")

	// Raise the quota so only the pool can reject: fill the pool with a
	// reservation that leaves less than the 90-byte growth.
	putQuota(t, h, "qp", "team-a", 1000)
	if rec := reserve(t, h, "qp", `{"requestId":"r1","bytes":450}`); rec.Code != http.StatusCreated {
		t.Fatalf("reserve = %d %s", rec.Code, rec.Body.String())
	}
	wantError(t, doTenant(h, http.MethodPost, "/v1/volumes/vol1/restore",
		`{"snapshotId":"snap1","expectedGeneration":1}`, "team-a"),
		http.StatusConflict, "insufficient_capacity")

	// Free the reservation; the restore now fits exactly and charges the
	// tenant the 90-byte delta.
	if rec := do(h, http.MethodDelete, "/v1/storage-pools/qp/reservations/r1", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete reservation = %d", rec.Code)
	}
	rec := doTenant(h, http.MethodPost, "/v1/volumes/vol1/restore",
		`{"snapshotId":"snap1","expectedGeneration":1}`, "team-a")
	if rec.Code != http.StatusOK {
		t.Fatalf("restore = %d %s", rec.Code, rec.Body.String())
	}
	q := decodeBody(t, do(h, http.MethodGet, "/v1/storage-pools/qp/tenant-quotas/team-a", ""))
	if q["usedBytes"].(float64) != 200 || q["availableBytes"].(float64) != 800 {
		t.Fatalf("quota after restore = %v", q)
	}

	// Shrink restores ignore both limits: take a small snapshot, grow the
	// volume to the quota limit, then restore down past any limit.
	snapshotVol(t, h, "vol1", `{"id":"snap2","expectedGeneration":2}`)
	if rec := doTenant(h, http.MethodPut, "/v1/volumes/vol1/size",
		`{"sizeBytes":400,"expectedGeneration":2}`, "team-a"); rec.Code != http.StatusOK {
		t.Fatalf("grow = %d %s", rec.Code, rec.Body.String())
	}
	putQuota(t, h, "qp", "team-a", 600)
	rec = doTenant(h, http.MethodPost, "/v1/volumes/vol1/restore",
		`{"snapshotId":"snap2","expectedGeneration":3}`, "team-a")
	if rec.Code != http.StatusOK {
		t.Fatalf("shrink restore = %d %s", rec.Code, rec.Body.String())
	}
	q = decodeBody(t, do(h, http.MethodGet, "/v1/storage-pools/qp/tenant-quotas/team-a", ""))
	if q["usedBytes"].(float64) != 300 {
		t.Fatalf("quota after shrink restore = %v", q)
	}
}

func TestRestoreFailuresLeaveNoStateOrAudit(t *testing.T) {
	h := Handler()
	createPool(t, h, volPool)
	createVol(t, h, `{"id":"vol1","poolId":"vp","sizeBytes":100}`)
	snapshotVol(t, h, "vol1", `{"id":"snap1","expectedGeneration":0}`)
	if rec := resize(h, "vol1", `{"sizeBytes":200,"expectedGeneration":0}`); rec.Code != http.StatusOK {
		t.Fatalf("resize = %d", rec.Code)
	}

	before := getAuditEvents(t, h, "/v1/audit-events?poolId=vp")
	wantError(t, restore(h, "vol1", `{"snapshotId":"snap1","expectedGeneration":9}`),
		http.StatusConflict, "stale_generation")
	wantError(t, restore(h, "vol1", `{"snapshotId":"nope","expectedGeneration":1}`),
		http.StatusNotFound, "snapshot_not_found")
	wantError(t, restore(h, "vol1", `{"snapshotId":"snap1","expectedGeneration":-1}`),
		http.StatusBadRequest, "invalid_request")

	v := decodeBody(t, do(h, http.MethodGet, "/v1/volumes/vol1", ""))
	if v["sizeBytes"].(float64) != 200 || v["generation"].(float64) != 1 {
		t.Fatalf("volume changed on failed restore: %v", v)
	}
	p := decodeBody(t, do(h, http.MethodGet, "/v1/storage-pools/vp", ""))
	if p["allocatedBytes"].(float64) != 300 {
		t.Fatalf("pool changed on failed restore: %v", p)
	}
	after := getAuditEvents(t, h, "/v1/audit-events?poolId=vp")
	if len(after["items"].([]any)) != len(before["items"].([]any)) {
		t.Fatalf("audit events changed on failure: before=%v after=%v", before, after)
	}
}

func TestConcurrentRestoreOnlyOneWins(t *testing.T) {
	h := Handler()
	createPool(t, h, volPool)
	createVol(t, h, `{"id":"vol1","poolId":"vp","sizeBytes":100}`)
	snapshotVol(t, h, "vol1", `{"id":"snap1","expectedGeneration":0}`)
	if rec := resize(h, "vol1", `{"sizeBytes":200,"expectedGeneration":0}`); rec.Code != http.StatusOK {
		t.Fatalf("resize = %d", rec.Code)
	}

	const goroutines = 30
	var wg sync.WaitGroup
	statuses := make(chan int, goroutines)
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			// Every request uses the same current generation; at most one can
			// commit, the rest observe the bumped generation as stale.
			statuses <- restore(h, "vol1", `{"snapshotId":"snap1","expectedGeneration":1}`).Code
		}()
	}
	wg.Wait()
	close(statuses)
	var ok200, stale int
	for code := range statuses {
		switch code {
		case http.StatusOK:
			ok200++
		case http.StatusConflict:
			stale++
		default:
			t.Fatalf("unexpected status %d", code)
		}
	}
	if ok200 != 1 || stale != goroutines-1 {
		t.Fatalf("200=%d stale=%d", ok200, stale)
	}
	v := decodeBody(t, do(h, http.MethodGet, "/v1/volumes/vol1", ""))
	if v["sizeBytes"].(float64) != 100 || v["generation"].(float64) != 2 {
		t.Fatalf("volume after concurrent restore = %v", v)
	}
	// Pool accounting matches exactly one restore: 100 volume + 100 snapshot.
	p := decodeBody(t, do(h, http.MethodGet, "/v1/storage-pools/vp", ""))
	if p["allocatedBytes"].(float64) != 200 {
		t.Fatalf("pool after concurrent restore = %v", p)
	}
}

func TestRestoreAuditEvent(t *testing.T) {
	h := Handler()
	createPool(t, h, volPool)
	createVol(t, h, `{"id":"vol1","poolId":"vp","sizeBytes":300}`)
	snapshotVol(t, h, "vol1", `{"id":"snap1","expectedGeneration":0}`)

	// Shrink restore (delta -200), grow restore (delta +200), same-size
	// restore (delta 0, still recorded), then a failed restore (not recorded).
	if rec := resize(h, "vol1", `{"sizeBytes":500,"expectedGeneration":0}`); rec.Code != http.StatusOK {
		t.Fatalf("grow = %d", rec.Code)
	}
	if rec := restore(h, "vol1", `{"snapshotId":"snap1","expectedGeneration":1}`); rec.Code != http.StatusOK {
		t.Fatalf("shrink restore = %d", rec.Code)
	}
	if rec := resize(h, "vol1", `{"sizeBytes":100,"expectedGeneration":2}`); rec.Code != http.StatusOK {
		t.Fatalf("shrink = %d", rec.Code)
	}
	if rec := restore(h, "vol1", `{"snapshotId":"snap1","expectedGeneration":3}`); rec.Code != http.StatusOK {
		t.Fatalf("grow restore = %d", rec.Code)
	}
	if rec := restore(h, "vol1", `{"snapshotId":"snap1","expectedGeneration":4}`); rec.Code != http.StatusOK {
		t.Fatalf("same-size restore = %d", rec.Code)
	}
	wantError(t, restore(h, "vol1", `{"snapshotId":"snap1","expectedGeneration":9}`),
		http.StatusConflict, "stale_generation")

	v := getAuditEvents(t, h, "/v1/audit-events?poolId=vp")
	items := v["items"].([]any)
	var restored []map[string]any
	for _, it := range items {
		ev := it.(map[string]any)
		if ev["action"] == "volume.restored" {
			restored = append(restored, ev)
		}
	}
	if len(restored) != 3 {
		t.Fatalf("want exactly 3 volume.restored events, got %d: %v", len(restored), items)
	}
	wantDeltas := []float64{-200, 200, 0}
	for i, ev := range restored {
		if ev["resource"] != "/v1/volumes/vol1/restore" || ev["poolId"] != "vp" {
			t.Fatalf("event %d = %v", i, ev)
		}
		if ev["bytesDelta"].(float64) != wantDeltas[i] {
			t.Fatalf("event %d bytesDelta = %v, want %v", i, ev["bytesDelta"], wantDeltas[i])
		}
		if i > 0 && restored[i-1]["sequence"].(float64) >= ev["sequence"].(float64) {
			t.Fatalf("event ordering = %v", restored)
		}
	}
}

func TestRestoreUpdatesCapacityReports(t *testing.T) {
	h := Handler()
	createPool(t, h, volPool)
	createVol(t, h, `{"id":"vol1","poolId":"vp","sizeBytes":300}`)
	snapshotVol(t, h, "vol1", `{"id":"snap1","expectedGeneration":0}`)
	if rec := resize(h, "vol1", `{"sizeBytes":700,"expectedGeneration":0}`); rec.Code != http.StatusOK {
		t.Fatalf("resize = %d", rec.Code)
	}
	if rec := restore(h, "vol1", `{"snapshotId":"snap1","expectedGeneration":1}`); rec.Code != http.StatusOK {
		t.Fatalf("restore = %d", rec.Code)
	}

	// volumeBytes drops by the delta; snapshotBytes keeps its 300.
	rep := decodeBody(t, do(h, http.MethodGet, "/v1/capacity-report?poolId=vp", ""))
	row := rep["items"].([]any)[0].(map[string]any)
	if row["volumeBytes"].(float64) != 300 || row["snapshotBytes"].(float64) != 300 ||
		row["allocatedBytes"].(float64) != 600 || row["availableBytes"].(float64) != 400 {
		t.Fatalf("json report row = %v", row)
	}
	summary := rep["summary"].(map[string]any)
	if summary["volumeBytes"].(float64) != 300 || summary["allocatedBytes"].(float64) != 600 {
		t.Fatalf("json report summary = %v", summary)
	}

	csv := do(h, http.MethodGet, "/v1/capacity-report?format=csv&poolId=vp", "").Body.String()
	want := csvHeader + "\nvp,1000,0,300,300,0,600,400\n"
	if csv != want {
		t.Fatalf("csv report = %q want %q", csv, want)
	}
}

func TestRestoreRouting(t *testing.T) {
	h := Handler()
	createPool(t, h, volPool)
	createVol(t, h, `{"id":"vol1","poolId":"vp","sizeBytes":10}`)

	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		rec := do(h, method, "/v1/volumes/vol1/restore", `{"snapshotId":"s","expectedGeneration":0}`)
		if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "POST" {
			t.Fatalf("%s restore = %d allow=%q", method, rec.Code, rec.Header().Get("Allow"))
		}
	}
	wantError(t, do(h, http.MethodPost, "/v1/volumes/vol1/restore/extra", `{}`),
		http.StatusNotFound, "not_found")
}

func TestRestoreKeepsSnapshotAndCloneBehavior(t *testing.T) {
	h := Handler()
	createPool(t, h, `{"id":"rp","devices":[{"id":"dr","capacityBytes":2000,"faultDomain":"rack1"}]}`)
	createVol(t, h, `{"id":"vol1","poolId":"rp","sizeBytes":300}`)
	snapshotVol(t, h, "vol1", `{"id":"snap1","expectedGeneration":0}`)
	if rec := resize(h, "vol1", `{"sizeBytes":900,"expectedGeneration":0}`); rec.Code != http.StatusOK {
		t.Fatalf("resize = %d", rec.Code)
	}
	if rec := restore(h, "vol1", `{"snapshotId":"snap1","expectedGeneration":1}`); rec.Code != http.StatusOK {
		t.Fatalf("restore = %d", rec.Code)
	}

	// The snapshot is neither deleted nor re-charged: it still charges the
	// pool exactly once, and a clone from it still uses the snapshot size.
	p := decodeBody(t, do(h, http.MethodGet, "/v1/storage-pools/rp", ""))
	if p["allocatedBytes"].(float64) != 600 {
		t.Fatalf("pool after restore = %v", p)
	}
	createPool(t, h, `{"id":"pool-b","devices":[{"id":"db","capacityBytes":500,"faultDomain":"rack1"}]}`)
	if rec := do(h, http.MethodPost, "/v1/snapshots/snap1/clones", `{"id":"clone1","poolId":"pool-b"}`); rec.Code != http.StatusCreated {
		t.Fatalf("clone = %d %s", rec.Code, rec.Body.String())
	}
	clone := decodeBody(t, do(h, http.MethodGet, "/v1/volumes/clone1", ""))
	if clone["sizeBytes"].(float64) != 300 || clone["generation"].(float64) != 0 {
		t.Fatalf("clone view = %v", clone)
	}

	// Restoring does not disturb the create-idempotency replay of the volume.
	rec := do(h, http.MethodPost, "/v1/volumes", `{"id":"vol1","poolId":"rp","sizeBytes":300}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("create replay = %d %s", rec.Code, rec.Body.String())
	}
	v := decodeBody(t, rec)
	if v["sizeBytes"].(float64) != 300 || v["generation"].(float64) != 2 {
		t.Fatalf("replay view = %v", v)
	}
}

func TestRestoreConcurrentWithDeleteSerializes(t *testing.T) {
	// A restore racing a volume delete must be equivalent to some serial
	// order: either the restore commits first (delete then succeeds and the
	// volume is gone) or the delete commits first (restore then 404s).
	for i := 0; i < 20; i++ {
		h := Handler()
		createPool(t, h, volPool)
		createVol(t, h, `{"id":"vol1","poolId":"vp","sizeBytes":100}`)
		snapshotVol(t, h, "vol1", fmt.Sprintf(`{"id":"snap%d","expectedGeneration":0}`, i))

		var wg sync.WaitGroup
		wg.Add(2)
		var restoreCode, deleteCode int
		go func() {
			defer wg.Done()
			restoreCode = restore(h, "vol1", fmt.Sprintf(`{"snapshotId":"snap%d","expectedGeneration":0}`, i)).Code
		}()
		go func() {
			defer wg.Done()
			deleteCode = do(h, http.MethodDelete, "/v1/volumes/vol1", "").Code
		}()
		wg.Wait()

		if deleteCode != http.StatusNoContent {
			t.Fatalf("delete = %d", deleteCode)
		}
		if restoreCode != http.StatusOK && restoreCode != http.StatusNotFound {
			t.Fatalf("restore = %d, want 200 or 404", restoreCode)
		}
		// The volume is gone either way; the snapshot remains.
		wantError(t, do(h, http.MethodGet, "/v1/volumes/vol1", ""), http.StatusNotFound, "volume_not_found")
		p := decodeBody(t, do(h, http.MethodGet, "/v1/storage-pools/vp", ""))
		if p["allocatedBytes"].(float64) != 100 {
			t.Fatalf("pool after race = %v", p)
		}
	}
}

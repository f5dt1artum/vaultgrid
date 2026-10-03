package server

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func restore(h http.Handler, id, body string) *httptest.ResponseRecorder {
	return do(h, http.MethodPost, "/v1/volumes/"+id+"/restore", body)
}

func snapshotVol(t *testing.T, h http.Handler, volumeID, body string) {
	t.Helper()
	if rec := do(h, http.MethodPost, "/v1/volumes/"+volumeID+"/snapshots", body); rec.Code != http.StatusCreated {
		t.Fatalf("snapshot = %d %s", rec.Code, rec.Body.String())
	}
}

func TestRestoreVolumeShrinkGrowAndSameSize(t *testing.T) {
	h := Handler()
	createPool(t, h, volPool)
	createVol(t, h, `{"id":"vol1","poolId":"vp","sizeBytes":300}`)
	snapshotVol(t, h, "vol1", `{"id":"snap1","expectedGeneration":0}`)

	// Grow the volume past the snapshot, then restore: size returns to the
	// snapshot's 300, generation bumps, binding stays null.
	if rec := resize(h, "vol1", `{"sizeBytes":700,"expectedGeneration":0}`); rec.Code != http.StatusOK {
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
		t.Fatalf("restore view = %v", v)
	}
	if binding, ok := v["binding"]; !ok || binding != nil {
		t.Fatalf("binding after restore = %v", binding)
	}
	// Pool holds the restored volume (300) plus the kept snapshot (300).
	p := decodeBody(t, do(h, http.MethodGet, "/v1/storage-pools/vp", ""))
	if p["allocatedBytes"].(float64) != 600 || p["availableBytes"].(float64) != 400 {
		t.Fatalf("pool after restore = %v", p)
	}

	// Restoring again to the same size still commits: the generation bumps
	// even though the size does not change.
	rec = restore(h, "vol1", `{"snapshotId":"snap1","expectedGeneration":2}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("same-size restore status = %d %s", rec.Code, rec.Body.String())
	}
	v = decodeBody(t, rec)
	if v["sizeBytes"].(float64) != 300 || v["generation"].(float64) != 3 {
		t.Fatalf("same-size restore view = %v", v)
	}

	// Shrink below the snapshot, then restore grows the volume back.
	if rec := resize(h, "vol1", `{"sizeBytes":100,"expectedGeneration":3}`); rec.Code != http.StatusOK {
		t.Fatalf("shrink = %d %s", rec.Code, rec.Body.String())
	}
	rec = restore(h, "vol1", `{"snapshotId":"snap1","expectedGeneration":4}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("restore grow status = %d %s", rec.Code, rec.Body.String())
	}
	v = decodeBody(t, rec)
	if v["sizeBytes"].(float64) != 300 || v["generation"].(float64) != 5 {
		t.Fatalf("restore grow view = %v", v)
	}
	p = decodeBody(t, do(h, http.MethodGet, "/v1/storage-pools/vp", ""))
	if p["allocatedBytes"].(float64) != 600 {
		t.Fatalf("pool after restore grow = %v", p)
	}

	// The snapshot survives every restore, unchanged and still charging.
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
		"empty snapshot":      `{"snapshotId":"","expectedGeneration":0}`,
		"bad snapshot id":     `{"snapshotId":"bad id","expectedGeneration":0}`,
		"numeric snapshot":    `{"snapshotId":7,"expectedGeneration":0}`,
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

	// Unknown volume and unknown snapshot: 404 each.
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

	// A snapshot of vol1 cannot restore vol2.
	wantError(t, restore(h, "vol2", `{"snapshotId":"snap1","expectedGeneration":0}`),
		http.StatusConflict, "snapshot_source_mismatch")

	// Delete vol1 and recreate it under the same id: the old snapshot is not
	// part of the new instance's history.
	if rec := do(h, http.MethodDelete, "/v1/volumes/vol1", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d", rec.Code)
	}
	createVol(t, h, `{"id":"vol1","poolId":"vp","sizeBytes":100}`)
	wantError(t, restore(h, "vol1", `{"snapshotId":"snap1","expectedGeneration":0}`),
		http.StatusConflict, "snapshot_source_mismatch")

	// A snapshot of the new instance restores it fine.
	snapshotVol(t, h, "vol1", `{"id":"snap2","expectedGeneration":0}`)
	if rec := resize(h, "vol1", `{"sizeBytes":50,"expectedGeneration":0}`); rec.Code != http.StatusOK {
		t.Fatalf("resize = %d %s", rec.Code, rec.Body.String())
	}
	rec := restore(h, "vol1", `{"snapshotId":"snap2","expectedGeneration":1}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("restore new instance = %d %s", rec.Code, rec.Body.String())
	}
	if v := decodeBody(t, rec); v["sizeBytes"].(float64) != 100 || v["generation"].(float64) != 2 {
		t.Fatalf("new instance restore view = %v", v)
	}

	// A deleted snapshot is snapshot_not_found, not a mismatch.
	if rec := do(h, http.MethodDelete, "/v1/snapshots/snap2", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete snapshot = %d", rec.Code)
	}
	wantError(t, restore(h, "vol1", `{"snapshotId":"snap2","expectedGeneration":2}`),
		http.StatusNotFound, "snapshot_not_found")
}

func TestRestoreQuotaAndCapacityOrdering(t *testing.T) {
	h := Handler()
	createPool(t, h, `{"id":"qp","devices":[{"id":"d","capacityBytes":5000,"faultDomain":"rack1"}]}`)
	putQuota(t, h, "qp", "team-a", 1000)
	if rec := doTenant(h, http.MethodPost, "/v1/volumes",
		`{"id":"vol1","poolId":"qp","sizeBytes":300}`, "team-a"); rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
	}
	// Snapshot the 300-byte state: tenant usage and pool allocation reach 600.
	snapshotVol(t, h, "vol1", `{"id":"snap1","expectedGeneration":0}`)
	// Shrink the volume to 10: usage 310, allocated 310.
	if rec := doTenant(h, http.MethodPut, "/v1/volumes/vol1/size",
		`{"sizeBytes":10,"expectedGeneration":0}`, "team-a"); rec.Code != http.StatusOK {
		t.Fatalf("shrink = %d %s", rec.Code, rec.Body.String())
	}
	// Fill the pool so a 290-byte restore growth also exceeds capacity.
	if rec := reserve(t, h, "qp", `{"requestId":"r1","bytes":4410}`); rec.Code != http.StatusCreated {
		t.Fatalf("reserve = %d %s", rec.Code, rec.Body.String())
	}

	// Both limits would fail (usage 310+290 > 400, allocated 4720+290 > 5000);
	// the tenant quota is checked first.
	putQuota(t, h, "qp", "team-a", 400)
	wantError(t, doTenant(h, http.MethodPost, "/v1/volumes/vol1/restore",
		`{"snapshotId":"snap1","expectedGeneration":1}`, "team-a"),
		http.StatusConflict, "tenant_quota_exceeded")

	// Quota permits the growth but the pool does not: capacity error.
	putQuota(t, h, "qp", "team-a", 1000)
	wantError(t, doTenant(h, http.MethodPost, "/v1/volumes/vol1/restore",
		`{"snapshotId":"snap1","expectedGeneration":1}`, "team-a"),
		http.StatusConflict, "insufficient_capacity")

	// Free the reservation: the restore now fits and commits.
	if rec := do(h, http.MethodDelete, "/v1/storage-pools/qp/reservations/r1", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("release = %d", rec.Code)
	}
	rec := doTenant(h, http.MethodPost, "/v1/volumes/vol1/restore",
		`{"snapshotId":"snap1","expectedGeneration":1}`, "team-a")
	if rec.Code != http.StatusOK {
		t.Fatalf("restore = %d %s", rec.Code, rec.Body.String())
	}
	v := decodeBody(t, rec)
	if v["sizeBytes"].(float64) != 300 || v["generation"].(float64) != 2 {
		t.Fatalf("restore view = %v", v)
	}
	q := decodeBody(t, do(h, http.MethodGet, "/v1/storage-pools/qp/tenant-quotas/team-a", ""))
	if q["usedBytes"].(float64) != 600 || q["availableBytes"].(float64) != 400 {
		t.Fatalf("quota after restore = %v", q)
	}

	// A shrinking restore is subject to neither limit. Snapshot the 300-byte
	// state, grow the volume to 4000, then pin the quota at current usage and
	// fill the pool: the restore back to 300 still succeeds.
	snapshotVol(t, h, "vol1", `{"id":"snap2","expectedGeneration":2}`)
	putQuota(t, h, "qp", "team-a", 10000)
	if rec := resize(h, "vol1", `{"sizeBytes":4000,"expectedGeneration":2}`); rec.Code != http.StatusOK {
		t.Fatalf("grow = %d %s", rec.Code, rec.Body.String())
	}
	putQuota(t, h, "qp", "team-a", 4600) // exactly current usage: 4000 + 300 + 300
	if rec := reserve(t, h, "qp", `{"requestId":"r2","bytes":100}`); rec.Code != http.StatusCreated {
		t.Fatalf("reserve r2 = %d %s", rec.Code, rec.Body.String())
	}
	rec = doTenant(h, http.MethodPost, "/v1/volumes/vol1/restore",
		`{"snapshotId":"snap2","expectedGeneration":3}`, "team-a")
	if rec.Code != http.StatusOK {
		t.Fatalf("shrinking restore = %d %s", rec.Code, rec.Body.String())
	}
	v = decodeBody(t, rec)
	if v["sizeBytes"].(float64) != 300 || v["generation"].(float64) != 4 {
		t.Fatalf("shrinking restore view = %v", v)
	}
	q = decodeBody(t, do(h, http.MethodGet, "/v1/storage-pools/qp/tenant-quotas/team-a", ""))
	if q["usedBytes"].(float64) != 900 {
		t.Fatalf("quota after shrinking restore = %v", q)
	}
	p := decodeBody(t, do(h, http.MethodGet, "/v1/storage-pools/qp", ""))
	if p["allocatedBytes"].(float64) != 1000 || p["availableBytes"].(float64) != 4000 {
		t.Fatalf("pool after shrinking restore = %v", p)
	}
}

func TestRestoreFailuresLeaveNoStateOrAudit(t *testing.T) {
	h := Handler()
	createPool(t, h, volPool)
	createVol(t, h, `{"id":"vol1","poolId":"vp","sizeBytes":100}`)
	snapshotVol(t, h, "vol1", `{"id":"snap1","expectedGeneration":0}`)

	before := getAuditEvents(t, h, "/v1/audit-events?poolId=vp")
	wantError(t, restore(h, "vol1", `{"snapshotId":"snap1","expectedGeneration":9}`),
		http.StatusConflict, "stale_generation")
	wantError(t, restore(h, "vol1", `{"snapshotId":"nope","expectedGeneration":0}`),
		http.StatusNotFound, "snapshot_not_found")

	v := decodeBody(t, do(h, http.MethodGet, "/v1/volumes/vol1", ""))
	if v["sizeBytes"].(float64) != 100 || v["generation"].(float64) != 0 {
		t.Fatalf("volume changed on failed restore: %v", v)
	}
	p := decodeBody(t, do(h, http.MethodGet, "/v1/storage-pools/vp", ""))
	if p["allocatedBytes"].(float64) != 200 || p["availableBytes"].(float64) != 800 {
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
	if rec := resize(h, "vol1", `{"sizeBytes":150,"expectedGeneration":0}`); rec.Code != http.StatusOK {
		t.Fatalf("resize = %d", rec.Code)
	}

	const goroutines = 30
	var wg sync.WaitGroup
	statuses := make(chan int, goroutines)
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
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
	// Pool accounting matches exactly one restore: volume 100 + snapshot 100.
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

	// Shrink-restore (delta -400), same-size restore (delta 0), grow-restore
	// (delta +200): each commits an event, the same-size one included.
	if rec := resize(h, "vol1", `{"sizeBytes":700,"expectedGeneration":0}`); rec.Code != http.StatusOK {
		t.Fatalf("grow = %d", rec.Code)
	}
	if rec := restore(h, "vol1", `{"snapshotId":"snap1","expectedGeneration":1}`); rec.Code != http.StatusOK {
		t.Fatalf("shrink restore = %d", rec.Code)
	}
	if rec := restore(h, "vol1", `{"snapshotId":"snap1","expectedGeneration":2}`); rec.Code != http.StatusOK {
		t.Fatalf("same-size restore = %d", rec.Code)
	}
	if rec := resize(h, "vol1", `{"sizeBytes":100,"expectedGeneration":3}`); rec.Code != http.StatusOK {
		t.Fatalf("shrink = %d", rec.Code)
	}
	if rec := restore(h, "vol1", `{"snapshotId":"snap1","expectedGeneration":4}`); rec.Code != http.StatusOK {
		t.Fatalf("grow restore = %d", rec.Code)
	}
	// A failed restore appends nothing.
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
	wantDeltas := []float64{-400, 0, 200}
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

	rep := decodeBody(t, do(h, http.MethodGet, "/v1/capacity-report?poolId=vp", ""))
	items := rep["items"].([]any)
	row := items[0].(map[string]any)
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

// Restore never disturbs the create-idempotency contract: replaying the
// original creation after a restore still returns the current representation.
func TestRestoreCreateIdempotentReplay(t *testing.T) {
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

	rec := do(h, http.MethodPost, "/v1/volumes", `{"id":"vol1","poolId":"vp","sizeBytes":300}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("create replay = %d %s", rec.Code, rec.Body.String())
	}
	v := decodeBody(t, rec)
	if v["sizeBytes"].(float64) != 300 || v["generation"].(float64) != 2 {
		t.Fatalf("replay view = %v", v)
	}
	p := decodeBody(t, do(h, http.MethodGet, "/v1/storage-pools/vp", ""))
	if p["allocatedBytes"].(float64) != 600 {
		t.Fatalf("pool after replay = %v", p)
	}
}

// A restore racing a snapshot delete is equivalent to some serial order:
// either the restore commits first (200) and the delete then succeeds, or the
// delete commits first and the restore reports snapshot_not_found.
func TestConcurrentRestoreAndSnapshotDelete(t *testing.T) {
	for trial := 0; trial < 20; trial++ {
		h := Handler()
		createPool(t, h, volPool)
		createVol(t, h, `{"id":"vol1","poolId":"vp","sizeBytes":100}`)
		snapshotVol(t, h, "vol1", `{"id":"snap1","expectedGeneration":0}`)
		if rec := resize(h, "vol1", `{"sizeBytes":150,"expectedGeneration":0}`); rec.Code != http.StatusOK {
			t.Fatalf("resize = %d", rec.Code)
		}

		var wg sync.WaitGroup
		results := make(chan int, 2)
		wg.Add(2)
		go func() {
			defer wg.Done()
			results <- restore(h, "vol1", `{"snapshotId":"snap1","expectedGeneration":1}`).Code
		}()
		go func() {
			defer wg.Done()
			results <- do(h, http.MethodDelete, "/v1/snapshots/snap1", "").Code
		}()
		wg.Wait()
		close(results)
		var restoreOK, restoreGone, deleted int
		for code := range results {
			switch code {
			case http.StatusOK:
				restoreOK++
			case http.StatusNotFound:
				restoreGone++
			case http.StatusNoContent:
				deleted++
			default:
				t.Fatalf("unexpected status %d", code)
			}
		}
		if deleted != 1 || restoreOK+restoreGone != 1 {
			t.Fatalf("restoreOK=%d restoreGone=%d deleted=%d", restoreOK, restoreGone, deleted)
		}
		// Whatever the order, the snapshot is gone and the state is consistent.
		wantError(t, do(h, http.MethodGet, "/v1/snapshots/snap1", ""), http.StatusNotFound, "snapshot_not_found")
		v := decodeBody(t, do(h, http.MethodGet, "/v1/volumes/vol1", ""))
		p := decodeBody(t, do(h, http.MethodGet, "/v1/storage-pools/vp", ""))
		if restoreOK == 1 {
			if v["sizeBytes"].(float64) != 100 || v["generation"].(float64) != 2 {
				t.Fatalf("volume after winning restore = %v", v)
			}
			if p["allocatedBytes"].(float64) != 100 {
				t.Fatalf("pool after winning restore = %v", p)
			}
		} else {
			if v["sizeBytes"].(float64) != 150 || v["generation"].(float64) != 1 {
				t.Fatalf("volume after losing restore = %v", v)
			}
			if p["allocatedBytes"].(float64) != 150 {
				t.Fatalf("pool after losing restore = %v", p)
			}
		}
	}
}

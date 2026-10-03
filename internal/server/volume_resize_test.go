package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func resize(h http.Handler, id, body string) *httptest.ResponseRecorder {
	return do(h, http.MethodPut, "/v1/volumes/"+id+"/size", body)
}

func TestResizeVolumeGrowShrinkAndNoOp(t *testing.T) {
	h := Handler()
	createPool(t, h, volPool)
	createVol(t, h, `{"id":"vol1","poolId":"vp","sizeBytes":300}`)

	// Grow at generation 0: 200, size updated, generation bumps to 1.
	rec := resize(h, "vol1", `{"sizeBytes":900,"expectedGeneration":0}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("grow status = %d %s", rec.Code, rec.Body.String())
	}
	v := decodeBody(t, rec)
	if v["id"] != "vol1" || v["poolId"] != "vp" {
		t.Fatalf("identity changed: %v", v)
	}
	if v["sizeBytes"].(float64) != 900 || v["generation"].(float64) != 1 {
		t.Fatalf("grow view = %v", v)
	}
	if binding, ok := v["binding"]; !ok || binding != nil {
		t.Fatalf("binding after grow = %v", binding)
	}
	p := decodeBody(t, do(h, http.MethodGet, "/v1/storage-pools/vp", ""))
	if p["allocatedBytes"].(float64) != 900 || p["availableBytes"].(float64) != 100 {
		t.Fatalf("pool after grow = %v", p)
	}

	// Same size, matching generation: idempotent 200 without a generation bump.
	rec = resize(h, "vol1", `{"sizeBytes":900,"expectedGeneration":1}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("same-size status = %d %s", rec.Code, rec.Body.String())
	}
	v = decodeBody(t, rec)
	if v["sizeBytes"].(float64) != 900 || v["generation"].(float64) != 1 {
		t.Fatalf("same-size view = %v", v)
	}
	p = decodeBody(t, do(h, http.MethodGet, "/v1/storage-pools/vp", ""))
	if p["allocatedBytes"].(float64) != 900 {
		t.Fatalf("accounting changed on same-size request: %v", p)
	}

	// Shrink at generation 1: 200, generation bumps to 2, capacity freed.
	rec = resize(h, "vol1", `{"sizeBytes":100,"expectedGeneration":1}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("shrink status = %d %s", rec.Code, rec.Body.String())
	}
	v = decodeBody(t, rec)
	if v["sizeBytes"].(float64) != 100 || v["generation"].(float64) != 2 {
		t.Fatalf("shrink view = %v", v)
	}
	p = decodeBody(t, do(h, http.MethodGet, "/v1/storage-pools/vp", ""))
	if p["allocatedBytes"].(float64) != 100 || p["availableBytes"].(float64) != 900 {
		t.Fatalf("pool after shrink = %v", p)
	}
}

func TestResizeVolumeValidation(t *testing.T) {
	h := Handler()
	createPool(t, h, volPool)
	createVol(t, h, `{"id":"vol1","poolId":"vp","sizeBytes":10}`)

	for name, body := range map[string]string{
		"unknown field":       `{"sizeBytes":20,"expectedGeneration":0,"extra":1}`,
		"missing size":        `{"expectedGeneration":0}`,
		"missing generation":  `{"sizeBytes":20}`,
		"null generation":     `{"sizeBytes":20,"expectedGeneration":null}`,
		"zero size":           `{"sizeBytes":0,"expectedGeneration":0}`,
		"negative size":       `{"sizeBytes":-1,"expectedGeneration":0}`,
		"float size":          `{"sizeBytes":20.5,"expectedGeneration":0}`,
		"string size":         `{"sizeBytes":"20","expectedGeneration":0}`,
		"negative generation": `{"sizeBytes":20,"expectedGeneration":-1}`,
		"float generation":    `{"sizeBytes":20,"expectedGeneration":0.5}`,
		"string generation":   `{"sizeBytes":20,"expectedGeneration":"0"}`,
		"malformed json":      `{`,
	} {
		t.Run(name, func(t *testing.T) {
			wantError(t, resize(h, "vol1", body), http.StatusBadRequest, "invalid_request")
		})
	}

	// Illegal volume id and any query string are invalid_request.
	wantError(t, do(h, http.MethodPut, "/v1/volumes/bad%20id/size", `{"sizeBytes":20,"expectedGeneration":0}`),
		http.StatusBadRequest, "invalid_request")
	for _, target := range []string{
		"/v1/volumes/vol1/size?",
		"/v1/volumes/vol1/size?expectedGeneration=0",
		"/v1/volumes/vol1/size?sizeBytes=20",
		"/v1/volumes/vol1/size?x=",
	} {
		wantError(t, do(h, http.MethodPut, target, `{"sizeBytes":20,"expectedGeneration":0}`),
			http.StatusBadRequest, "invalid_request")
	}

	// Unknown volume: 404. Bad id is still 400 even with a valid body.
	wantError(t, resize(h, "nope", `{"sizeBytes":20,"expectedGeneration":0}`),
		http.StatusNotFound, "volume_not_found")

	// No state changed by any failed request.
	v := decodeBody(t, do(h, http.MethodGet, "/v1/volumes/vol1", ""))
	if v["sizeBytes"].(float64) != 10 || v["generation"].(float64) != 0 {
		t.Fatalf("partial resize state: %v", v)
	}
}

func TestResizeVolumeBindingAndGenerationPrecedence(t *testing.T) {
	h := Handler()
	createPool(t, h, volPool)
	createVol(t, h, `{"id":"vol1","poolId":"vp","sizeBytes":10}`)

	// Stale generation on an unbound volume.
	wantError(t, resize(h, "vol1", `{"sizeBytes":20,"expectedGeneration":5}`),
		http.StatusConflict, "stale_generation")

	// Bind: generation moves 0 -> 1.
	if rec := do(h, http.MethodPut, "/v1/volumes/vol1/binding", `{"nodeId":"n1","expectedGeneration":0}`); rec.Code != http.StatusOK {
		t.Fatalf("bind = %d %s", rec.Code, rec.Body.String())
	}

	// A bound volume is rejected with volume_in_use regardless of whether the
	// generation matches: binding takes precedence over the generation check.
	wantError(t, resize(h, "vol1", `{"sizeBytes":20,"expectedGeneration":0}`),
		http.StatusConflict, "volume_in_use")
	wantError(t, resize(h, "vol1", `{"sizeBytes":20,"expectedGeneration":1}`),
		http.StatusConflict, "volume_in_use")

	// Unbind: generation 1 -> 2; resize is accepted at the new generation.
	if rec := do(h, http.MethodDelete, "/v1/volumes/vol1/binding?expectedGeneration=1", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("unbind = %d", rec.Code)
	}
	wantError(t, resize(h, "vol1", `{"sizeBytes":20,"expectedGeneration":1}`),
		http.StatusConflict, "stale_generation")
	rec := resize(h, "vol1", `{"sizeBytes":20,"expectedGeneration":2}`)
	if rec.Code != http.StatusOK || decodeBody(t, rec)["generation"].(float64) != 3 {
		t.Fatalf("resize after unbind = %d %s", rec.Code, rec.Body.String())
	}
}

func TestResizeQuotaAndCapacityOrdering(t *testing.T) {
	h := Handler()
	createPool(t, h, `{"id":"qp","devices":[{"id":"d","capacityBytes":600,"faultDomain":"rack1"}]}`)
	// Tenant team-a gets a tight 300-byte quota.
	putQuota(t, h, "qp", "team-a", 300)
	if rec := doTenant(h, http.MethodPost, "/v1/volumes",
		`{"id":"vol1","poolId":"qp","sizeBytes":100}`, "team-a"); rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
	}

	// Grow to 700: both quota (100+600 > 300) and capacity (700 > 600) would
	// fail; quota is checked first.
	wantError(t, doTenant(h, http.MethodPut, "/v1/volumes/vol1/size",
		`{"sizeBytes":700,"expectedGeneration":0}`, "team-a"),
		http.StatusConflict, "tenant_quota_exceeded")

	// Grow to 500: quota alone fails (500 > 300), capacity would fit.
	wantError(t, doTenant(h, http.MethodPut, "/v1/volumes/vol1/size",
		`{"sizeBytes":500,"expectedGeneration":0}`, "team-a"),
		http.StatusConflict, "tenant_quota_exceeded")

	// Grow to exactly the quota limit succeeds and bumps the tenant usage.
	rec := doTenant(h, http.MethodPut, "/v1/volumes/vol1/size",
		`{"sizeBytes":300,"expectedGeneration":0}`, "team-a")
	if rec.Code != http.StatusOK {
		t.Fatalf("grow to quota = %d %s", rec.Code, rec.Body.String())
	}
	q := decodeBody(t, do(h, http.MethodGet, "/v1/storage-pools/qp/tenant-quotas/team-a", ""))
	if q["usedBytes"].(float64) != 300 || q["availableBytes"].(float64) != 0 {
		t.Fatalf("quota after grow = %v", q)
	}

	// Quota permits this grow but the pool does not: capacity error. Raise the
	// quota first so only the pool limit can reject it.
	putQuota(t, h, "qp", "team-a", 1000)
	wantError(t, doTenant(h, http.MethodPut, "/v1/volumes/vol1/size",
		`{"sizeBytes":601,"expectedGeneration":1}`, "team-a"),
		http.StatusConflict, "insufficient_capacity")

	// Exactly the remaining pool capacity fits.
	rec = doTenant(h, http.MethodPut, "/v1/volumes/vol1/size",
		`{"sizeBytes":600,"expectedGeneration":1}`, "team-a")
	if rec.Code != http.StatusOK {
		t.Fatalf("grow to capacity = %d %s", rec.Code, rec.Body.String())
	}
	wantError(t, doTenant(h, http.MethodPut, "/v1/volumes/vol1/size",
		`{"sizeBytes":601,"expectedGeneration":2}`, "team-a"),
		http.StatusConflict, "insufficient_capacity")

	// Shrink ignores both limits and frees quota and pool usage immediately.
	rec = doTenant(h, http.MethodPut, "/v1/volumes/vol1/size",
		`{"sizeBytes":10,"expectedGeneration":2}`, "team-a")
	if rec.Code != http.StatusOK {
		t.Fatalf("shrink = %d %s", rec.Code, rec.Body.String())
	}
	p := decodeBody(t, do(h, http.MethodGet, "/v1/storage-pools/qp", ""))
	if p["allocatedBytes"].(float64) != 10 || p["availableBytes"].(float64) != 590 {
		t.Fatalf("pool after shrink = %v", p)
	}
	q = decodeBody(t, do(h, http.MethodGet, "/v1/storage-pools/qp/tenant-quotas/team-a", ""))
	if q["usedBytes"].(float64) != 10 || q["availableBytes"].(float64) != 990 {
		t.Fatalf("quota after shrink = %v", q)
	}

	// Failed grow left no trace: the volume was still at 600/gen 2 before the
	// successful shrink, and after it nothing else occupies the pool.
}

func TestResizeFailuresLeaveNoStateOrAudit(t *testing.T) {
	h := Handler()
	createPool(t, h, volPool)
	createVol(t, h, `{"id":"vol1","poolId":"vp","sizeBytes":900}`)

	before := getAuditEvents(t, h, "/v1/audit-events?poolId=vp")
	wantError(t, resize(h, "vol1", `{"sizeBytes":1001,"expectedGeneration":0}`),
		http.StatusConflict, "insufficient_capacity")
	wantError(t, resize(h, "vol1", `{"sizeBytes":1001,"expectedGeneration":9}`),
		http.StatusConflict, "stale_generation")

	v := decodeBody(t, do(h, http.MethodGet, "/v1/volumes/vol1", ""))
	if v["sizeBytes"].(float64) != 900 || v["generation"].(float64) != 0 {
		t.Fatalf("volume changed on failed resize: %v", v)
	}
	p := decodeBody(t, do(h, http.MethodGet, "/v1/storage-pools/vp", ""))
	if p["allocatedBytes"].(float64) != 900 || p["availableBytes"].(float64) != 100 {
		t.Fatalf("pool changed on failed resize: %v", p)
	}
	after := getAuditEvents(t, h, "/v1/audit-events?poolId=vp")
	if len(after["items"].([]any)) != len(before["items"].([]any)) {
		t.Fatalf("audit events changed on failure: before=%v after=%v", before, after)
	}
}

func TestConcurrentResizeOnlyOneWins(t *testing.T) {
	h := Handler()
	createPool(t, h, volPool)
	createVol(t, h, `{"id":"vol1","poolId":"vp","sizeBytes":100}`)

	const goroutines = 30
	var wg sync.WaitGroup
	statuses := make(chan int, goroutines)
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(i int) {
			defer wg.Done()
			// Every request uses the same current generation but distinct
			// target sizes; capacity still admits every individual grow.
			body := fmt.Sprintf(`{"sizeBytes":%d,"expectedGeneration":0}`, 200+i)
			statuses <- do(h, http.MethodPut, "/v1/volumes/vol1/size", body).Code
		}(i)
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
	if v["generation"].(float64) != 1 {
		t.Fatalf("generation after concurrent resize = %v", v["generation"])
	}
	// Pool accounting matches exactly one grow to a size within [200,229].
	p := decodeBody(t, do(h, http.MethodGet, "/v1/storage-pools/vp", ""))
	allocated := p["allocatedBytes"].(float64)
	if allocated != v["sizeBytes"].(float64) || allocated < 200 || allocated > 229 {
		t.Fatalf("inconsistent accounting: pool=%v volume=%v", allocated, v["sizeBytes"])
	}
}

func TestResizeDoesNotAffectSnapshotsOrClones(t *testing.T) {
	h := Handler()
	createPool(t, h, `{"id":"rp","devices":[{"id":"dr","capacityBytes":2000,"faultDomain":"rack1"}]}`)
	createVolIn := `{"id":"vol1","poolId":"rp","sizeBytes":300}`
	if rec := do(h, http.MethodPost, "/v1/volumes", createVolIn); rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
	}

	// Snapshot taken at generation 0, size 300, charging the source pool.
	snapBody := `{"id":"snap1","expectedGeneration":0}`
	if rec := do(h, http.MethodPost, "/v1/volumes/vol1/snapshots", snapBody); rec.Code != http.StatusCreated {
		t.Fatalf("snapshot = %d %s", rec.Code, rec.Body.String())
	}

	// Grow the source to 900.
	if rec := resize(h, "vol1", `{"sizeBytes":900,"expectedGeneration":0}`); rec.Code != http.StatusOK {
		t.Fatalf("resize = %d %s", rec.Code, rec.Body.String())
	}

	// The snapshot keeps its creation-time size and source generation.
	snap := decodeBody(t, do(h, http.MethodGet, "/v1/snapshots/snap1", ""))
	if snap["sizeBytes"].(float64) != 300 || snap["sourceGeneration"].(float64) != 0 {
		t.Fatalf("snapshot mutated by resize: %v", snap)
	}
	// Pool holds the grown volume (900) plus the untouched snapshot (300).
	p := decodeBody(t, do(h, http.MethodGet, "/v1/storage-pools/rp", ""))
	if p["allocatedBytes"].(float64) != 1200 || p["availableBytes"].(float64) != 800 {
		t.Fatalf("pool = %v", p)
	}

	// A clone of the snapshot still uses the snapshot size (300), not the
	// source's current 900.
	createPool(t, h, `{"id":"pool-b","devices":[{"id":"db","capacityBytes":500,"faultDomain":"rack1"}]}`)
	if rec := do(h, http.MethodPost, "/v1/snapshots/snap1/clones", `{"id":"clone1","poolId":"pool-b"}`); rec.Code != http.StatusCreated {
		t.Fatalf("clone = %d %s", rec.Code, rec.Body.String())
	}
	clone := decodeBody(t, do(h, http.MethodGet, "/v1/volumes/clone1", ""))
	if clone["sizeBytes"].(float64) != 300 || clone["generation"].(float64) != 0 || clone["binding"] != nil {
		t.Fatalf("clone view = %v", clone)
	}

	// The clone is itself resizable.
	rec := resize(h, "clone1", `{"sizeBytes":400,"expectedGeneration":0}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("resize clone = %d %s", rec.Code, rec.Body.String())
	}
	clone = decodeBody(t, rec)
	if clone["sizeBytes"].(float64) != 400 || clone["generation"].(float64) != 1 {
		t.Fatalf("clone after resize = %v", clone)
	}

	// Replaying the original clone creation still returns 200 and the current
	// representation; other parameters remain 409 volume_exists.
	rec = do(h, http.MethodPost, "/v1/snapshots/snap1/clones", `{"id":"clone1","poolId":"pool-b"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("clone replay = %d %s", rec.Code, rec.Body.String())
	}
	replay := decodeBody(t, rec)
	if replay["sizeBytes"].(float64) != 400 || replay["generation"].(float64) != 1 {
		t.Fatalf("clone replay view = %v", replay)
	}
	wantError(t, do(h, http.MethodPost, "/v1/snapshots/snap1/clones", `{"id":"clone1","poolId":"vp"}`),
		http.StatusConflict, "volume_exists")

	// The snapshot itself still reports 300 and charges pool-b's clone only in
	// pool-b; pool-b allocation reflects the resized clone (400).
	pb := decodeBody(t, do(h, http.MethodGet, "/v1/storage-pools/pool-b", ""))
	if pb["allocatedBytes"].(float64) != 400 {
		t.Fatalf("pool-b = %v", pb)
	}
}

func TestResizeCreateIdempotentReplay(t *testing.T) {
	h := Handler()
	createPool(t, h, volPool)
	createVol(t, h, `{"id":"vol1","poolId":"vp","sizeBytes":300}`)
	if rec := resize(h, "vol1", `{"sizeBytes":900,"expectedGeneration":0}`); rec.Code != http.StatusOK {
		t.Fatalf("resize = %d %s", rec.Code, rec.Body.String())
	}

	// Original creation parameters replay as 200 with the current view.
	rec := do(h, http.MethodPost, "/v1/volumes", `{"id":"vol1","poolId":"vp","sizeBytes":300}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("create replay = %d %s", rec.Code, rec.Body.String())
	}
	v := decodeBody(t, rec)
	if v["sizeBytes"].(float64) != 900 || v["generation"].(float64) != 1 {
		t.Fatalf("replay view = %v", v)
	}
	// No double counting.
	p := decodeBody(t, do(h, http.MethodGet, "/v1/storage-pools/vp", ""))
	if p["allocatedBytes"].(float64) != 900 {
		t.Fatalf("pool after replay = %v", p)
	}

	// Different parameters remain 409 volume_exists.
	wantError(t, do(h, http.MethodPost, "/v1/volumes", `{"id":"vol1","poolId":"vp","sizeBytes":301}`),
		http.StatusConflict, "volume_exists")
}

func TestResizeAuditEvent(t *testing.T) {
	h := Handler()
	createPool(t, h, volPool)
	createVol(t, h, `{"id":"vol1","poolId":"vp","sizeBytes":300}`)

	if rec := resize(h, "vol1", `{"sizeBytes":900,"expectedGeneration":0}`); rec.Code != http.StatusOK {
		t.Fatalf("grow = %d", rec.Code)
	}
	if rec := resize(h, "vol1", `{"sizeBytes":900,"expectedGeneration":1}`); rec.Code != http.StatusOK {
		t.Fatalf("no-op = %d", rec.Code)
	}
	if rec := resize(h, "vol1", `{"sizeBytes":100,"expectedGeneration":1}`); rec.Code != http.StatusOK {
		t.Fatalf("shrink = %d", rec.Code)
	}
	// A failed resize appends nothing.
	wantError(t, resize(h, "vol1", `{"sizeBytes":101,"expectedGeneration":9}`),
		http.StatusConflict, "stale_generation")

	v := getAuditEvents(t, h, "/v1/audit-events?poolId=vp")
	items := v["items"].([]any)
	var resized []map[string]any
	for _, it := range items {
		ev := it.(map[string]any)
		if ev["action"] == "volume.resized" {
			resized = append(resized, ev)
		}
	}
	if len(resized) != 2 {
		t.Fatalf("want exactly 2 volume.resized events, got %d: %v", len(resized), items)
	}
	grow, shrink := resized[0], resized[1]
	if grow["resource"] != "/v1/volumes/vol1/size" || grow["poolId"] != "vp" || grow["bytesDelta"].(float64) != 600 {
		t.Fatalf("grow event = %v", grow)
	}
	if shrink["resource"] != "/v1/volumes/vol1/size" || shrink["poolId"] != "vp" || shrink["bytesDelta"].(float64) != -800 {
		t.Fatalf("shrink event = %v", shrink)
	}
	if grow["sequence"].(float64) >= shrink["sequence"].(float64) {
		t.Fatalf("event ordering = %v", resized)
	}
}

func TestResizeUpdatesCapacityReports(t *testing.T) {
	h := Handler()
	createPool(t, h, volPool)
	createVol(t, h, `{"id":"vol1","poolId":"vp","sizeBytes":300}`)
	if rec := resize(h, "vol1", `{"sizeBytes":700,"expectedGeneration":0}`); rec.Code != http.StatusOK {
		t.Fatalf("resize = %d", rec.Code)
	}

	rep := decodeBody(t, do(h, http.MethodGet, "/v1/capacity-report?poolId=vp", ""))
	items := rep["items"].([]any)
	row := items[0].(map[string]any)
	if row["volumeBytes"].(float64) != 700 || row["allocatedBytes"].(float64) != 700 || row["availableBytes"].(float64) != 300 {
		t.Fatalf("json report row = %v", row)
	}
	summary := rep["summary"].(map[string]any)
	if summary["volumeBytes"].(float64) != 700 || summary["allocatedBytes"].(float64) != 700 || summary["availableBytes"].(float64) != 300 {
		t.Fatalf("json report summary = %v", summary)
	}

	csv := do(h, http.MethodGet, "/v1/capacity-report?format=csv&poolId=vp", "").Body.String()
	want := csvHeader + "\nvp,1000,0,700,0,0,700,300\n"
	if csv != want {
		t.Fatalf("csv report = %q want %q", csv, want)
	}
}

func TestResizeRouting(t *testing.T) {
	h := Handler()
	createPool(t, h, volPool)
	createVol(t, h, `{"id":"vol1","poolId":"vp","sizeBytes":10}`)

	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodDelete, http.MethodPatch} {
		rec := do(h, method, "/v1/volumes/vol1/size", `{"sizeBytes":20,"expectedGeneration":0}`)
		if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "PUT" {
			t.Fatalf("%s size = %d allow=%q", method, rec.Code, rec.Header().Get("Allow"))
		}
	}
	wantError(t, do(h, http.MethodPut, "/v1/volumes/vol1/size/extra", `{}`),
		http.StatusNotFound, "not_found")
}

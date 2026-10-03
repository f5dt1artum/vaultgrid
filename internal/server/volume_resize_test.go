package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// resizeVolume issues PUT /v1/volumes/{id}/size.
func resizeVolume(h http.Handler, id string, body string) *httptest.ResponseRecorder {
	return do(h, http.MethodPut, "/v1/volumes/"+id+"/size", body)
}

func TestResizeVolumeGrowRepresentation(t *testing.T) {
	h := Handler()
	createPool(t, h, volPool)
	createVol(t, h, `{"id":"vol1","poolId":"vp","sizeBytes":300}`)

	rec := resizeVolume(h, "vol1", `{"sizeBytes":900,"expectedGeneration":0}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("resize status = %d %s", rec.Code, rec.Body.String())
	}
	v := decodeBody(t, rec)
	if v["id"] != "vol1" || v["poolId"] != "vp" {
		t.Fatalf("identity fields wrong: %v", v)
	}
	if v["sizeBytes"].(float64) != 900 {
		t.Fatalf("sizeBytes = %v", v["sizeBytes"])
	}
	if v["generation"].(float64) != 1 {
		t.Fatalf("generation = %v, want 1", v["generation"])
	}
	if binding, ok := v["binding"]; !ok || binding != nil {
		t.Fatalf("binding = %v, want null", binding)
	}

	// Pool counters move by the delta immediately.
	p := decodeBody(t, do(h, http.MethodGet, "/v1/storage-pools/vp", ""))
	if p["allocatedBytes"].(float64) != 900 || p["availableBytes"].(float64) != 100 {
		t.Fatalf("pool after grow = %v", p)
	}
}

func TestResizeVolumeShrink(t *testing.T) {
	h := Handler()
	createPool(t, h, volPool)
	createVol(t, h, `{"id":"vol1","poolId":"vp","sizeBytes":300}`)

	rec := resizeVolume(h, "vol1", `{"sizeBytes":50,"expectedGeneration":0}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("shrink status = %d %s", rec.Code, rec.Body.String())
	}
	v := decodeBody(t, rec)
	if v["sizeBytes"].(float64) != 50 || v["generation"].(float64) != 1 {
		t.Fatalf("volume after shrink = %v", v)
	}
	p := decodeBody(t, do(h, http.MethodGet, "/v1/storage-pools/vp", ""))
	if p["allocatedBytes"].(float64) != 50 || p["availableBytes"].(float64) != 950 {
		t.Fatalf("pool after shrink = %v", p)
	}

	// The freed capacity is reusable: fill the pool so only the 50 bytes the
	// volume shrank away are left.
	rec = do(h, http.MethodPost, "/v1/storage-pools/vp/reservations", `{"requestId":"r1","bytes":900}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("reserve freed capacity = %d %s", rec.Code, rec.Body.String())
	}
	// Growing back by exactly the free delta fits (950 <= 950).
	rec = resizeVolume(h, "vol1", `{"sizeBytes":100,"expectedGeneration":1}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("regrow to the limit = %d %s", rec.Code, rec.Body.String())
	}
	// One delta byte more fails on pool capacity.
	wantError(t, resizeVolume(h, "vol1", `{"sizeBytes":101,"expectedGeneration":2}`),
		http.StatusConflict, "insufficient_capacity")
	// The failed grow changed nothing.
	v = decodeBody(t, do(h, http.MethodGet, "/v1/volumes/vol1", ""))
	if v["sizeBytes"].(float64) != 100 || v["generation"].(float64) != 2 {
		t.Fatalf("volume after failed grow = %v", v)
	}
}

func TestResizeVolumeSameSizeNoOp(t *testing.T) {
	h := Handler()
	createPool(t, h, volPool)
	createVol(t, h, `{"id":"vol1","poolId":"vp","sizeBytes":300}`)

	rec := resizeVolume(h, "vol1", `{"sizeBytes":300,"expectedGeneration":0}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("same-size status = %d %s", rec.Code, rec.Body.String())
	}
	v := decodeBody(t, rec)
	if v["sizeBytes"].(float64) != 300 || v["generation"].(float64) != 0 {
		t.Fatalf("same-size view = %v", v)
	}
	// No metering change.
	p := decodeBody(t, do(h, http.MethodGet, "/v1/storage-pools/vp", ""))
	if p["allocatedBytes"].(float64) != 300 || p["availableBytes"].(float64) != 700 {
		t.Fatalf("pool after same-size = %v", p)
	}
	// No audit event: pool.created and volume.created remain the only entries.
	events := getAuditEvents(t, h, "/v1/audit-events?limit=1000")["items"].([]any)
	if len(events) != 2 {
		t.Fatalf("same-size resize left %d events: %v", len(events), events)
	}
	for _, raw := range events {
		if raw.(map[string]any)["action"] == "volume.resized" {
			t.Fatalf("same-size resize recorded an event: %v", raw)
		}
	}

	// A same-size request still has to name the current generation.
	wantError(t, resizeVolume(h, "vol1", `{"sizeBytes":300,"expectedGeneration":5}`),
		http.StatusConflict, "stale_generation")
}

func TestResizeVolumeGenerationAdvancesGuard(t *testing.T) {
	h := Handler()
	createPool(t, h, volPool)
	createVol(t, h, `{"id":"vol1","poolId":"vp","sizeBytes":10}`)

	wantError(t, resizeVolume(h, "vol1", `{"sizeBytes":20,"expectedGeneration":1}`),
		http.StatusConflict, "stale_generation")
	if rec := resizeVolume(h, "vol1", `{"sizeBytes":20,"expectedGeneration":0}`); rec.Code != http.StatusOK {
		t.Fatalf("grow = %d %s", rec.Code, rec.Body.String())
	}
	// After the resize the next guard is generation 1; binding follows suit.
	wantError(t, resizeVolume(h, "vol1", `{"sizeBytes":30,"expectedGeneration":0}`),
		http.StatusConflict, "stale_generation")
	if rec := resizeVolume(h, "vol1", `{"sizeBytes":30,"expectedGeneration":1}`); rec.Code != http.StatusOK {
		t.Fatalf("second grow = %d %s", rec.Code, rec.Body.String())
	}
	rec := do(h, http.MethodPut, "/v1/volumes/vol1/binding", `{"nodeId":"n1","expectedGeneration":2}`)
	if rec.Code != http.StatusOK || decodeBody(t, rec)["generation"].(float64) != 3 {
		t.Fatalf("bind after resize = %d %s", rec.Code, rec.Body.String())
	}
}

func TestResizeVolumeBoundVolumeInUse(t *testing.T) {
	h := Handler()
	createPool(t, h, volPool)
	createVol(t, h, `{"id":"vol1","poolId":"vp","sizeBytes":10}`)
	if rec := do(h, http.MethodPut, "/v1/volumes/vol1/binding", `{"nodeId":"n1","expectedGeneration":0}`); rec.Code != http.StatusOK {
		t.Fatalf("bind = %d %s", rec.Code, rec.Body.String())
	}

	// volume_in_use wins over a stale generation, a matching generation and a
	// same-size no-op alike.
	wantError(t, resizeVolume(h, "vol1", `{"sizeBytes":20,"expectedGeneration":0}`),
		http.StatusConflict, "volume_in_use")
	wantError(t, resizeVolume(h, "vol1", `{"sizeBytes":20,"expectedGeneration":1}`),
		http.StatusConflict, "volume_in_use")
	wantError(t, resizeVolume(h, "vol1", `{"sizeBytes":10,"expectedGeneration":1}`),
		http.StatusConflict, "volume_in_use")

	v := decodeBody(t, do(h, http.MethodGet, "/v1/volumes/vol1", ""))
	if v["sizeBytes"].(float64) != 10 || v["generation"].(float64) != 1 {
		t.Fatalf("state changed on rejected resize: %v", v)
	}
}

func TestResizeVolumeNotFoundAndInvalid(t *testing.T) {
	h := Handler()
	createPool(t, h, volPool)

	wantError(t, resizeVolume(h, "missing", `{"sizeBytes":1,"expectedGeneration":0}`),
		http.StatusNotFound, "volume_not_found")
	wantError(t, resizeVolume(h, "bad%20id", `{"sizeBytes":1,"expectedGeneration":0}`),
		http.StatusBadRequest, "invalid_request")

	createVol(t, h, `{"id":"vol1","poolId":"vp","sizeBytes":10}`)
	for name, body := range map[string]string{
		"unknown field":  `{"sizeBytes":20,"expectedGeneration":0,"x":1}`,
		"missing size":   `{"expectedGeneration":0}`,
		"missing gen":    `{"sizeBytes":20}`,
		"zero size":      `{"sizeBytes":0,"expectedGeneration":0}`,
		"negative size":  `{"sizeBytes":-1,"expectedGeneration":0}`,
		"float size":     `{"sizeBytes":1.5,"expectedGeneration":0}`,
		"string size":    `{"sizeBytes":"20","expectedGeneration":0}`,
		"null size":      `{"sizeBytes":null,"expectedGeneration":0}`,
		"negative gen":   `{"sizeBytes":20,"expectedGeneration":-1}`,
		"float gen":      `{"sizeBytes":20,"expectedGeneration":0.5}`,
		"string gen":     `{"sizeBytes":20,"expectedGeneration":"0"}`,
		"null gen":       `{"sizeBytes":20,"expectedGeneration":null}`,
		"malformed json": `{`,
	} {
		t.Run(name, func(t *testing.T) {
			wantError(t, resizeVolume(h, "vol1", body), http.StatusBadRequest, "invalid_request")
		})
	}
	// Any query string is invalid, even one naming the body field.
	for _, target := range []string{
		"/v1/volumes/vol1/size?foo=1",
		"/v1/volumes/vol1/size?expectedGeneration=0",
		"/v1/volumes/vol1/size?sizeBytes=20",
	} {
		wantError(t, do(h, http.MethodPut, target, `{"sizeBytes":20,"expectedGeneration":0}`),
			http.StatusBadRequest, "invalid_request")
	}
	// A rejected resize leaves the volume at its original state.
	v := decodeBody(t, do(h, http.MethodGet, "/v1/volumes/vol1", ""))
	if v["sizeBytes"].(float64) != 10 || v["generation"].(float64) != 0 {
		t.Fatalf("partial resize state: %v", v)
	}
}

func TestResizeVolumeQuotaCheckedBeforeCapacity(t *testing.T) {
	h := Handler()
	createPool(t, h, volPool) // pool "vp", raw 1000
	mustQuota(t, h, "vp", "team-a", 250)
	// team-a owns 200 bytes; a reservation from another tenant fills the pool
	// to capacity, so growing by 51+ bytes breaches both the quota and the
	// pool. The quota error must win.
	if rec := doTenant(h, http.MethodPost, "/v1/volumes",
		`{"id":"vol1","poolId":"vp","sizeBytes":200}`, "team-a"); rec.Code != http.StatusCreated {
		t.Fatalf("create team-a volume = %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(h, http.MethodPost, "/v1/storage-pools/vp/reservations",
		`{"requestId":"r1","bytes":800}`); rec.Code != http.StatusCreated {
		t.Fatalf("fill pool = %d %s", rec.Code, rec.Body.String())
	}
	wantError(t, resizeVolume(h, "vol1", `{"sizeBytes":251,"expectedGeneration":0}`),
		http.StatusConflict, "tenant_quota_exceeded")

	// Exactly at the quota still breaches the full pool, surfacing capacity.
	wantError(t, resizeVolume(h, "vol1", `{"sizeBytes":250,"expectedGeneration":0}`),
		http.StatusConflict, "insufficient_capacity")

	// Shrinking is constrained by neither limit even while the pool is full.
	if rec := resizeVolume(h, "vol1", `{"sizeBytes":50,"expectedGeneration":0}`); rec.Code != http.StatusOK {
		t.Fatalf("shrink under quota/full pool = %d %s", rec.Code, rec.Body.String())
	}
	q := decodeBody(t, do(h, http.MethodGet, "/v1/storage-pools/vp/tenant-quotas/team-a", ""))
	if q["usedBytes"].(float64) != 50 || q["availableBytes"].(float64) != 200 {
		t.Fatalf("quota view after shrink = %v", q)
	}

	// Growing to the quota limit now succeeds: pool has 150 bytes free after
	// the shrink, and 50+150=200 <= 250.
	rec := resizeVolume(h, "vol1", `{"sizeBytes":200,"expectedGeneration":1}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("grow within quota = %d %s", rec.Code, rec.Body.String())
	}
	q = decodeBody(t, do(h, http.MethodGet, "/v1/storage-pools/vp/tenant-quotas/team-a", ""))
	if q["usedBytes"].(float64) != 200 {
		t.Fatalf("quota used after grow = %v", q["usedBytes"])
	}
	wantError(t, resizeVolume(h, "vol1", `{"sizeBytes":251,"expectedGeneration":2}`),
		http.StatusConflict, "tenant_quota_exceeded")

	// A failed resize moves neither quota usage nor pool counters.
	p := decodeBody(t, do(h, http.MethodGet, "/v1/storage-pools/vp", ""))
	if p["allocatedBytes"].(float64) != 1000 || p["availableBytes"].(float64) != 0 {
		t.Fatalf("pool after rejected grow = %v", p)
	}
}

func TestResizeVolumeCapacityReportJSONAndCSV(t *testing.T) {
	h := Handler()
	createPool(t, h, volPool)
	createVol(t, h, `{"id":"vol1","poolId":"vp","sizeBytes":200}`)
	if rec := resizeVolume(h, "vol1", `{"sizeBytes":350,"expectedGeneration":0}`); rec.Code != http.StatusOK {
		t.Fatalf("resize = %d %s", rec.Code, rec.Body.String())
	}

	report := getReport(t, h, "/v1/capacity-report?poolId=vp")
	item := report["items"].([]any)[0].(map[string]any)
	if item["volumeBytes"].(float64) != 350 ||
		item["allocatedBytes"].(float64) != 350 ||
		item["availableBytes"].(float64) != 650 {
		t.Fatalf("JSON report row = %v", item)
	}
	summary := report["summary"].(map[string]any)
	if summary["volumeBytes"].(float64) != 350 || summary["allocatedBytes"].(float64) != 350 {
		t.Fatalf("JSON report summary = %v", summary)
	}

	rec := do(h, http.MethodGet, "/v1/capacity-report?format=csv&poolId=vp", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("CSV report = %d %s", rec.Code, rec.Body.String())
	}
	want := csvHeader + "\nvp,1000,0,350,0,0,350,650\n"
	if rec.Body.String() != want {
		t.Fatalf("CSV report = %q, want %q", rec.Body.String(), want)
	}

	// A shrink is reflected just as immediately.
	if rec := resizeVolume(h, "vol1", `{"sizeBytes":100,"expectedGeneration":1}`); rec.Code != http.StatusOK {
		t.Fatalf("shrink = %d %s", rec.Code, rec.Body.String())
	}
	rec = do(h, http.MethodGet, "/v1/capacity-report?format=csv&poolId=vp", "")
	want = csvHeader + "\nvp,1000,0,100,0,0,100,900\n"
	if rec.Body.String() != want {
		t.Fatalf("CSV after shrink = %q, want %q", rec.Body.String(), want)
	}
}

func TestResizeVolumeAuditEvent(t *testing.T) {
	h := Handler()
	createPool(t, h, volPool)
	createVol(t, h, `{"id":"vol1","poolId":"vp","sizeBytes":100}`)

	if rec := resizeVolume(h, "vol1", `{"sizeBytes":300,"expectedGeneration":0}`); rec.Code != http.StatusOK {
		t.Fatalf("grow = %d %s", rec.Code, rec.Body.String())
	}
	if rec := resizeVolume(h, "vol1", `{"sizeBytes":50,"expectedGeneration":1}`); rec.Code != http.StatusOK {
		t.Fatalf("shrink = %d %s", rec.Code, rec.Body.String())
	}
	// Same-size and failed requests add no event.
	if rec := resizeVolume(h, "vol1", `{"sizeBytes":50,"expectedGeneration":2}`); rec.Code != http.StatusOK {
		t.Fatalf("same-size = %d %s", rec.Code, rec.Body.String())
	}
	wantError(t, resizeVolume(h, "vol1", `{"sizeBytes":60,"expectedGeneration":0}`),
		http.StatusConflict, "stale_generation")

	items := getAuditEvents(t, h, "/v1/audit-events?limit=1000")["items"].([]any)
	if len(items) != 4 {
		t.Fatalf("events = %v", items)
	}
	checkEvent(t, items[2], expectedEvent{3, "volume.resized", "/v1/volumes/vol1/size", "vp", 200})
	checkEvent(t, items[3], expectedEvent{4, "volume.resized", "/v1/volumes/vol1/size", "vp", -250})
	for _, raw := range items[2:] {
		if len(raw.(map[string]any)) != 5 {
			t.Fatalf("resize event has extra fields: %v", raw)
		}
	}
}

func TestResizeVolumeConcurrentSameGeneration(t *testing.T) {
	h := Handler()
	createPool(t, h, `{"id":"cp","devices":[{"id":"d","capacityBytes":1000,"faultDomain":"fd"}]}`)
	createVol(t, h, `{"id":"vol1","poolId":"cp","sizeBytes":100}`)

	const goroutines = 30
	var wg sync.WaitGroup
	statuses := make(chan int, goroutines)
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			// Every request names generation 0; the grow fits for one winner.
			rec := resizeVolume(h, "vol1", `{"sizeBytes":900,"expectedGeneration":0}`)
			statuses <- rec.Code
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
	if v["sizeBytes"].(float64) != 900 || v["generation"].(float64) != 1 {
		t.Fatalf("volume after concurrent resize = %v", v)
	}
	p := decodeBody(t, do(h, http.MethodGet, "/v1/storage-pools/cp", ""))
	if p["allocatedBytes"].(float64) != 900 || p["availableBytes"].(float64) != 100 {
		t.Fatalf("pool after concurrent resize = %v", p)
	}
	events := getAuditEvents(t, h, "/v1/audit-events?limit=1000")["items"].([]any)
	if len(events) != 3 { // pool.created, volume.created + exactly one volume.resized
		t.Fatalf("event count = %d, want 3", len(events))
	}
}

func TestResizeVolumeSnapshotsKeepTheirSize(t *testing.T) {
	h := Handler()
	createPool(t, h, `{"id":"sp","devices":[{"id":"d","capacityBytes":3000,"faultDomain":"fd"}]}`)
	createPool(t, h, `{"id":"pool-b","devices":[{"id":"d2","capacityBytes":800,"faultDomain":"fd"}]}`)
	createVol(t, h, `{"id":"vol1","poolId":"sp","sizeBytes":600}`)

	// Snapshot taken at generation 0 captures size 600.
	if rec := do(h, http.MethodPost, "/v1/volumes/vol1/snapshots",
		`{"id":"snap1","expectedGeneration":0}`); rec.Code != http.StatusCreated {
		t.Fatalf("snapshot = %d %s", rec.Code, rec.Body.String())
	}
	if rec := resizeVolume(h, "vol1", `{"sizeBytes":900,"expectedGeneration":0}`); rec.Code != http.StatusOK {
		t.Fatalf("resize source = %d %s", rec.Code, rec.Body.String())
	}

	// The old snapshot keeps its captured size, source generation and pool
	// charge; the source resize does not reach it.
	snap := decodeBody(t, do(h, http.MethodGet, "/v1/snapshots/snap1", ""))
	if snap["sizeBytes"].(float64) != 600 || snap["sourceGeneration"].(float64) != 0 {
		t.Fatalf("snapshot after source resize = %v", snap)
	}
	report := getReport(t, h, "/v1/capacity-report?poolId=sp")
	item := report["items"].([]any)[0].(map[string]any)
	if item["volumeBytes"].(float64) != 900 || item["snapshotBytes"].(float64) != 600 ||
		item["allocatedBytes"].(float64) != 1500 {
		t.Fatalf("report after resize with snapshot = %v", item)
	}

	// A snapshot created after the resize captures the new size and
	// generation; the old snapshot is untouched.
	if rec := do(h, http.MethodPost, "/v1/volumes/vol1/snapshots",
		`{"id":"snap2","expectedGeneration":1}`); rec.Code != http.StatusCreated {
		t.Fatalf("second snapshot = %d %s", rec.Code, rec.Body.String())
	}
	snap2 := decodeBody(t, do(h, http.MethodGet, "/v1/snapshots/snap2", ""))
	if snap2["sizeBytes"].(float64) != 900 || snap2["sourceGeneration"].(float64) != 1 {
		t.Fatalf("post-resize snapshot = %v", snap2)
	}

	// A clone of the old snapshot is sized 600 regardless of the source's 900.
	if rec := do(h, http.MethodPost, "/v1/snapshots/snap1/clones",
		`{"id":"copy1","poolId":"pool-b"}`); rec.Code != http.StatusCreated {
		t.Fatalf("clone = %d %s", rec.Code, rec.Body.String())
	}
	copyView := decodeBody(t, do(h, http.MethodGet, "/v1/volumes/copy1", ""))
	if copyView["sizeBytes"].(float64) != 600 || copyView["generation"].(float64) != 0 {
		t.Fatalf("clone size = %v", copyView)
	}

	// Clone volumes resize like any other volume.
	if rec := resizeVolume(h, "copy1", `{"sizeBytes":700,"expectedGeneration":0}`); rec.Code != http.StatusOK {
		t.Fatalf("resize clone = %d %s", rec.Code, rec.Body.String())
	}
	// Replaying the original clone creation is still idempotent and returns
	// the current representation.
	rec := do(h, http.MethodPost, "/v1/snapshots/snap1/clones", `{"id":"copy1","poolId":"pool-b"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("clone replay after resize = %d %s", rec.Code, rec.Body.String())
	}
	replay := decodeBody(t, rec)
	if replay["sizeBytes"].(float64) != 700 || replay["generation"].(float64) != 1 {
		t.Fatalf("clone replay view = %v", replay)
	}
	// Different parameters still conflict.
	wantError(t, do(h, http.MethodPost, "/v1/snapshots/snap1/clones", `{"id":"copy1","poolId":"sp"}`),
		http.StatusConflict, "volume_exists")
}

func TestResizeVolumeChargesOwningTenant(t *testing.T) {
	h := Handler()
	createPool(t, h, volPool)
	mustQuota(t, h, "vp", "team-a", 400)
	if rec := doTenant(h, http.MethodPost, "/v1/volumes",
		`{"id":"vol1","poolId":"vp","sizeBytes":200}`, "team-a"); rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
	}

	// The resize carries no tenant header: the volume's owner stays team-a,
	// and the delta is charged against team-a's quota.
	wantError(t, resizeVolume(h, "vol1", `{"sizeBytes":401,"expectedGeneration":0}`),
		http.StatusConflict, "tenant_quota_exceeded")
	if rec := resizeVolume(h, "vol1", `{"sizeBytes":400,"expectedGeneration":0}`); rec.Code != http.StatusOK {
		t.Fatalf("grow to quota = %d %s", rec.Code, rec.Body.String())
	}
	q := decodeBody(t, do(h, http.MethodGet, "/v1/storage-pools/vp/tenant-quotas/team-a", ""))
	if q["usedBytes"].(float64) != 400 || q["availableBytes"].(float64) != 0 {
		t.Fatalf("quota view = %v", q)
	}
	// The default tenant remains untouched and unlimited.
	p := decodeBody(t, do(h, http.MethodGet, "/v1/storage-pools/vp", ""))
	if p["allocatedBytes"].(float64) != 400 {
		t.Fatalf("pool allocation = %v", p["allocatedBytes"])
	}
}

func TestResizeVolumeRouting(t *testing.T) {
	h := Handler()
	createPool(t, h, volPool)
	createVol(t, h, `{"id":"vol1","poolId":"vp","sizeBytes":10}`)

	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodDelete, http.MethodPatch} {
		rec := do(h, method, "/v1/volumes/vol1/size", "")
		if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodPut {
			t.Fatalf("%s size = %d allow=%q", method, rec.Code, rec.Header().Get("Allow"))
		}
	}
	// Extra sub-paths stay 404.
	wantError(t, do(h, http.MethodPut, "/v1/volumes/vol1/size/extra", `{"sizeBytes":20,"expectedGeneration":0}`),
		http.StatusNotFound, "not_found")
}

func TestResizeVolumeDeleteReconciles(t *testing.T) {
	h := Handler()
	createPool(t, h, volPool)
	createVol(t, h, `{"id":"vol1","poolId":"vp","sizeBytes":100}`)
	for i, size := range []int64{400, 250} {
		gen := fmt.Sprintf("%d", i)
		if rec := resizeVolume(h, "vol1", fmt.Sprintf(`{"sizeBytes":%d,"expectedGeneration":%s}`, size, gen)); rec.Code != http.StatusOK {
			t.Fatalf("resize %d = %d %s", size, rec.Code, rec.Body.String())
		}
	}
	// Deleting the resized volume returns its current size, not the original.
	if rec := do(h, http.MethodDelete, "/v1/volumes/vol1", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d %s", rec.Code, rec.Body.String())
	}
	p := decodeBody(t, do(h, http.MethodGet, "/v1/storage-pools/vp", ""))
	if p["allocatedBytes"].(float64) != 0 || p["availableBytes"].(float64) != 1000 {
		t.Fatalf("pool after delete = %v", p)
	}
	events := getAuditEvents(t, h, "/v1/audit-events?limit=1000")["items"].([]any)
	last := events[len(events)-1].(map[string]any)
	if last["action"] != "volume.deleted" || last["bytesDelta"].(float64) != -250 {
		t.Fatalf("delete event = %v", last)
	}
}

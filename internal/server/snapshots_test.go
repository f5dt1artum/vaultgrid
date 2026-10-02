package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func snapURL(volumeID string) string {
	return "/v1/volumes/" + volumeID + "/snapshots"
}

func createSnapshot(t *testing.T, h http.Handler, volumeID, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := do(h, http.MethodPost, snapURL(volumeID), body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create snapshot status = %d body = %s", rec.Code, rec.Body.String())
	}
	return rec
}

func getPoolView(t *testing.T, h http.Handler, id string) map[string]any {
	t.Helper()
	return decodeBody(t, do(h, http.MethodGet, "/v1/storage-pools/"+id, ""))
}

func TestCreateSnapshotRepresentationAndCharging(t *testing.T) {
	h := Handler()
	createPool(t, h, volPool)
	createVol(t, h, `{"id":"vol1","poolId":"vp","sizeBytes":300}`)

	rec := createSnapshot(t, h, "vol1", `{"id":"snap-1","expectedGeneration":0}`)
	s := decodeBody(t, rec)
	if s["id"] != "snap-1" || s["sourceVolumeId"] != "vol1" || s["poolId"] != "vp" {
		t.Fatalf("snapshot identity fields = %v", s)
	}
	if s["sizeBytes"].(float64) != 300 || s["sourceGeneration"].(float64) != 0 {
		t.Fatalf("snapshot size/generation = %v", s)
	}

	// The snapshot charges the source pool on top of the source volume.
	p := getPoolView(t, h, "vp")
	if p["allocatedBytes"].(float64) != 600 || p["availableBytes"].(float64) != 400 {
		t.Fatalf("pool after snapshot = %v", p)
	}

	// The snapshot records the generation it was taken at.
	if rec := do(h, http.MethodPut, "/v1/volumes/vol1/binding", `{"nodeId":"n1","expectedGeneration":0}`); rec.Code != http.StatusOK {
		t.Fatalf("bind = %d", rec.Code)
	}
	if rec := do(h, http.MethodDelete, "/v1/volumes/vol1/binding?expectedGeneration=1", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("unbind = %d", rec.Code)
	}
	rec = do(h, http.MethodPost, snapURL("vol1"), `{"id":"snap-2","expectedGeneration":2}`)
	if rec.Code != http.StatusCreated || decodeBody(t, rec)["sourceGeneration"].(float64) != 2 {
		t.Fatalf("generation-2 snapshot = %d %s", rec.Code, rec.Body.String())
	}
}

func TestCreateSnapshotErrors(t *testing.T) {
	h := Handler()
	createPool(t, h, volPool)
	createVol(t, h, `{"id":"vol1","poolId":"vp","sizeBytes":10}`)

	wantError(t, do(h, http.MethodPost, snapURL("missing"), `{"id":"s1","expectedGeneration":0}`),
		http.StatusNotFound, "volume_not_found")

	// Bound volumes cannot be snapshotted; in-use takes precedence over a
	// stale generation.
	if rec := do(h, http.MethodPut, "/v1/volumes/vol1/binding", `{"nodeId":"n1","expectedGeneration":0}`); rec.Code != http.StatusOK {
		t.Fatalf("bind = %d", rec.Code)
	}
	wantError(t, do(h, http.MethodPost, snapURL("vol1"), `{"id":"s1","expectedGeneration":1}`),
		http.StatusConflict, "volume_in_use")
	wantError(t, do(h, http.MethodPost, snapURL("vol1"), `{"id":"s1","expectedGeneration":0}`),
		http.StatusConflict, "volume_in_use")
	if rec := do(h, http.MethodDelete, "/v1/volumes/vol1/binding?expectedGeneration=1", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("unbind = %d", rec.Code)
	}

	// Unbound again at generation 2: a stale generation is rejected.
	wantError(t, do(h, http.MethodPost, snapURL("vol1"), `{"id":"s1","expectedGeneration":0}`),
		http.StatusConflict, "stale_generation")
	wantError(t, do(h, http.MethodPost, snapURL("vol1"), `{"id":"s1","expectedGeneration":1}`),
		http.StatusConflict, "stale_generation")

	// Fill the pool with a second volume so no snapshot charge fits.
	createVol(t, h, `{"id":"bigvol","poolId":"vp","sizeBytes":990}`)
	// 10 (vol1) + 990 (bigvol) = 1000; a further 990-byte snapshot of
	// bigvol cannot fit and leaves nothing behind.
	wantError(t, do(h, http.MethodPost, snapURL("bigvol"), `{"id":"snap-big","expectedGeneration":0}`),
		http.StatusConflict, "insufficient_capacity")
	if rec := do(h, http.MethodGet, "/v1/snapshots", ""); rec.Body.String() != "{\"items\":[]}\n" {
		t.Fatalf("partial snapshot state: %s", rec.Body.String())
	}
	if p := getPoolView(t, h, "vp"); p["allocatedBytes"].(float64) != 1000 {
		t.Fatalf("partial allocation after failure: %v", p["allocatedBytes"])
	}
}

func TestCreateSnapshotInvalidRequests(t *testing.T) {
	h := Handler()
	createPool(t, h, volPool)
	createVol(t, h, `{"id":"vol1","poolId":"vp","sizeBytes":10}`)
	for name, body := range map[string]string{
		"unknown field":       `{"id":"s1","expectedGeneration":0,"x":1}`,
		"missing id":          `{"expectedGeneration":0}`,
		"missing generation":  `{"id":"s1"}`,
		"empty id":            `{"id":"","expectedGeneration":0}`,
		"bad id":              `{"id":"bad id","expectedGeneration":0}`,
		"null generation":     `{"id":"s1","expectedGeneration":null}`,
		"negative generation": `{"id":"s1","expectedGeneration":-1}`,
		"float generation":    `{"id":"s1","expectedGeneration":0.5}`,
		"string generation":   `{"id":"s1","expectedGeneration":"0"}`,
		"malformed json":      `{`,
	} {
		t.Run(name, func(t *testing.T) {
			wantError(t, do(h, http.MethodPost, snapURL("vol1"), body), http.StatusBadRequest, "invalid_request")
		})
	}
	wantError(t, do(h, http.MethodPost, snapURL("bad%20id"), `{"id":"s1","expectedGeneration":0}`),
		http.StatusBadRequest, "invalid_request")
	if rec := do(h, http.MethodGet, "/v1/snapshots", ""); rec.Body.String() != "{\"items\":[]}\n" {
		t.Fatalf("partial state: %s", rec.Body.String())
	}
}

func TestSnapshotIdempotencyAndExists(t *testing.T) {
	h := Handler()
	createPool(t, h, volPool)
	createVol(t, h, `{"id":"vol1","poolId":"vp","sizeBytes":300}`)
	createSnapshot(t, h, "vol1", `{"id":"snap-1","expectedGeneration":0}`)

	// Identical retry: 200, same representation, no double metering.
	rec := do(h, http.MethodPost, snapURL("vol1"), `{"id":"snap-1","expectedGeneration":0}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("retry status = %d %s", rec.Code, rec.Body.String())
	}
	s := decodeBody(t, rec)
	if s["sourceGeneration"].(float64) != 0 || s["sizeBytes"].(float64) != 300 {
		t.Fatalf("retry view = %v", s)
	}
	if p := getPoolView(t, h, "vp"); p["allocatedBytes"].(float64) != 600 {
		t.Fatalf("allocated after retry = %v", p["allocatedBytes"])
	}

	// Same id, different source generation: snapshot_exists.
	if rec := do(h, http.MethodPut, "/v1/volumes/vol1/binding", `{"nodeId":"n1","expectedGeneration":0}`); rec.Code != http.StatusOK {
		t.Fatalf("bind = %d", rec.Code)
	}
	if rec := do(h, http.MethodDelete, "/v1/volumes/vol1/binding?expectedGeneration=1", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("unbind = %d", rec.Code)
	}
	wantError(t, do(h, http.MethodPost, snapURL("vol1"), `{"id":"snap-1","expectedGeneration":2}`),
		http.StatusConflict, "snapshot_exists")

	// Same id, different source volume: snapshot_exists.
	createVol(t, h, `{"id":"vol2","poolId":"vp","sizeBytes":10}`)
	wantError(t, do(h, http.MethodPost, snapURL("vol2"), `{"id":"snap-1","expectedGeneration":0}`),
		http.StatusConflict, "snapshot_exists")
}

func TestSnapshotIndependentOfSourceLifecycle(t *testing.T) {
	h := Handler()
	createPool(t, h, volPool)
	createVol(t, h, `{"id":"vol1","poolId":"vp","sizeBytes":300}`)
	createSnapshot(t, h, "vol1", `{"id":"snap-1","expectedGeneration":0}`)

	// Rebind the source; the snapshot stays at its captured generation.
	if rec := do(h, http.MethodPut, "/v1/volumes/vol1/binding", `{"nodeId":"n1","expectedGeneration":0}`); rec.Code != http.StatusOK {
		t.Fatalf("bind = %d", rec.Code)
	}
	s := decodeBody(t, do(h, http.MethodGet, "/v1/snapshots/snap-1", ""))
	if s["sourceGeneration"].(float64) != 0 {
		t.Fatalf("snapshot followed source change: %v", s)
	}
	if rec := do(h, http.MethodDelete, "/v1/volumes/vol1/binding?expectedGeneration=1", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("unbind = %d", rec.Code)
	}

	// Delete the source: only the volume's 300 bytes return; the snapshot
	// remains readable and keeps charging the pool.
	if rec := do(h, http.MethodDelete, "/v1/volumes/vol1", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete source = %d", rec.Code)
	}
	rec := do(h, http.MethodGet, "/v1/snapshots/snap-1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("snapshot after source delete = %d", rec.Code)
	}
	s = decodeBody(t, rec)
	if s["sourceVolumeId"] != "vol1" || s["sizeBytes"].(float64) != 300 || s["poolId"] != "vp" {
		t.Fatalf("snapshot view after source delete = %v", s)
	}
	if p := getPoolView(t, h, "vp"); p["allocatedBytes"].(float64) != 300 || p["availableBytes"].(float64) != 700 {
		t.Fatalf("pool after source delete = %v", p)
	}

	// An idempotent retry still returns 200 even though the source is gone.
	if rec := do(h, http.MethodPost, snapURL("vol1"), `{"id":"snap-1","expectedGeneration":0}`); rec.Code != http.StatusOK {
		t.Fatalf("retry after source delete = %d %s", rec.Code, rec.Body.String())
	}

	// The pool cannot be deleted while the snapshot exists.
	wantError(t, do(h, http.MethodDelete, "/v1/storage-pools/vp", ""), http.StatusConflict, "pool_not_empty")

	// Deleting the snapshot returns its capacity and frees the pool.
	if rec := do(h, http.MethodDelete, "/v1/snapshots/snap-1", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete snapshot = %d", rec.Code)
	}
	wantError(t, do(h, http.MethodGet, "/v1/snapshots/snap-1", ""), http.StatusNotFound, "snapshot_not_found")
	wantError(t, do(h, http.MethodDelete, "/v1/snapshots/snap-1", ""), http.StatusNotFound, "snapshot_not_found")
	if p := getPoolView(t, h, "vp"); p["allocatedBytes"].(float64) != 0 || p["availableBytes"].(float64) != 1000 {
		t.Fatalf("pool after snapshot delete = %v", p)
	}
	if rec := do(h, http.MethodDelete, "/v1/storage-pools/vp", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete pool = %d", rec.Code)
	}
}

func TestListAndGetSnapshots(t *testing.T) {
	h := Handler()
	createPool(t, h, volPool)
	createVol(t, h, `{"id":"vol1","poolId":"vp","sizeBytes":1}`)
	if rec := do(h, http.MethodGet, "/v1/snapshots", ""); rec.Body.String() != "{\"items\":[]}\n" {
		t.Fatalf("empty list = %q", rec.Body.String())
	}
	createSnapshot(t, h, "vol1", `{"id":"zeta","expectedGeneration":0}`)
	createSnapshot(t, h, "vol1", `{"id":"alpha","expectedGeneration":0}`)
	createSnapshot(t, h, "vol1", `{"id":"mid","expectedGeneration":0}`)

	items := decodeBody(t, do(h, http.MethodGet, "/v1/snapshots", ""))["items"].([]any)
	got := []string{
		items[0].(map[string]any)["id"].(string),
		items[1].(map[string]any)["id"].(string),
		items[2].(map[string]any)["id"].(string),
	}
	if got[0] != "alpha" || got[1] != "mid" || got[2] != "zeta" {
		t.Fatalf("snapshots not sorted: %v", got)
	}
	wantError(t, do(h, http.MethodGet, "/v1/snapshots/missing", ""), http.StatusNotFound, "snapshot_not_found")
	wantError(t, do(h, http.MethodGet, "/v1/snapshots/bad%20id", ""), http.StatusBadRequest, "invalid_request")
}

func clone(t *testing.T, h http.Handler, snapshotID, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := do(h, http.MethodPost, "/v1/snapshots/"+snapshotID+"/clones", body)
	return rec
}

func TestCloneLifecycle(t *testing.T) {
	h := Handler()
	createPool(t, h, `{"id":"pa","devices":[{"id":"da","capacityBytes":2000,"faultDomain":"r1"}]}`)
	createPool(t, h, `{"id":"pb","devices":[{"id":"db","capacityBytes":1000,"faultDomain":"r2"}]}`)
	createVol(t, h, `{"id":"vol1","poolId":"pa","sizeBytes":300}`)
	createSnapshot(t, h, "vol1", `{"id":"snap-1","expectedGeneration":0}`)

	// Cross-pool clone: independent volume at generation 0, unbound, charged
	// to the target pool.
	rec := clone(t, h, "snap-1", `{"id":"vol-copy","poolId":"pb"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("clone status = %d %s", rec.Code, rec.Body.String())
	}
	v := decodeBody(t, rec)
	if v["id"] != "vol-copy" || v["poolId"] != "pb" || v["sizeBytes"].(float64) != 300 {
		t.Fatalf("clone view = %v", v)
	}
	if v["generation"].(float64) != 0 || v["binding"] != nil {
		t.Fatalf("clone generation/binding = %v", v)
	}
	if p := getPoolView(t, h, "pb"); p["allocatedBytes"].(float64) != 300 || p["availableBytes"].(float64) != 700 {
		t.Fatalf("target pool after clone = %v", p)
	}
	// Source pool keeps its volume + snapshot charge.
	if p := getPoolView(t, h, "pa"); p["allocatedBytes"].(float64) != 600 {
		t.Fatalf("source pool after clone = %v", p)
	}

	// The clone is a normal volume: it can be bound and listed with volumes.
	if rec := do(h, http.MethodPut, "/v1/volumes/vol-copy/binding", `{"nodeId":"n1","expectedGeneration":0}`); rec.Code != http.StatusOK {
		t.Fatalf("bind clone = %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(h, http.MethodDelete, "/v1/volumes/vol-copy/binding?expectedGeneration=1", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("unbind clone = %d", rec.Code)
	}

	// Identical retry: 200, current representation, no extra charge.
	rec = clone(t, h, "snap-1", `{"id":"vol-copy","poolId":"pb"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("clone retry = %d %s", rec.Code, rec.Body.String())
	}
	if p := getPoolView(t, h, "pb"); p["allocatedBytes"].(float64) != 300 {
		t.Fatalf("target pool after retry = %v", p["allocatedBytes"])
	}

	// Same volume id toward a different pool: volume_exists.
	wantError(t, clone(t, h, "snap-1", `{"id":"vol-copy","poolId":"pa"}`),
		http.StatusConflict, "volume_exists")

	// A volume id owned by a direct creation clashes even on the same pool.
	createVol(t, h, `{"id":"direct","poolId":"pb","sizeBytes":401}`)
	wantError(t, clone(t, h, "snap-1", `{"id":"direct","poolId":"pb"}`),
		http.StatusConflict, "volume_exists")

	// A volume id produced from another snapshot clashes as well.
	createSnapshot(t, h, "vol1", `{"id":"snap-2","expectedGeneration":0}`)
	wantError(t, clone(t, h, "snap-2", `{"id":"vol-copy","poolId":"pb"}`),
		http.StatusConflict, "volume_exists")

	// Same-pool clone is allowed.
	rec = clone(t, h, "snap-1", `{"id":"vol-twin","poolId":"pa"}`)
	if rec.Code != http.StatusCreated || decodeBody(t, rec)["poolId"] != "pa" {
		t.Fatalf("same-pool clone = %d %s", rec.Code, rec.Body.String())
	}

	// Snapshot and pool lookup failures.
	wantError(t, clone(t, h, "missing", `{"id":"x","poolId":"pb"}`), http.StatusNotFound, "snapshot_not_found")
	wantError(t, clone(t, h, "snap-1", `{"id":"x","poolId":"nope"}`), http.StatusNotFound, "pool_not_found")

	// Insufficient capacity in the target pool leaves nothing behind.
	wantError(t, clone(t, h, "snap-1", `{"id":"fat","poolId":"pb"}`),
		http.StatusConflict, "insufficient_capacity")
	wantError(t, do(h, http.MethodGet, "/v1/volumes/fat", ""), http.StatusNotFound, "volume_not_found")

	// Invalid requests.
	for name, body := range map[string]string{
		"unknown field": `{"id":"c","poolId":"pb","x":1}`,
		"missing id":    `{"poolId":"pb"}`,
		"missing pool":  `{"id":"c"}`,
		"bad id":        `{"id":"bad c","poolId":"pb"}`,
		"bad pool":      `{"id":"c","poolId":"bad p"}`,
		"malformed":     `{`,
	} {
		t.Run(name, func(t *testing.T) {
			wantError(t, clone(t, h, "snap-1", body), http.StatusBadRequest, "invalid_request")
		})
	}
	wantError(t, clone(t, h, "bad%20id", `{"id":"c","poolId":"pb"}`), http.StatusBadRequest, "invalid_request")
}

func TestSnapshotAndCloneRouting(t *testing.T) {
	h := Handler()
	createPool(t, h, volPool)
	createVol(t, h, `{"id":"vol1","poolId":"vp","sizeBytes":1}`)
	createSnapshot(t, h, "vol1", `{"id":"snap-1","expectedGeneration":0}`)

	rec := do(h, http.MethodPost, "/v1/snapshots", "")
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET" {
		t.Fatalf("POST snapshot collection = %d allow=%q", rec.Code, rec.Header().Get("Allow"))
	}
	rec = do(h, http.MethodPut, "/v1/snapshots/snap-1", "")
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET, DELETE" {
		t.Fatalf("PUT snapshot = %d allow=%q", rec.Code, rec.Header().Get("Allow"))
	}
	rec = do(h, http.MethodGet, "/v1/snapshots/snap-1/clones", "")
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "POST" {
		t.Fatalf("GET clones = %d allow=%q", rec.Code, rec.Header().Get("Allow"))
	}
	rec = do(h, http.MethodGet, "/v1/volumes/vol1/snapshots", "")
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "POST" {
		t.Fatalf("GET volume snapshots = %d allow=%q", rec.Code, rec.Header().Get("Allow"))
	}
	wantError(t, do(h, http.MethodGet, "/v1/snapshots/snap-1/extra", ""), http.StatusNotFound, "not_found")
	wantError(t, do(h, http.MethodGet, "/v1/snapshots/snap-1/clones/extra", ""), http.StatusNotFound, "not_found")
	wantError(t, do(h, http.MethodGet, "/v1/volumes/vol1/snapshots/extra", ""), http.StatusNotFound, "not_found")
}

func TestConcurrentSnapshotRetriesMeteredOnce(t *testing.T) {
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
			statuses <- do(h, http.MethodPost, snapURL("vol1"), `{"id":"same","expectedGeneration":0}`).Code
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
	// Volume 100 + one snapshot charge 100, never more.
	if p := getPoolView(t, h, "cp"); p["allocatedBytes"].(float64) != 200 {
		t.Fatalf("pool after concurrent retries = %v", p)
	}
}

func TestConcurrentDistinctSnapshotsNeverOvercommit(t *testing.T) {
	h := Handler()
	createPool(t, h, `{"id":"cp","devices":[{"id":"d","capacityBytes":100,"faultDomain":"fd"}]}`)
	// No source volume: snapshots are charged independently against the pool.
	// Use one small source volume shared by all snapshots.
	createVol(t, h, `{"id":"vol1","poolId":"cp","sizeBytes":10}`)

	const goroutines = 50
	var wg sync.WaitGroup
	var created, rejected int64
	var mu sync.Mutex
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(i int) {
			defer wg.Done()
			body := fmt.Sprintf(`{"id":"s%d","expectedGeneration":0}`, i)
			rec := do(h, http.MethodPost, snapURL("vol1"), body)
			mu.Lock()
			switch rec.Code {
			case http.StatusCreated:
				created++
			case http.StatusConflict:
				rejected++
			default:
				t.Errorf("unexpected status %d", rec.Code)
			}
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	// 10 bytes for the volume leaves room for nine 10-byte snapshots.
	if created != 9 || rejected != 41 {
		t.Fatalf("created=%d rejected=%d, want 9/41", created, rejected)
	}
	if p := getPoolView(t, h, "cp"); p["allocatedBytes"].(float64) != 100 || p["availableBytes"].(float64) != 0 {
		t.Fatalf("post-concurrency pool = %v", p)
	}
}

func TestConcurrentCloneRetriesMeteredOnce(t *testing.T) {
	h := Handler()
	createPool(t, h, `{"id":"pa","devices":[{"id":"da","capacityBytes":1000,"faultDomain":"r1"}]}`)
	createPool(t, h, `{"id":"pb","devices":[{"id":"db","capacityBytes":1000,"faultDomain":"r2"}]}`)
	createVol(t, h, `{"id":"vol1","poolId":"pa","sizeBytes":100}`)
	createSnapshot(t, h, "vol1", `{"id":"snap-1","expectedGeneration":0}`)

	const goroutines = 30
	var wg sync.WaitGroup
	statuses := make(chan int, goroutines)
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			statuses <- clone(t, h, "snap-1", `{"id":"copy","poolId":"pb"}`).Code
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
	if p := getPoolView(t, h, "pb"); p["allocatedBytes"].(float64) != 100 {
		t.Fatalf("target pool after concurrent retries = %v", p)
	}
}

func TestConcurrentDistinctClonesNeverOvercommit(t *testing.T) {
	h := Handler()
	createPool(t, h, `{"id":"pa","devices":[{"id":"da","capacityBytes":1000,"faultDomain":"r1"}]}`)
	createPool(t, h, `{"id":"pb","devices":[{"id":"db","capacityBytes":100,"faultDomain":"r2"}]}`)
	createVol(t, h, `{"id":"vol1","poolId":"pa","sizeBytes":10}`)
	createSnapshot(t, h, "vol1", `{"id":"snap-1","expectedGeneration":0}`)

	const goroutines = 50
	var wg sync.WaitGroup
	var created, rejected int64
	var mu sync.Mutex
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(i int) {
			defer wg.Done()
			body := fmt.Sprintf(`{"id":"c%d","poolId":"pb"}`, i)
			rec := clone(t, h, "snap-1", body)
			mu.Lock()
			switch rec.Code {
			case http.StatusCreated:
				created++
			case http.StatusConflict:
				rejected++
			default:
				t.Errorf("unexpected status %d", rec.Code)
			}
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	if created != 10 || rejected != 40 {
		t.Fatalf("created=%d rejected=%d, want 10/40", created, rejected)
	}
	if p := getPoolView(t, h, "pb"); p["allocatedBytes"].(float64) != 100 || p["availableBytes"].(float64) != 0 {
		t.Fatalf("post-concurrency target pool = %v", p)
	}
}

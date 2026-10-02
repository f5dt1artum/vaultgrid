package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// snapshotFixture builds two pools and one 300-byte unbound volume in pool-a.
func snapshotFixture(t *testing.T, h http.Handler) {
	t.Helper()
	createPool(t, h, `{"id":"pool-a","devices":[{"id":"dev-a","capacityBytes":1000,"faultDomain":"rack1"}]}`)
	createPool(t, h, `{"id":"pool-b","devices":[{"id":"dev-b","capacityBytes":1000,"faultDomain":"rack2"}]}`)
	createVol(t, h, `{"id":"vol1","poolId":"pool-a","sizeBytes":300}`)
}

func postSnapshot(t *testing.T, h http.Handler, volumeID, body string) *httptest.ResponseRecorder {
	t.Helper()
	return do(h, http.MethodPost, "/v1/volumes/"+volumeID+"/snapshots", body)
}

func createSnap(t *testing.T, h http.Handler, volumeID, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := postSnapshot(t, h, volumeID, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create snapshot status = %d body = %s", rec.Code, rec.Body.String())
	}
	return rec
}

func cloneFrom(t *testing.T, h http.Handler, snapID, body string) *httptest.ResponseRecorder {
	t.Helper()
	return do(h, http.MethodPost, "/v1/snapshots/"+snapID+"/clones", body)
}

func poolViewOf(t *testing.T, h http.Handler, id string) map[string]any {
	t.Helper()
	return decodeBody(t, do(h, http.MethodGet, "/v1/storage-pools/"+id, ""))
}

func TestCreateSnapshotRepresentationAndCharging(t *testing.T) {
	h := Handler()
	snapshotFixture(t, h)

	rec := createSnap(t, h, "vol1", `{"id":"snap-1","expectedGeneration":0}`)
	s := decodeBody(t, rec)
	if s["id"] != "snap-1" || s["sourceVolumeId"] != "vol1" || s["poolId"] != "pool-a" {
		t.Fatalf("snapshot identity fields = %v", s)
	}
	if s["sizeBytes"].(float64) != 300 || s["sourceGeneration"].(float64) != 0 {
		t.Fatalf("snapshot size/generation = %v", s)
	}

	// The snapshot reserves an additional sizeBytes in the source pool.
	p := poolViewOf(t, h, "pool-a")
	if p["allocatedBytes"].(float64) != 600 || p["availableBytes"].(float64) != 400 {
		t.Fatalf("source pool after snapshot = %v", p)
	}

	// GET member and sorted listing agree.
	if rec := do(h, http.MethodGet, "/v1/snapshots/snap-1", ""); rec.Code != http.StatusOK ||
		decodeBody(t, rec)["id"] != "snap-1" {
		t.Fatalf("get snapshot = %d %s", rec.Code, rec.Body.String())
	}
	createSnap(t, h, "vol1", `{"id":"snap-2","expectedGeneration":0}`)
	rec = do(h, http.MethodGet, "/v1/snapshots", "")
	items := decodeBody(t, rec)["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("snapshot items = %v", items)
	}
	if items[0].(map[string]any)["id"] != "snap-1" || items[1].(map[string]any)["id"] != "snap-2" {
		t.Fatalf("snapshots not sorted by id: %v", items)
	}
}

func TestCreateSnapshotIdempotencyAndConflicts(t *testing.T) {
	h := Handler()
	snapshotFixture(t, h)
	createSnap(t, h, "vol1", `{"id":"snap-1","expectedGeneration":0}`)

	// Identical retry: 200, same representation, no double charge.
	rec := postSnapshot(t, h, "vol1", `{"id":"snap-1","expectedGeneration":0}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("retry status = %d %s", rec.Code, rec.Body.String())
	}
	s := decodeBody(t, rec)
	if s["sourceVolumeId"] != "vol1" || s["sourceGeneration"].(float64) != 0 || s["sizeBytes"].(float64) != 300 {
		t.Fatalf("retry view = %v", s)
	}
	if p := poolViewOf(t, h, "pool-a"); p["allocatedBytes"].(float64) != 600 {
		t.Fatalf("allocated changed after retry: %v", p["allocatedBytes"])
	}

	// Same id, different source generation or volume: 409 snapshot_exists.
	wantError(t, postSnapshot(t, h, "vol1", `{"id":"snap-1","expectedGeneration":1}`),
		http.StatusConflict, "snapshot_exists")
	createVol(t, h, `{"id":"vol2","poolId":"pool-a","sizeBytes":10}`)
	wantError(t, postSnapshot(t, h, "vol2", `{"id":"snap-1","expectedGeneration":0}`),
		http.StatusConflict, "snapshot_exists")

	// Source volume missing, bound, stale generation and capacity failures.
	wantError(t, postSnapshot(t, h, "nope", `{"id":"snap-x","expectedGeneration":0}`),
		http.StatusNotFound, "volume_not_found")
	wantError(t, postSnapshot(t, h, "vol1", `{"id":"snap-x","expectedGeneration":2}`),
		http.StatusConflict, "stale_generation")

	if rec := do(h, http.MethodPut, "/v1/volumes/vol2/binding", `{"nodeId":"node-1","expectedGeneration":0}`); rec.Code != http.StatusOK {
		t.Fatalf("bind vol2 = %d %s", rec.Code, rec.Body.String())
	}
	wantError(t, postSnapshot(t, h, "vol2", `{"id":"snap-x","expectedGeneration":1}`),
		http.StatusConflict, "volume_in_use")

	// pool-a is at 610 (vol1 300 + vol2 10 + snap-1 300). Fill the remaining
	// 390 bytes, then another 300-byte snapshot of vol1 cannot fit and must
	// leave no state behind.
	createVol(t, h, `{"id":"vol3","poolId":"pool-a","sizeBytes":390}`)
	wantError(t, postSnapshot(t, h, "vol1", `{"id":"snap-x","expectedGeneration":0}`),
		http.StatusConflict, "insufficient_capacity")
	if rec := do(h, http.MethodGet, "/v1/snapshots", ""); len(decodeBody(t, rec)["items"].([]any)) != 1 {
		t.Fatalf("partial snapshot after capacity failure: %s", rec.Body.String())
	}
	if p := poolViewOf(t, h, "pool-a"); p["allocatedBytes"].(float64) != 1000 {
		t.Fatalf("partial allocation after failure: %v", p["allocatedBytes"])
	}
}

func TestCreateSnapshotInvalidRequests(t *testing.T) {
	h := Handler()
	snapshotFixture(t, h)
	for name, body := range map[string]string{
		"unknown field":       `{"id":"snap-1","expectedGeneration":0,"extra":1}`,
		"missing id":          `{"expectedGeneration":0}`,
		"empty id":            `{"id":"","expectedGeneration":0}`,
		"bad id":              `{"id":"bad id","expectedGeneration":0}`,
		"missing generation":  `{"id":"snap-1"}`,
		"null generation":     `{"id":"snap-1","expectedGeneration":null}`,
		"negative generation": `{"id":"snap-1","expectedGeneration":-1}`,
		"float generation":    `{"id":"snap-1","expectedGeneration":0.5}`,
		"string generation":   `{"id":"snap-1","expectedGeneration":"0"}`,
		"malformed json":      `{"id":"snap-1",`,
	} {
		t.Run(name, func(t *testing.T) {
			wantError(t, postSnapshot(t, h, "vol1", body), http.StatusBadRequest, "invalid_request")
		})
	}
	wantError(t, postSnapshot(t, h, "bad%20id", `{"id":"snap-1","expectedGeneration":0}`),
		http.StatusBadRequest, "invalid_request")
	// No partial state.
	if rec := do(h, http.MethodGet, "/v1/snapshots", ""); rec.Body.String() != "{\"items\":[]}\n" {
		t.Fatalf("partial state: %s", rec.Body.String())
	}
}

func TestSnapshotIndependentOfSourceLifecycle(t *testing.T) {
	h := Handler()
	snapshotFixture(t, h)
	createSnap(t, h, "vol1", `{"id":"snap-1","expectedGeneration":0}`)

	// Drive the source volume through bind/unbind (generation 0 -> 1 -> 2),
	// then delete it. The snapshot keeps sizeBytes and sourceGeneration 0.
	if rec := do(h, http.MethodPut, "/v1/volumes/vol1/binding", `{"nodeId":"node-1","expectedGeneration":0}`); rec.Code != http.StatusOK {
		t.Fatalf("bind = %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(h, http.MethodDelete, "/v1/volumes/vol1/binding?expectedGeneration=1", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("unbind = %d", rec.Code)
	}
	if rec := do(h, http.MethodDelete, "/v1/volumes/vol1", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete source = %d %s", rec.Code, rec.Body.String())
	}
	s := decodeBody(t, do(h, http.MethodGet, "/v1/snapshots/snap-1", ""))
	if s["sizeBytes"].(float64) != 300 || s["sourceGeneration"].(float64) != 0 || s["sourceVolumeId"] != "vol1" {
		t.Fatalf("snapshot changed after source delete: %v", s)
	}
	// Source volume's 300 bytes are released, but the snapshot still charges
	// 300 bytes to the same pool.
	if p := poolViewOf(t, h, "pool-a"); p["allocatedBytes"].(float64) != 300 || p["availableBytes"].(float64) != 700 {
		t.Fatalf("pool after source delete = %v", p)
	}
	// A snapshot alone keeps the pool non-empty.
	wantError(t, do(h, http.MethodDelete, "/v1/storage-pools/pool-a", ""), http.StatusConflict, "pool_not_empty")

	// Deleting the snapshot returns its capacity; the pool can then be removed.
	if rec := do(h, http.MethodDelete, "/v1/snapshots/snap-1", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete snapshot = %d %s", rec.Code, rec.Body.String())
	}
	wantError(t, do(h, http.MethodGet, "/v1/snapshots/snap-1", ""), http.StatusNotFound, "snapshot_not_found")
	wantError(t, do(h, http.MethodDelete, "/v1/snapshots/snap-1", ""), http.StatusNotFound, "snapshot_not_found")
	if p := poolViewOf(t, h, "pool-a"); p["allocatedBytes"].(float64) != 0 || p["availableBytes"].(float64) != 1000 {
		t.Fatalf("pool after snapshot delete = %v", p)
	}
	if rec := do(h, http.MethodDelete, "/v1/storage-pools/pool-a", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete pool after snapshot = %d %s", rec.Code, rec.Body.String())
	}
}

func TestSnapshotCapturesGeneration(t *testing.T) {
	h := Handler()
	snapshotFixture(t, h)
	// vol1 at generation 0 cannot be snapshotted expecting generation 1...
	wantError(t, postSnapshot(t, h, "vol1", `{"id":"snap-before","expectedGeneration":1}`),
		http.StatusConflict, "stale_generation")
	// ...but after a bind/unbind cycle a generation-2 snapshot records it.
	do(h, http.MethodPut, "/v1/volumes/vol1/binding", `{"nodeId":"node-1","expectedGeneration":0}`)
	do(h, http.MethodDelete, "/v1/volumes/vol1/binding?expectedGeneration=1", "")
	rec := createSnap(t, h, "vol1", `{"id":"snap-g2","expectedGeneration":2}`)
	if s := decodeBody(t, rec); s["sourceGeneration"].(float64) != 2 {
		t.Fatalf("sourceGeneration = %v", s["sourceGeneration"])
	}
}

func TestCloneLifecycle(t *testing.T) {
	h := Handler()
	snapshotFixture(t, h)
	createSnap(t, h, "vol1", `{"id":"snap-1","expectedGeneration":0}`)

	// Cross-pool clone into pool-b.
	rec := cloneFrom(t, h, "snap-1", `{"id":"vol-copy","poolId":"pool-b"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("clone status = %d %s", rec.Code, rec.Body.String())
	}
	v := decodeBody(t, rec)
	if v["id"] != "vol-copy" || v["poolId"] != "pool-b" || v["sizeBytes"].(float64) != 300 {
		t.Fatalf("clone view = %v", v)
	}
	if v["generation"].(float64) != 0 || v["binding"] != nil {
		t.Fatalf("clone generation/binding = %v", v)
	}
	// Clone charges the target pool; source pool allocation is unchanged.
	if p := poolViewOf(t, h, "pool-b"); p["allocatedBytes"].(float64) != 300 || p["availableBytes"].(float64) != 700 {
		t.Fatalf("target pool after clone = %v", p)
	}
	if p := poolViewOf(t, h, "pool-a"); p["allocatedBytes"].(float64) != 600 {
		t.Fatalf("source pool after clone = %v", p)
	}

	// The clone is an independent volume: binding works and deleting it
	// returns capacity to pool-b, with no effect on the snapshot.
	if rec := do(h, http.MethodPut, "/v1/volumes/vol-copy/binding", `{"nodeId":"node-9","expectedGeneration":0}`); rec.Code != http.StatusOK {
		t.Fatalf("bind clone = %d %s", rec.Code, rec.Body.String())
	}
	wantError(t, do(h, http.MethodDelete, "/v1/volumes/vol-copy", ""), http.StatusConflict, "volume_in_use")
	if rec := do(h, http.MethodDelete, "/v1/volumes/vol-copy/binding?expectedGeneration=1", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("unbind clone = %d", rec.Code)
	}
	if rec := do(h, http.MethodDelete, "/v1/volumes/vol-copy", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete clone = %d", rec.Code)
	}
	if p := poolViewOf(t, h, "pool-b"); p["allocatedBytes"].(float64) != 0 {
		t.Fatalf("target pool after clone delete = %v", p)
	}
	if s := decodeBody(t, do(h, http.MethodGet, "/v1/snapshots/snap-1", "")); s["sizeBytes"].(float64) != 300 {
		t.Fatalf("snapshot after clone delete = %v", s)
	}

	// In-pool clone is allowed too.
	rec = cloneFrom(t, h, "snap-1", `{"id":"vol-local","poolId":"pool-a"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("in-pool clone = %d %s", rec.Code, rec.Body.String())
	}
}

func TestCloneIdempotencyAndConflicts(t *testing.T) {
	h := Handler()
	snapshotFixture(t, h)
	createSnap(t, h, "vol1", `{"id":"snap-1","expectedGeneration":0}`)
	createSnap(t, h, "vol1", `{"id":"snap-2","expectedGeneration":0}`)
	clone := func(snapID, body string) *httptest.ResponseRecorder {
		return cloneFrom(t, h, snapID, body)
	}

	// Retry of the same snapshot/volume/pool: 200, no extra charge.
	rec := clone("snap-1", `{"id":"vol-copy","poolId":"pool-b"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("first clone = %d %s", rec.Code, rec.Body.String())
	}
	rec = clone("snap-1", `{"id":"vol-copy","poolId":"pool-b"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("retry clone = %d %s", rec.Code, rec.Body.String())
	}
	if p := poolViewOf(t, h, "pool-b"); p["allocatedBytes"].(float64) != 300 {
		t.Fatalf("double charge on retry: %v", p["allocatedBytes"])
	}

	// Same volume id against a different snapshot or target pool: 409.
	wantError(t, clone("snap-2", `{"id":"vol-copy","poolId":"pool-b"}`), http.StatusConflict, "volume_exists")
	wantError(t, clone("snap-1", `{"id":"vol-copy","poolId":"pool-a"}`), http.StatusConflict, "volume_exists")

	// Volume id minted by an ordinary create: 409 volume_exists.
	createVol(t, h, `{"id":"plain","poolId":"pool-b","sizeBytes":1}`)
	wantError(t, clone("snap-1", `{"id":"plain","poolId":"pool-b"}`), http.StatusConflict, "volume_exists")

	// Unknown snapshot and unknown target pool.
	wantError(t, clone("nope", `{"id":"vol-x","poolId":"pool-b"}`), http.StatusNotFound, "snapshot_not_found")
	wantError(t, clone("snap-1", `{"id":"vol-x","poolId":"nope"}`), http.StatusNotFound, "pool_not_found")

	// Fill pool-b (vol-copy 300 + plain 1 already used); a further 300-byte
	// clone must fail and leave no volume or allocation behind.
	createVol(t, h, `{"id":"fill","poolId":"pool-b","sizeBytes":699}`)
	wantError(t, clone("snap-1", `{"id":"vol-big2","poolId":"pool-b"}`),
		http.StatusConflict, "insufficient_capacity")
	if p := poolViewOf(t, h, "pool-b"); p["allocatedBytes"].(float64) != 1000 {
		t.Fatalf("partial clone allocation: %v", p["allocatedBytes"])
	}
	for _, it := range decodeBody(t, do(h, http.MethodGet, "/v1/volumes", ""))["items"].([]any) {
		if id := it.(map[string]any)["id"].(string); id == "vol-big2" {
			t.Fatalf("partial clone volume left behind: %s", id)
		}
	}
}

func TestCloneInvalidRequests(t *testing.T) {
	h := Handler()
	snapshotFixture(t, h)
	createSnap(t, h, "vol1", `{"id":"snap-1","expectedGeneration":0}`)
	for name, body := range map[string]string{
		"unknown field": `{"id":"vol-copy","poolId":"pool-b","extra":1}`,
		"missing id":    `{"poolId":"pool-b"}`,
		"empty id":      `{"id":"","poolId":"pool-b"}`,
		"bad id":        `{"id":"bad copy","poolId":"pool-b"}`,
		"missing pool":  `{"id":"vol-copy"}`,
		"empty pool":    `{"id":"vol-copy","poolId":""}`,
		"bad pool":      `{"id":"vol-copy","poolId":"bad pool"}`,
		"malformed":     `{`,
	} {
		t.Run(name, func(t *testing.T) {
			wantError(t, cloneFrom(t, h, "snap-1", body), http.StatusBadRequest, "invalid_request")
		})
	}
	wantError(t, cloneFrom(t, h, "bad%20id", `{"id":"vol-copy","poolId":"pool-b"}`),
		http.StatusBadRequest, "invalid_request")
}

func TestSnapshotRouting(t *testing.T) {
	h := Handler()
	snapshotFixture(t, h)
	createSnap(t, h, "vol1", `{"id":"snap-1","expectedGeneration":0}`)

	rec := do(h, http.MethodPost, "/v1/snapshots", "")
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET" {
		t.Fatalf("POST snapshot collection = %d allow=%q", rec.Code, rec.Header().Get("Allow"))
	}
	rec = do(h, http.MethodPut, "/v1/snapshots/snap-1", "")
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET, DELETE" {
		t.Fatalf("PUT snapshot member = %d allow=%q", rec.Code, rec.Header().Get("Allow"))
	}
	rec = do(h, http.MethodGet, "/v1/snapshots/snap-1/clones", "")
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "POST" {
		t.Fatalf("GET clones = %d allow=%q", rec.Code, rec.Header().Get("Allow"))
	}
	rec = do(h, http.MethodGet, "/v1/volumes/vol1/snapshots", "")
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "POST" {
		t.Fatalf("GET volume snapshots = %d allow=%q", rec.Code, rec.Header().Get("Allow"))
	}
	wantError(t, do(h, http.MethodGet, "/v1/snapshots/snap-1/other", ""), http.StatusNotFound, "not_found")
	wantError(t, do(h, http.MethodGet, "/v1/volumes/vol1/snapshots/extra", ""), http.StatusNotFound, "not_found")
}

func TestConcurrentSnapshotCreates(t *testing.T) {
	h := Handler()
	createPool(t, h, `{"id":"cp","devices":[{"id":"d","capacityBytes":100,"faultDomain":"fd"}]}`)
	createVol(t, h, `{"id":"vol1","poolId":"cp","sizeBytes":10}`)

	const goroutines = 50
	var wg sync.WaitGroup
	var mu sync.Mutex
	var created, ok, rejected int
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			rec := postSnapshot(t, h, "vol1", `{"id":"same","expectedGeneration":0}`)
			mu.Lock()
			switch rec.Code {
			case http.StatusCreated:
				created++
			case http.StatusOK:
				ok++
			case http.StatusConflict:
				rejected++
			default:
				t.Errorf("unexpected status %d", rec.Code)
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	if created != 1 || ok != goroutines-1 {
		t.Fatalf("created=%d ok=%d rejected=%d, want 1/%d/0", created, ok, rejected, goroutines-1)
	}
	if p := poolViewOf(t, h, "cp"); p["allocatedBytes"].(float64) != 20 {
		t.Fatalf("pool after concurrent same-id snapshots = %v", p)
	}
}

func TestConcurrentSnapshotCapacityNoOvercommit(t *testing.T) {
	h := Handler()
	createPool(t, h, `{"id":"cp","devices":[{"id":"d","capacityBytes":100,"faultDomain":"fd"}]}`)
	createVol(t, h, `{"id":"vol1","poolId":"cp","sizeBytes":10}`)

	const goroutines = 50
	var wg sync.WaitGroup
	var mu sync.Mutex
	var created, rejected int64
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		i := i
		go func() {
			defer wg.Done()
			body := fmt.Sprintf(`{"id":"snap-%d","expectedGeneration":0}`, i)
			rec := postSnapshot(t, h, "vol1", body)
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
		}()
	}
	wg.Wait()
	// Volume takes 10; nine more 10-byte snapshots fill the remaining 90.
	if created != 9 || rejected != goroutines-9 {
		t.Fatalf("created=%d rejected=%d, want 9/%d", created, rejected, goroutines-9)
	}
	if p := poolViewOf(t, h, "cp"); p["allocatedBytes"].(float64) != 100 || p["availableBytes"].(float64) != 0 {
		t.Fatalf("pool overcommitted: %v", p)
	}
}

func TestConcurrentClonesNoOvercommit(t *testing.T) {
	h := Handler()
	createPool(t, h, `{"id":"cp","devices":[{"id":"d1","capacityBytes":100,"faultDomain":"fd"}]}`)
	createPool(t, h, `{"id":"tp","devices":[{"id":"d2","capacityBytes":100,"faultDomain":"fd2"}]}`)
	createVol(t, h, `{"id":"vol1","poolId":"cp","sizeBytes":10}`)
	createSnap(t, h, "vol1", `{"id":"snap-1","expectedGeneration":0}`)

	const goroutines = 50
	var wg sync.WaitGroup
	var mu sync.Mutex
	var created, ok200, rejected int
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			rec := cloneFrom(t, h, "snap-1", `{"id":"same-copy","poolId":"tp"}`)
			mu.Lock()
			switch rec.Code {
			case http.StatusCreated:
				created++
			case http.StatusOK:
				ok200++
			case http.StatusConflict:
				rejected++
			default:
				t.Errorf("unexpected status %d", rec.Code)
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	if created != 1 || ok200 != goroutines-1 {
		t.Fatalf("created=%d ok=%d rejected=%d", created, ok200, rejected)
	}
	if p := poolViewOf(t, h, "tp"); p["allocatedBytes"].(float64) != 10 {
		t.Fatalf("target pool after retries = %v", p)
	}
}

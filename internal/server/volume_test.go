package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func createVolume(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := do(h, http.MethodPost, "/v1/volumes", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create volume status = %d body = %s", rec.Code, rec.Body.String())
	}
	return rec
}

func TestCreateVolumeRepresentation(t *testing.T) {
	h := Handler()
	createPool(t, h, poolA)

	rec := createVolume(t, h, `{"id":"vol-1","poolId":"pool-a","sizeBytes":400}`)
	v := decodeBody(t, rec)
	if v["id"] != "vol-1" || v["poolId"] != "pool-a" || v["sizeBytes"].(float64) != 400 {
		t.Fatalf("volume view = %v", v)
	}
	if v["generation"].(float64) != 0 {
		t.Fatalf("generation = %v, want 0", v["generation"])
	}
	if b, ok := v["binding"]; !ok || b != nil {
		t.Fatalf("binding = %v, want null", v["binding"])
	}

	// Volume capacity counts into the pool immediately.
	p := decodeBody(t, do(h, http.MethodGet, "/v1/storage-pools/pool-a", ""))
	if p["allocatedBytes"].(float64) != 400 || p["availableBytes"].(float64) != 1100 {
		t.Fatalf("pool after create = %v", p)
	}
}

func TestCreateVolumeIdempotency(t *testing.T) {
	h := Handler()
	createPool(t, h, poolA)
	createVolume(t, h, `{"id":"vol-1","poolId":"pool-a","sizeBytes":400}`)

	// Same id, same parameters: 200 with the current result, no double count.
	rec := do(h, http.MethodPost, "/v1/volumes", `{"id":"vol-1","poolId":"pool-a","sizeBytes":400}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("retry status = %d %s", rec.Code, rec.Body.String())
	}
	p := decodeBody(t, do(h, http.MethodGet, "/v1/storage-pools/pool-a", ""))
	if p["allocatedBytes"].(float64) != 400 {
		t.Fatalf("allocated changed after retry: %v", p["allocatedBytes"])
	}

	// Same id, different parameters: conflict.
	wantError(t, do(h, http.MethodPost, "/v1/volumes", `{"id":"vol-1","poolId":"pool-a","sizeBytes":401}`),
		http.StatusConflict, "volume_exists")
	wantError(t, do(h, http.MethodPost, "/v1/volumes", `{"id":"vol-1","poolId":"other","sizeBytes":400}`),
		http.StatusNotFound, "pool_not_found")
}

func TestCreateVolumeErrors(t *testing.T) {
	h := Handler()
	createPool(t, h, poolA)

	wantError(t, do(h, http.MethodPost, "/v1/volumes", `{"id":"v1","poolId":"missing","sizeBytes":1}`),
		http.StatusNotFound, "pool_not_found")
	wantError(t, do(h, http.MethodPost, "/v1/volumes", `{"id":"v1","poolId":"pool-a","sizeBytes":1501}`),
		http.StatusConflict, "insufficient_capacity")

	for _, body := range []string{
		`{"id":"bad id","poolId":"pool-a","sizeBytes":1}`,
		`{"id":"v1","poolId":"bad id","sizeBytes":1}`,
		`{"id":"v1","poolId":"pool-a","sizeBytes":0}`,
		`{"id":"v1","poolId":"pool-a","sizeBytes":-1}`,
		`{"id":"v1","poolId":"pool-a","sizeBytes":1.5}`,
		`{"id":"v1","poolId":"pool-a","sizeBytes":"1"}`,
		`{"id":"v1","poolId":"pool-a"}`,
		`{"id":"v1","sizeBytes":1}`,
		`{"poolId":"pool-a","sizeBytes":1}`,
		`{"id":"v1","poolId":"pool-a","sizeBytes":1,"extra":2}`,
		`not json`,
	} {
		wantError(t, do(h, http.MethodPost, "/v1/volumes", body), http.StatusBadRequest, "invalid_request")
	}

	// No partial state: no volume, no allocation.
	if rec := do(h, http.MethodGet, "/v1/volumes", ""); rec.Body.String() != "{\"items\":[]}\n" {
		t.Fatalf("partial state after failures: %s", rec.Body.String())
	}
	p := decodeBody(t, do(h, http.MethodGet, "/v1/storage-pools/pool-a", ""))
	if p["allocatedBytes"].(float64) != 0 {
		t.Fatalf("allocated after failures = %v", p["allocatedBytes"])
	}
}

func TestListAndGetVolumes(t *testing.T) {
	h := Handler()
	createPool(t, h, poolA)
	createVolume(t, h, `{"id":"zeta","poolId":"pool-a","sizeBytes":10}`)
	createVolume(t, h, `{"id":"alpha","poolId":"pool-a","sizeBytes":10}`)

	rec := do(h, http.MethodGet, "/v1/volumes", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d", rec.Code)
	}
	items := decodeBody(t, rec)["items"].([]any)
	if len(items) != 2 || items[0].(map[string]any)["id"] != "alpha" || items[1].(map[string]any)["id"] != "zeta" {
		t.Fatalf("items not sorted by id: %v", items)
	}

	rec = do(h, http.MethodGet, "/v1/volumes/alpha", "")
	if rec.Code != http.StatusOK || decodeBody(t, rec)["id"] != "alpha" {
		t.Fatalf("get volume failed: %d %s", rec.Code, rec.Body.String())
	}
	wantError(t, do(h, http.MethodGet, "/v1/volumes/missing", ""), http.StatusNotFound, "volume_not_found")
	wantError(t, do(h, http.MethodGet, "/v1/volumes/bad%20id", ""), http.StatusBadRequest, "invalid_request")
}

func bind(t *testing.T, h http.Handler, id, body string) *httptest.ResponseRecorder {
	t.Helper()
	return do(h, http.MethodPut, "/v1/volumes/"+id+"/binding", body)
}

func TestBindingLifecycle(t *testing.T) {
	h := Handler()
	createPool(t, h, poolA)
	createVolume(t, h, `{"id":"vol-1","poolId":"pool-a","sizeBytes":100}`)

	// Bind: 200, generation 1, binding records the node.
	rec := bind(t, h, "vol-1", `{"nodeId":"node-1","expectedGeneration":0}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("bind status = %d %s", rec.Code, rec.Body.String())
	}
	v := decodeBody(t, rec)
	if v["generation"].(float64) != 1 {
		t.Fatalf("generation after bind = %v", v["generation"])
	}
	b := v["binding"].(map[string]any)
	if b["nodeId"] != "node-1" {
		t.Fatalf("binding = %v", b)
	}

	// Same node, matching generation: idempotent, no increment.
	rec = bind(t, h, "vol-1", `{"nodeId":"node-1","expectedGeneration":1}`)
	if rec.Code != http.StatusOK || decodeBody(t, rec)["generation"].(float64) != 1 {
		t.Fatalf("idempotent rebind = %d %s", rec.Code, rec.Body.String())
	}

	// Different node: conflict.
	wantError(t, bind(t, h, "vol-1", `{"nodeId":"node-2","expectedGeneration":1}`),
		http.StatusConflict, "volume_already_bound")

	// Stale generation.
	wantError(t, bind(t, h, "vol-1", `{"nodeId":"node-1","expectedGeneration":0}`),
		http.StatusConflict, "stale_generation")

	// Bound volume cannot be deleted.
	wantError(t, do(h, http.MethodDelete, "/v1/volumes/vol-1", ""), http.StatusConflict, "volume_in_use")

	// Unbind with the wrong generation fails.
	wantError(t, do(h, http.MethodDelete, "/v1/volumes/vol-1/binding?expectedGeneration=0", ""),
		http.StatusConflict, "stale_generation")

	// Unbind: 204, generation advances to 2.
	rec = do(h, http.MethodDelete, "/v1/volumes/vol-1/binding?expectedGeneration=1", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("unbind status = %d %s", rec.Code, rec.Body.String())
	}
	v = decodeBody(t, do(h, http.MethodGet, "/v1/volumes/vol-1", ""))
	if v["generation"].(float64) != 2 || v["binding"] != nil {
		t.Fatalf("after unbind = %v", v)
	}

	// Unbinding an unbound volume: 204, generation unchanged.
	rec = do(h, http.MethodDelete, "/v1/volumes/vol-1/binding?expectedGeneration=2", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("unbind unbound = %d", rec.Code)
	}
	v = decodeBody(t, do(h, http.MethodGet, "/v1/volumes/vol-1", ""))
	if v["generation"].(float64) != 2 {
		t.Fatalf("generation changed on no-op unbind: %v", v)
	}

	// Rebind after unbind works with the new generation.
	rec = bind(t, h, "vol-1", `{"nodeId":"node-2","expectedGeneration":2}`)
	if rec.Code != http.StatusOK || decodeBody(t, rec)["generation"].(float64) != 3 {
		t.Fatalf("rebind = %d %s", rec.Code, rec.Body.String())
	}
}

func TestBindingValidation(t *testing.T) {
	h := Handler()
	createPool(t, h, poolA)
	createVolume(t, h, `{"id":"vol-1","poolId":"pool-a","sizeBytes":100}`)

	wantError(t, bind(t, h, "missing", `{"nodeId":"n1","expectedGeneration":0}`),
		http.StatusNotFound, "volume_not_found")
	wantError(t, do(h, http.MethodDelete, "/v1/volumes/missing/binding?expectedGeneration=0", ""),
		http.StatusNotFound, "volume_not_found")

	for _, body := range []string{
		`{"nodeId":"bad id","expectedGeneration":0}`,
		`{"nodeId":"n1","expectedGeneration":-1}`,
		`{"nodeId":"n1","expectedGeneration":0.5}`,
		`{"nodeId":"n1"}`,
		`{"expectedGeneration":0}`,
		`{"nodeId":"n1","expectedGeneration":0,"extra":1}`,
		`not json`,
	} {
		wantError(t, bind(t, h, "vol-1", body), http.StatusBadRequest, "invalid_request")
	}

	for _, target := range []string{
		"/v1/volumes/vol-1/binding",
		"/v1/volumes/vol-1/binding?expectedGeneration=",
		"/v1/volumes/vol-1/binding?expectedGeneration=abc",
		"/v1/volumes/vol-1/binding?expectedGeneration=1.5",
		"/v1/volumes/vol-1/binding?expectedGeneration=-1",
		"/v1/volumes/vol-1/binding?expectedGeneration=+1",
		"/v1/volumes/vol-1/binding?expectedGeneration=0&expectedGeneration=0",
		"/v1/volumes/vol-1/binding?expectedGeneration=0&other=1",
		"/v1/volumes/vol-1/binding?other=1",
	} {
		wantError(t, do(h, http.MethodDelete, target, ""), http.StatusBadRequest, "invalid_request")
	}

	// Failed binds/unbinds left the volume untouched.
	v := decodeBody(t, do(h, http.MethodGet, "/v1/volumes/vol-1", ""))
	if v["generation"].(float64) != 0 || v["binding"] != nil {
		t.Fatalf("state after invalid requests = %v", v)
	}
}

func TestDeleteVolume(t *testing.T) {
	h := Handler()
	createPool(t, h, poolA)
	createVolume(t, h, `{"id":"vol-1","poolId":"pool-a","sizeBytes":400}`)

	wantError(t, do(h, http.MethodDelete, "/v1/volumes/missing", ""), http.StatusNotFound, "volume_not_found")

	rec := do(h, http.MethodDelete, "/v1/volumes/vol-1", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete volume = %d %s", rec.Code, rec.Body.String())
	}
	// Capacity is returned to the pool.
	p := decodeBody(t, do(h, http.MethodGet, "/v1/storage-pools/pool-a", ""))
	if p["allocatedBytes"].(float64) != 0 || p["availableBytes"].(float64) != 1500 {
		t.Fatalf("pool after delete = %v", p)
	}
	wantError(t, do(h, http.MethodGet, "/v1/volumes/vol-1", ""), http.StatusNotFound, "volume_not_found")

	// The id can be reused after deletion.
	createVolume(t, h, `{"id":"vol-1","poolId":"pool-a","sizeBytes":600}`)
}

func TestDeletePoolWithVolumes(t *testing.T) {
	h := Handler()
	createPool(t, h, poolA)
	createVolume(t, h, `{"id":"vol-1","poolId":"pool-a","sizeBytes":100}`)

	wantError(t, do(h, http.MethodDelete, "/v1/storage-pools/pool-a", ""), http.StatusConflict, "pool_not_empty")

	if rec := do(h, http.MethodDelete, "/v1/volumes/vol-1", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete volume = %d", rec.Code)
	}
	if rec := do(h, http.MethodDelete, "/v1/storage-pools/pool-a", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete pool = %d %s", rec.Code, rec.Body.String())
	}
}

func TestVolumeSharesCapacityWithReservations(t *testing.T) {
	h := Handler()
	createPool(t, h, poolA)
	createVolume(t, h, `{"id":"vol-1","poolId":"pool-a","sizeBytes":1000}`)

	// Only 500 bytes remain for reservations.
	wantError(t, reserve(t, h, "pool-a", `{"requestId":"r1","bytes":501}`), http.StatusConflict, "insufficient_capacity")
	if rec := reserve(t, h, "pool-a", `{"requestId":"r1","bytes":500}`); rec.Code != http.StatusCreated {
		t.Fatalf("reserve = %d %s", rec.Code, rec.Body.String())
	}
	// And nothing remains for a second volume.
	wantError(t, do(h, http.MethodPost, "/v1/volumes", `{"id":"vol-2","poolId":"pool-a","sizeBytes":1}`),
		http.StatusConflict, "insufficient_capacity")
}

func TestConcurrentVolumesNeverOvercommit(t *testing.T) {
	h := Handler()
	createPool(t, h, `{"id":"cp","devices":[{"id":"d","capacityBytes":100,"faultDomain":"fd"}]}`)

	const goroutines = 50
	var wg sync.WaitGroup
	var created, rejected int64
	var mu sync.Mutex
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(i int) {
			defer wg.Done()
			body := fmt.Sprintf(`{"id":"vol-%d","poolId":"cp","sizeBytes":10}`, i)
			rec := do(h, http.MethodPost, "/v1/volumes", body)
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
	p := decodeBody(t, do(h, http.MethodGet, "/v1/storage-pools/cp", ""))
	if p["allocatedBytes"].(float64) != 100 || p["availableBytes"].(float64) != 0 {
		t.Fatalf("post-concurrency pool = %v", p)
	}
}

func TestConcurrentBindingSingleWinner(t *testing.T) {
	h := Handler()
	createPool(t, h, poolA)
	createVolume(t, h, `{"id":"vol-1","poolId":"pool-a","sizeBytes":10}`)

	const goroutines = 30
	var wg sync.WaitGroup
	statuses := make(chan int, goroutines)
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(i int) {
			defer wg.Done()
			body := fmt.Sprintf(`{"nodeId":"node-%d","expectedGeneration":0}`, i)
			statuses <- bind(t, h, "vol-1", body).Code
		}(i)
	}
	wg.Wait()
	close(statuses)
	var ok, conflict int
	for code := range statuses {
		switch code {
		case http.StatusOK:
			ok++
		case http.StatusConflict:
			conflict++
		default:
			t.Fatalf("unexpected status %d", code)
		}
	}
	if ok != 1 || conflict != goroutines-1 {
		t.Fatalf("ok=%d conflict=%d, want 1/%d", ok, conflict, goroutines-1)
	}
	v := decodeBody(t, do(h, http.MethodGet, "/v1/volumes/vol-1", ""))
	if v["generation"].(float64) != 1 {
		t.Fatalf("generation = %v, want 1", v["generation"])
	}
}

func TestVolumeRouting(t *testing.T) {
	h := Handler()

	rec := do(h, http.MethodPut, "/v1/volumes", "")
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET, POST" {
		t.Fatalf("PUT collection = %d allow=%q", rec.Code, rec.Header().Get("Allow"))
	}
	rec = do(h, http.MethodPost, "/v1/volumes/v1", "")
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET, DELETE" {
		t.Fatalf("POST member = %d allow=%q", rec.Code, rec.Header().Get("Allow"))
	}
	rec = do(h, http.MethodGet, "/v1/volumes/v1/binding", "")
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "PUT, DELETE" {
		t.Fatalf("GET binding = %d allow=%q", rec.Code, rec.Header().Get("Allow"))
	}

	for _, target := range []string{"/v1/volumes/", "/v1/volumes/v1/binding/", "/v1/volumes/v1/extra", "/v1/volumes/v1/binding/x"} {
		wantError(t, do(h, http.MethodGet, target, ""), http.StatusNotFound, "not_found")
	}
}

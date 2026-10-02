package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

const volPool = `{"id":"vp","devices":[{"id":"vdev","capacityBytes":1000,"faultDomain":"rack1"}]}`

func createVol(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := do(h, http.MethodPost, "/v1/volumes", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create volume status = %d body = %s", rec.Code, rec.Body.String())
	}
	return rec
}

func TestCreateVolumeRepresentation(t *testing.T) {
	h := Handler()
	createPool(t, h, volPool)

	rec := createVol(t, h, `{"id":"vol1","poolId":"vp","sizeBytes":300}`)
	v := decodeBody(t, rec)
	if v["id"] != "vol1" || v["poolId"] != "vp" || v["sizeBytes"].(float64) != 300 {
		t.Fatalf("volume view = %v", v)
	}
	if v["generation"].(float64) != 0 {
		t.Fatalf("initial generation = %v", v["generation"])
	}
	if binding, ok := v["binding"]; !ok || binding != nil {
		t.Fatalf("initial binding = %v", binding)
	}

	// Volume size counts toward the owning pool's allocation.
	p := decodeBody(t, do(h, http.MethodGet, "/v1/storage-pools/vp", ""))
	if p["allocatedBytes"].(float64) != 300 || p["availableBytes"].(float64) != 700 {
		t.Fatalf("pool after volume = %v", p)
	}
}

func TestCreateVolumeIdempotencyAndConflicts(t *testing.T) {
	h := Handler()
	createPool(t, h, volPool)
	createVol(t, h, `{"id":"vol1","poolId":"vp","sizeBytes":300}`)

	// Identical retry: 200, same representation, no double count.
	rec := do(h, http.MethodPost, "/v1/volumes", `{"id":"vol1","poolId":"vp","sizeBytes":300}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("retry status = %d %s", rec.Code, rec.Body.String())
	}
	v := decodeBody(t, rec)
	if v["generation"].(float64) != 0 || v["binding"] != nil {
		t.Fatalf("retry view = %v", v)
	}
	p := decodeBody(t, do(h, http.MethodGet, "/v1/storage-pools/vp", ""))
	if p["allocatedBytes"].(float64) != 300 {
		t.Fatalf("allocated after retry = %v", p["allocatedBytes"])
	}

	// Different parameters: 409 volume_exists.
	wantError(t, do(h, http.MethodPost, "/v1/volumes", `{"id":"vol1","poolId":"vp","sizeBytes":301}`),
		http.StatusConflict, "volume_exists")
	wantError(t, do(h, http.MethodPost, "/v1/volumes", `{"id":"vol1","poolId":"other","sizeBytes":300}`),
		http.StatusConflict, "volume_exists")

	// Unknown pool: 404 pool_not_found.
	wantError(t, do(h, http.MethodPost, "/v1/volumes", `{"id":"vol2","poolId":"nope","sizeBytes":1}`),
		http.StatusNotFound, "pool_not_found")

	// Insufficient capacity leaves no volume and no allocation behind.
	wantError(t, do(h, http.MethodPost, "/v1/volumes", `{"id":"big","poolId":"vp","sizeBytes":701}`),
		http.StatusConflict, "insufficient_capacity")
	rec = do(h, http.MethodGet, "/v1/volumes", "")
	items := decodeBody(t, rec)["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("partial volume after failure: %v", items)
	}
	p = decodeBody(t, do(h, http.MethodGet, "/v1/storage-pools/vp", ""))
	if p["allocatedBytes"].(float64) != 300 {
		t.Fatalf("partial allocation after failure: %v", p["allocatedBytes"])
	}

	// Exactly the remaining capacity fits.
	createVol(t, h, `{"id":"exact","poolId":"vp","sizeBytes":700}`)
	wantError(t, do(h, http.MethodPost, "/v1/volumes", `{"id":"one","poolId":"vp","sizeBytes":1}`),
		http.StatusConflict, "insufficient_capacity")
}

func TestCreateVolumeInvalidRequests(t *testing.T) {
	h := Handler()
	createPool(t, h, volPool)
	for name, body := range map[string]string{
		"unknown field":  `{"id":"v","poolId":"vp","sizeBytes":1,"extra":1}`,
		"missing id":     `{"poolId":"vp","sizeBytes":1}`,
		"missing pool":   `{"id":"v","sizeBytes":1}`,
		"missing size":   `{"id":"v","poolId":"vp"}`,
		"bad volume id":  `{"id":"bad id","poolId":"vp","sizeBytes":1}`,
		"bad pool id":    `{"id":"v","poolId":"bad pool","sizeBytes":1}`,
		"zero size":      `{"id":"v","poolId":"vp","sizeBytes":0}`,
		"negative size":  `{"id":"v","poolId":"vp","sizeBytes":-1}`,
		"float size":     `{"id":"v","poolId":"vp","sizeBytes":1.5}`,
		"string size":    `{"id":"v","poolId":"vp","sizeBytes":"1"}`,
		"malformed json": `{"id":"v",`,
	} {
		t.Run(name, func(t *testing.T) {
			wantError(t, do(h, http.MethodPost, "/v1/volumes", body), http.StatusBadRequest, "invalid_request")
		})
	}
	// No partial state from any failed create.
	if rec := do(h, http.MethodGet, "/v1/volumes", ""); rec.Body.String() != "{\"items\":[]}\n" {
		t.Fatalf("partial state: %s", rec.Body.String())
	}
}

func TestListAndGetVolumes(t *testing.T) {
	h := Handler()
	createPool(t, h, volPool)
	if rec := do(h, http.MethodGet, "/v1/volumes", ""); rec.Body.String() != "{\"items\":[]}\n" {
		t.Fatalf("empty list = %q", rec.Body.String())
	}
	createVol(t, h, `{"id":"zeta","poolId":"vp","sizeBytes":1}`)
	createVol(t, h, `{"id":"alpha","poolId":"vp","sizeBytes":1}`)
	createVol(t, h, `{"id":"mid","poolId":"vp","sizeBytes":1}`)

	rec := do(h, http.MethodGet, "/v1/volumes", "")
	items := decodeBody(t, rec)["items"].([]any)
	got := []string{
		items[0].(map[string]any)["id"].(string),
		items[1].(map[string]any)["id"].(string),
		items[2].(map[string]any)["id"].(string),
	}
	if got[0] != "alpha" || got[1] != "mid" || got[2] != "zeta" {
		t.Fatalf("items not sorted: %v", got)
	}

	rec = do(h, http.MethodGet, "/v1/volumes/alpha", "")
	if rec.Code != http.StatusOK || decodeBody(t, rec)["id"] != "alpha" {
		t.Fatalf("get volume = %d %s", rec.Code, rec.Body.String())
	}
	wantError(t, do(h, http.MethodGet, "/v1/volumes/missing", ""), http.StatusNotFound, "volume_not_found")
	wantError(t, do(h, http.MethodGet, "/v1/volumes/bad%20id", ""), http.StatusBadRequest, "invalid_request")
}

func TestBindingLifecycle(t *testing.T) {
	h := Handler()
	createPool(t, h, volPool)
	createVol(t, h, `{"id":"vol1","poolId":"vp","sizeBytes":10}`)

	put := func(body string) *httptest.ResponseRecorder {
		return do(h, http.MethodPut, "/v1/volumes/vol1/binding", body)
	}

	// Bind node-a at generation 0: 200, binding set, generation bumps to 1.
	rec := put(`{"nodeId":"node-a","expectedGeneration":0}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("bind status = %d %s", rec.Code, rec.Body.String())
	}
	v := decodeBody(t, rec)
	if v["generation"].(float64) != 1 {
		t.Fatalf("generation after bind = %v", v["generation"])
	}
	binding := v["binding"].(map[string]any)
	if binding["nodeId"] != "node-a" {
		t.Fatalf("binding = %v", binding)
	}

	// Same node, matching generation: idempotent 200 without increment.
	rec = put(`{"nodeId":"node-a","expectedGeneration":1}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("rebind same status = %d", rec.Code)
	}
	if decodeBody(t, rec)["generation"].(float64) != 1 {
		t.Fatalf("generation changed on idempotent rebind: %s", rec.Body.String())
	}

	// Rebind to another node: 409 volume_already_bound.
	wantError(t, put(`{"nodeId":"node-b","expectedGeneration":1}`), http.StatusConflict, "volume_already_bound")

	// Stale generation: 409 stale_generation.
	wantError(t, put(`{"nodeId":"node-a","expectedGeneration":0}`), http.StatusConflict, "stale_generation")
	wantError(t, put(`{"nodeId":"node-b","expectedGeneration":0}`), http.StatusConflict, "stale_generation")

	// Bound volume cannot be deleted.
	wantError(t, do(h, http.MethodDelete, "/v1/volumes/vol1", ""), http.StatusConflict, "volume_in_use")

	// Unbind with the correct generation: 204, generation bumps to 2.
	rec = do(h, http.MethodDelete, "/v1/volumes/vol1/binding?expectedGeneration=1", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("unbind status = %d %s", rec.Code, rec.Body.String())
	}
	v = decodeBody(t, do(h, http.MethodGet, "/v1/volumes/vol1", ""))
	if v["generation"].(float64) != 2 || v["binding"] != nil {
		t.Fatalf("volume after unbind = %v", v)
	}

	// Unbinding an already-unbound volume at the matching generation: 204
	// without bumping the generation.
	rec = do(h, http.MethodDelete, "/v1/volumes/vol1/binding?expectedGeneration=2", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("repeat unbind = %d", rec.Code)
	}
	v = decodeBody(t, do(h, http.MethodGet, "/v1/volumes/vol1", ""))
	if v["generation"].(float64) != 2 {
		t.Fatalf("generation changed after unbound unbind: %v", v["generation"])
	}

	// Stale generation on unbind.
	wantError(t, do(h, http.MethodDelete, "/v1/volumes/vol1/binding?expectedGeneration=0", ""),
		http.StatusConflict, "stale_generation")

	// Once unbound, it can be rebound at the new generation, then deleted
	// only after a final unbind.
	rec = put(`{"nodeId":"node-c","expectedGeneration":2}`)
	if rec.Code != http.StatusOK || decodeBody(t, rec)["generation"].(float64) != 3 {
		t.Fatalf("rebind after unbind = %d %s", rec.Code, rec.Body.String())
	}
	if rec = do(h, http.MethodDelete, "/v1/volumes/vol1/binding?expectedGeneration=3", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("final unbind = %d", rec.Code)
	}
	if rec = do(h, http.MethodDelete, "/v1/volumes/vol1", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete volume = %d %s", rec.Code, rec.Body.String())
	}
	wantError(t, do(h, http.MethodGet, "/v1/volumes/vol1", ""), http.StatusNotFound, "volume_not_found")

	// Capacity is returned to the pool.
	p := decodeBody(t, do(h, http.MethodGet, "/v1/storage-pools/vp", ""))
	if p["allocatedBytes"].(float64) != 0 || p["availableBytes"].(float64) != 1000 {
		t.Fatalf("pool after delete = %v", p)
	}
}

func TestPutBindingInvalidRequests(t *testing.T) {
	h := Handler()
	createPool(t, h, volPool)
	createVol(t, h, `{"id":"vol1","poolId":"vp","sizeBytes":10}`)

	for name, body := range map[string]string{
		"unknown field":       `{"nodeId":"n1","expectedGeneration":0,"x":1}`,
		"missing nodeId":      `{"expectedGeneration":0}`,
		"missing generation":  `{"nodeId":"n1"}`,
		"empty nodeId":        `{"nodeId":"","expectedGeneration":0}`,
		"negative generation": `{"nodeId":"n1","expectedGeneration":-1}`,
		"float generation":    `{"nodeId":"n1","expectedGeneration":0.5}`,
		"string generation":   `{"nodeId":"n1","expectedGeneration":"0"}`,
		"null generation":     `{"nodeId":"n1","expectedGeneration":null}`,
		"malformed json":      `{`,
	} {
		t.Run(name, func(t *testing.T) {
			wantError(t, do(h, http.MethodPut, "/v1/volumes/vol1/binding", body),
				http.StatusBadRequest, "invalid_request")
		})
	}
	// No state changed: volume is still at generation 0, unbound.
	v := decodeBody(t, do(h, http.MethodGet, "/v1/volumes/vol1", ""))
	if v["generation"].(float64) != 0 || v["binding"] != nil {
		t.Fatalf("partial binding state: %v", v)
	}

	// Illegal path id and unknown volume.
	wantError(t, do(h, http.MethodPut, "/v1/volumes/bad%20id/binding", `{"nodeId":"n1","expectedGeneration":0}`),
		http.StatusBadRequest, "invalid_request")
	wantError(t, do(h, http.MethodPut, "/v1/volumes/nope/binding", `{"nodeId":"n1","expectedGeneration":0}`),
		http.StatusNotFound, "volume_not_found")
}

func TestDeleteBindingInvalidRequests(t *testing.T) {
	h := Handler()
	createPool(t, h, volPool)
	createVol(t, h, `{"id":"vol1","poolId":"vp","sizeBytes":10}`)

	for _, raw := range []string{
		"/v1/volumes/vol1/binding",
		"/v1/volumes/vol1/binding?",
		"/v1/volumes/vol1/binding?other=0",
		"/v1/volumes/vol1/binding?expectedGeneration",
		"/v1/volumes/vol1/binding?expectedGeneration=",
		"/v1/volumes/vol1/binding?expectedGeneration=-1",
		"/v1/volumes/vol1/binding?expectedGeneration=1.5",
		"/v1/volumes/vol1/binding?expectedGeneration=abc",
		"/v1/volumes/vol1/binding?expectedGeneration=0&expectedGeneration=0",
		"/v1/volumes/vol1/binding?expectedGeneration=0&other=1",
	} {
		wantError(t, do(h, http.MethodDelete, raw, ""), http.StatusBadRequest, "invalid_request")
	}
	// Unknown volume and bad path id still surface their own statuses.
	wantError(t, do(h, http.MethodDelete, "/v1/volumes/nope/binding?expectedGeneration=0", ""),
		http.StatusNotFound, "volume_not_found")
	wantError(t, do(h, http.MethodDelete, "/v1/volumes/bad%20id/binding?expectedGeneration=0", ""),
		http.StatusBadRequest, "invalid_request")
}

func TestDeleteVolumeRules(t *testing.T) {
	h := Handler()
	createPool(t, h, volPool)
	wantError(t, do(h, http.MethodDelete, "/v1/volumes/missing", ""), http.StatusNotFound, "volume_not_found")

	createVol(t, h, `{"id":"vol1","poolId":"vp","sizeBytes":10}`)
	if rec := do(h, http.MethodDelete, "/v1/volumes/vol1", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete unbound = %d %s", rec.Code, rec.Body.String())
	}
	wantError(t, do(h, http.MethodDelete, "/v1/volumes/vol1", ""), http.StatusNotFound, "volume_not_found")
}

func TestPoolNotEmptyWithVolume(t *testing.T) {
	h := Handler()
	createPool(t, h, volPool)
	createVol(t, h, `{"id":"vol1","poolId":"vp","sizeBytes":10}`)
	wantError(t, do(h, http.MethodDelete, "/v1/storage-pools/vp", ""), http.StatusConflict, "pool_not_empty")

	// Reservations and volumes share the same capacity budget.
	wantError(t, do(h, http.MethodPost, "/v1/storage-pools/vp/reservations",
		`{"requestId":"r1","bytes":991}`), http.StatusConflict, "insufficient_capacity")
	rec := do(h, http.MethodPost, "/v1/storage-pools/vp/reservations", `{"requestId":"r1","bytes":990}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("reserve alongside volume = %d %s", rec.Code, rec.Body.String())
	}
}

func TestVolumeRouting(t *testing.T) {
	h := Handler()
	createPool(t, h, volPool)

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
	// Unknown sub-paths stay 404.
	wantError(t, do(h, http.MethodGet, "/v1/volumes/v1/other", ""), http.StatusNotFound, "not_found")
	wantError(t, do(h, http.MethodGet, "/v1/volumes/v1/binding/extra", ""), http.StatusNotFound, "not_found")
}

func TestConcurrentVolumeCreatesNeverOvercommit(t *testing.T) {
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
			body := fmt.Sprintf(`{"id":"v%d","poolId":"cp","sizeBytes":10}`, i)
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

func TestConcurrentBindingOnlyOneSucceeds(t *testing.T) {
	h := Handler()
	createPool(t, h, volPool)
	createVol(t, h, `{"id":"vol1","poolId":"vp","sizeBytes":1}`)

	const goroutines = 30
	var wg sync.WaitGroup
	statuses := make(chan int, goroutines)
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			rec := do(h, http.MethodPut, "/v1/volumes/vol1/binding", `{"nodeId":"node-a","expectedGeneration":0}`)
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
	if v["generation"].(float64) != 1 || v["binding"].(map[string]any)["nodeId"] != "node-a" {
		t.Fatalf("volume after concurrent bind = %v", v)
	}
}

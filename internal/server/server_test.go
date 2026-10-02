package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestHealthzReportsOK(t *testing.T) {
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var payload map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("health payload is not JSON: %v", err)
	}
	if payload["status"] != "ok" || payload["service"] != "vaultgrid" || payload["version"] != Version {
		t.Fatalf("unexpected payload: %v", payload)
	}
}

func do(h http.Handler, method, target, body string) *httptest.ResponseRecorder {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, bytes.NewBufferString(body))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("body %q is not JSON: %v", rec.Body.String(), err)
	}
	return v
}

func wantError(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d (body=%s)", rec.Code, status, rec.Body.String())
	}
	v := decodeBody(t, rec)
	errObj, ok := v["error"].(map[string]any)
	if !ok || errObj["code"] != code {
		t.Fatalf("body = %s, want error.code %q", rec.Body.String(), code)
	}
}

const poolA = `{"id":"pool-a","devices":[{"id":"dev1","capacityBytes":1000,"faultDomain":"rack1"},{"id":"dev2","capacityBytes":500,"faultDomain":"rack2"}]}`

func createPool(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := do(h, http.MethodPost, "/v1/storage-pools", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create pool status = %d body = %s", rec.Code, rec.Body.String())
	}
	return rec
}

func TestCreatePoolRepresentation(t *testing.T) {
	h := Handler()
	rec := createPool(t, h, poolA)
	p := decodeBody(t, rec)
	if p["id"] != "pool-a" {
		t.Fatalf("id = %v", p["id"])
	}
	if p["rawCapacityBytes"].(float64) != 1500 || p["allocatedBytes"].(float64) != 0 || p["availableBytes"].(float64) != 1500 {
		t.Fatalf("capacity fields wrong: %v", p)
	}
	devs := p["devices"].([]any)
	if len(devs) != 2 {
		t.Fatalf("devices = %v", devs)
	}
	first := devs[0].(map[string]any)
	if first["id"] != "dev1" || first["capacityBytes"].(float64) != 1000 || first["faultDomain"] != "rack1" {
		t.Fatalf("device view wrong: %v", first)
	}
}

func TestListPoolsSorted(t *testing.T) {
	h := Handler()
	createPool(t, h, `{"id":"zeta","devices":[{"id":"z1","capacityBytes":10,"faultDomain":"fd"}]}`)
	createPool(t, h, `{"id":"alpha","devices":[{"id":"a1","capacityBytes":10,"faultDomain":"fd"}]}`)

	rec := do(h, http.MethodGet, "/v1/storage-pools", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d", rec.Code)
	}
	v := decodeBody(t, rec)
	items := v["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("items = %v", items)
	}
	if items[0].(map[string]any)["id"] != "alpha" || items[1].(map[string]any)["id"] != "zeta" {
		t.Fatalf("items not sorted by id: %v", items)
	}
}

func TestListPoolsEmpty(t *testing.T) {
	h := Handler()
	rec := do(h, http.MethodGet, "/v1/storage-pools", "")
	if rec.Body.String() != "{\"items\":[]}\n" {
		t.Fatalf("empty list body = %q", rec.Body.String())
	}
}

func TestGetPool(t *testing.T) {
	h := Handler()
	createPool(t, h, poolA)
	rec := do(h, http.MethodGet, "/v1/storage-pools/pool-a", "")
	if rec.Code != http.StatusOK || decodeBody(t, rec)["id"] != "pool-a" {
		t.Fatalf("get pool failed: %d %s", rec.Code, rec.Body.String())
	}
}

func TestPoolErrors(t *testing.T) {
	h := Handler()
	wantError(t, do(h, http.MethodGet, "/v1/storage-pools/missing", ""), http.StatusNotFound, "pool_not_found")
	wantError(t, do(h, http.MethodPost, "/v1/storage-pools/missing/reservations", `{"requestId":"r1","bytes":1}`), http.StatusNotFound, "pool_not_found")
	wantError(t, do(h, http.MethodDelete, "/v1/storage-pools/missing", ""), http.StatusNotFound, "pool_not_found")

	createPool(t, h, poolA)
	wantError(t, do(h, http.MethodPost, "/v1/storage-pools", poolA), http.StatusConflict, "pool_exists")

	// Device already owned by pool-a.
	wantError(t, do(h, http.MethodPost, "/v1/storage-pools",
		`{"id":"pool-b","devices":[{"id":"dev1","capacityBytes":1,"faultDomain":"fd"}]}`),
		http.StatusConflict, "device_in_use")
}

func TestCreatePoolInvalidRequests(t *testing.T) {
	cases := map[string]string{
		"unknown field":        `{"id":"p","devices":[{"id":"d","capacityBytes":1,"faultDomain":"fd"}],"extra":1}`,
		"unknown device field": `{"id":"p","devices":[{"id":"d","capacityBytes":1,"faultDomain":"fd","x":1}]}`,
		"empty devices":        `{"id":"p","devices":[]}`,
		"missing devices":      `{"id":"p"}`,
		"bad pool id":          `{"id":"bad id!","devices":[{"id":"d","capacityBytes":1,"faultDomain":"fd"}]}`,
		"id too long":          `{"id":"` + repeat("a", 65) + `","devices":[{"id":"d","capacityBytes":1,"faultDomain":"fd"}]}`,
		"bad device id":        `{"id":"p","devices":[{"id":"","capacityBytes":1,"faultDomain":"fd"}]}`,
		"zero capacity":        `{"id":"p","devices":[{"id":"d","capacityBytes":0,"faultDomain":"fd"}]}`,
		"negative capacity":    `{"id":"p","devices":[{"id":"d","capacityBytes":-5,"faultDomain":"fd"}]}`,
		"float capacity":       `{"id":"p","devices":[{"id":"d","capacityBytes":1.5,"faultDomain":"fd"}]}`,
		"string capacity":      `{"id":"p","devices":[{"id":"d","capacityBytes":"1","faultDomain":"fd"}]}`,
		"empty fault domain":   `{"id":"p","devices":[{"id":"d","capacityBytes":1,"faultDomain":""}]}`,
		"duplicate devices":    `{"id":"p","devices":[{"id":"d","capacityBytes":1,"faultDomain":"fd"},{"id":"d","capacityBytes":2,"faultDomain":"fd2"}]}`,
		"malformed json":       `{"id":"p",`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			h := Handler()
			wantError(t, do(h, http.MethodPost, "/v1/storage-pools", body), http.StatusBadRequest, "invalid_request")
			// No partial state: collection stays empty.
			rec := do(h, http.MethodGet, "/v1/storage-pools", "")
			if rec.Body.String() != "{\"items\":[]}\n" {
				t.Fatalf("partial state after failure: %s", rec.Body.String())
			}
		})
	}
}

func repeat(s string, n int) string {
	out := ""
	for i := 0; i < n; i++ {
		out += s
	}
	return out
}

func reserve(t *testing.T, h http.Handler, pool string, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := do(h, http.MethodPost, "/v1/storage-pools/"+pool+"/reservations", body)
	return rec
}

func TestReservationLifecycle(t *testing.T) {
	h := Handler()
	createPool(t, h, poolA)

	rec := reserve(t, h, "pool-a", `{"requestId":"r1","bytes":600}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("reserve status = %d %s", rec.Code, rec.Body.String())
	}
	v := decodeBody(t, rec)
	if v["requestId"] != "r1" || v["bytes"].(float64) != 600 || v["poolId"] != "pool-a" {
		t.Fatalf("reservation view = %v", v)
	}

	// Pool view reflects allocation immediately.
	p := decodeBody(t, do(h, http.MethodGet, "/v1/storage-pools/pool-a", ""))
	if p["allocatedBytes"].(float64) != 600 || p["availableBytes"].(float64) != 900 {
		t.Fatalf("pool after reserve = %v", p)
	}

	// Same requestId+bytes retried: 200, original result, no double count.
	rec = reserve(t, h, "pool-a", `{"requestId":"r1","bytes":600}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("retry status = %d", rec.Code)
	}
	p = decodeBody(t, do(h, http.MethodGet, "/v1/storage-pools/pool-a", ""))
	if p["allocatedBytes"].(float64) != 600 {
		t.Fatalf("allocated changed after retry: %v", p["allocatedBytes"])
	}

	// Same requestId, different bytes: conflict.
	wantError(t, reserve(t, h, "pool-a", `{"requestId":"r1","bytes":10}`), http.StatusConflict, "idempotency_conflict")

	// Insufficient capacity.
	wantError(t, reserve(t, h, "pool-a", `{"requestId":"r2","bytes":901}`), http.StatusConflict, "insufficient_capacity")

	// Exactly at the limit succeeds.
	rec = reserve(t, h, "pool-a", `{"requestId":"r2","bytes":900}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("edge reserve status = %d %s", rec.Code, rec.Body.String())
	}
	wantError(t, reserve(t, h, "pool-a", `{"requestId":"r3","bytes":1}`), http.StatusConflict, "insufficient_capacity")

	// Invalid reservation bodies.
	for _, body := range []string{
		`{"requestId":"bad id","bytes":1}`,
		`{"requestId":"r9","bytes":0}`,
		`{"requestId":"r9","bytes":-1}`,
		`{"requestId":"r9","bytes":1.5}`,
		`{"requestId":"","bytes":1}`,
		`{"bytes":1}`,
		`{"requestId":"r9"}`,
		`{"requestId":"r9","bytes":1,"extra":2}`,
		`not json`,
	} {
		wantError(t, reserve(t, h, "pool-a", body), http.StatusBadRequest, "invalid_request")
	}

	// Delete releases capacity; repeated delete still 204.
	rec = do(h, http.MethodDelete, "/v1/storage-pools/pool-a/reservations/r2", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete reservation = %d", rec.Code)
	}
	rec = do(h, http.MethodDelete, "/v1/storage-pools/pool-a/reservations/r2", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("repeat delete = %d", rec.Code)
	}
	p = decodeBody(t, do(h, http.MethodGet, "/v1/storage-pools/pool-a", ""))
	if p["allocatedBytes"].(float64) != 600 || p["availableBytes"].(float64) != 900 {
		t.Fatalf("pool after release = %v", p)
	}

	// Reusing the requestId after delete counts as a fresh reservation.
	rec = reserve(t, h, "pool-a", `{"requestId":"r2","bytes":800}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("reuse after delete = %d %s", rec.Code, rec.Body.String())
	}
}

func TestDeletePoolRules(t *testing.T) {
	h := Handler()
	createPool(t, h, poolA)

	// Pool with a reservation cannot be deleted.
	rec := reserve(t, h, "pool-a", `{"requestId":"r1","bytes":10}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("reserve = %d", rec.Code)
	}
	wantError(t, do(h, http.MethodDelete, "/v1/storage-pools/pool-a", ""), http.StatusConflict, "pool_not_empty")

	// Remove reservation, then deletion succeeds and frees the devices.
	if rec := do(h, http.MethodDelete, "/v1/storage-pools/pool-a/reservations/r1", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete reservation = %d", rec.Code)
	}
	if rec := do(h, http.MethodDelete, "/v1/storage-pools/pool-a", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete pool = %d %s", rec.Code, rec.Body.String())
	}
	wantError(t, do(h, http.MethodGet, "/v1/storage-pools/pool-a", ""), http.StatusNotFound, "pool_not_found")

	// Devices can now be reused.
	createPool(t, h, `{"id":"pool-b","devices":[{"id":"dev1","capacityBytes":1000,"faultDomain":"rack1"}]}`)
}

func TestMethodNotAllowed(t *testing.T) {
	h := Handler()
	createPool(t, h, poolA)

	rec := do(h, http.MethodPut, "/v1/storage-pools", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT collection = %d", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != "GET, POST" {
		t.Fatalf("Allow = %q, want GET, POST", got)
	}
	wantError(t, rec, http.StatusMethodNotAllowed, "method_not_allowed")

	rec = do(h, http.MethodPost, "/v1/storage-pools/pool-a", "")
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET, DELETE" {
		t.Fatalf("POST member = %d allow=%q", rec.Code, rec.Header().Get("Allow"))
	}

	rec = do(h, http.MethodGet, "/v1/storage-pools/pool-a/reservations", "")
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "POST" {
		t.Fatalf("GET reservations = %d allow=%q", rec.Code, rec.Header().Get("Allow"))
	}

	rec = do(h, http.MethodPost, "/v1/storage-pools/pool-a/reservations/r1", "")
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "DELETE" {
		t.Fatalf("POST reservation = %d allow=%q", rec.Code, rec.Header().Get("Allow"))
	}

	rec = do(h, http.MethodPost, "/healthz", "")
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET" {
		t.Fatalf("POST healthz = %d allow=%q", rec.Code, rec.Header().Get("Allow"))
	}
}

func TestNotFound(t *testing.T) {
	h := Handler()
	for _, target := range []string{"/", "/v1", "/v1/", "/v1/storage-pools/", "/v1/other", "/v1/storage-pools/p/extra", "/v1/storage-pools/p/reservations/r/extra"} {
		wantError(t, do(h, http.MethodGet, target, ""), http.StatusNotFound, "not_found")
	}
}

func TestConcurrentReservationsNeverOvercommit(t *testing.T) {
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
			body := fmt.Sprintf(`{"requestId":"r%d","bytes":10}`, i)
			rec := reserve(t, h, "cp", body)
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

func TestConcurrentIdempotentRetries(t *testing.T) {
	h := Handler()
	createPool(t, h, `{"id":"cp","devices":[{"id":"d","capacityBytes":100,"faultDomain":"fd"}]}`)

	const goroutines = 30
	var wg sync.WaitGroup
	statuses := make(chan int, goroutines)
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			rec := reserve(t, h, "cp", `{"requestId":"same","bytes":100}`)
			statuses <- rec.Code
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
}

func TestIDValidationBoundaries(t *testing.T) {
	h := Handler()
	for i, id := range []string{"a", "A1._-", repeat("x", 64)} {
		body := fmt.Sprintf(`{"id":%q,"devices":[{"id":%q,"capacityBytes":1,"faultDomain":"fd"}]}`, id, fmt.Sprintf("dev-%d", i))
		if rec := do(h, http.MethodPost, "/v1/storage-pools", body); rec.Code != http.StatusCreated {
			t.Fatalf("id %q rejected: %d %s", id, rec.Code, rec.Body.String())
		}
	}
	for _, id := range []string{"", repeat("x", 65), "a/b", "a b", "a@b", "漢字"} {
		body := fmt.Sprintf(`{"id":%q,"devices":[{"id":"d","capacityBytes":1,"faultDomain":"fd"}]}`, id)
		wantError(t, do(h, http.MethodPost, "/v1/storage-pools", body), http.StatusBadRequest, "invalid_request")
	}
}

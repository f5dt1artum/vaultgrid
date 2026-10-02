package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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

func do(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *strings.Reader
	if body == "" {
		rdr = strings.NewReader("")
	} else {
		rdr = strings.NewReader(body)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, rdr))
	return rec
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var payload struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("error body is not JSON: %v", err)
	}
	return payload.Error.Code
}

func mustCreatePool(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := do(t, h, http.MethodPost, "/v1/storage-pools", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create pool: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	return rec
}

func TestCreatePoolAndViews(t *testing.T) {
	h := Handler()
	rec := mustCreatePool(t, h, `{"id":"pool.b","devices":[
		{"id":"d1","capacityBytes":100,"faultDomain":"rack-1"},
		{"id":"d2","capacityBytes":50,"faultDomain":"rack-2"}]}`)
	var view struct {
		ID               string `json:"id"`
		Devices          []struct {
			ID            string `json:"id"`
			CapacityBytes int64  `json:"capacityBytes"`
			FaultDomain   string `json:"faultDomain"`
		} `json:"devices"`
		RawCapacityBytes int64 `json:"rawCapacityBytes"`
		AllocatedBytes   int64 `json:"allocatedBytes"`
		AvailableBytes   int64 `json:"availableBytes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("pool view is not JSON: %v", err)
	}
	if view.ID != "pool.b" || len(view.Devices) != 2 ||
		view.RawCapacityBytes != 150 || view.AllocatedBytes != 0 || view.AvailableBytes != 150 {
		t.Fatalf("unexpected pool view: %+v", view)
	}

	// A second pool sorts before the first in the collection view.
	mustCreatePool(t, h, `{"id":"pool.a","devices":[{"id":"d3","capacityBytes":10,"faultDomain":"fd"}]}`)
	rec = do(t, h, http.MethodGet, "/v1/storage-pools", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list: status = %d", rec.Code)
	}
	var list struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("list body is not JSON: %v", err)
	}
	if len(list.Items) != 2 || list.Items[0].ID != "pool.a" || list.Items[1].ID != "pool.b" {
		t.Fatalf("list not sorted by id: %+v", list.Items)
	}

	rec = do(t, h, http.MethodGet, "/v1/storage-pools/pool.b", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get: status = %d", rec.Code)
	}
}

func TestEmptyCollectionIsItemsArray(t *testing.T) {
	h := Handler()
	rec := do(t, h, http.MethodGet, "/v1/storage-pools", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `{"items":[]}` {
		t.Fatalf("empty collection = %s", got)
	}
}

func TestCreatePoolValidation(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"unknown field", `{"id":"p1","devices":[{"id":"d1","capacityBytes":1,"faultDomain":"f"}],"extra":1}`},
		{"empty devices", `{"id":"p1","devices":[]}`},
		{"missing devices", `{"id":"p1"}`},
		{"bad pool id", `{"id":"bad id","devices":[{"id":"d1","capacityBytes":1,"faultDomain":"f"}]}`},
		{"empty pool id", `{"id":"","devices":[{"id":"d1","capacityBytes":1,"faultDomain":"f"}]}`},
		{"too long id", fmt.Sprintf(`{"id":%q,"devices":[{"id":"d1","capacityBytes":1,"faultDomain":"f"}]}`, strings.Repeat("a", 65))},
		{"bad device id", `{"id":"p1","devices":[{"id":"d/1","capacityBytes":1,"faultDomain":"f"}]}`},
		{"zero capacity", `{"id":"p1","devices":[{"id":"d1","capacityBytes":0,"faultDomain":"f"}]}`},
		{"negative capacity", `{"id":"p1","devices":[{"id":"d1","capacityBytes":-5,"faultDomain":"f"}]}`},
		{"fractional capacity", `{"id":"p1","devices":[{"id":"d1","capacityBytes":1.5,"faultDomain":"f"}]}`},
		{"string capacity", `{"id":"p1","devices":[{"id":"d1","capacityBytes":"10","faultDomain":"f"}]}`},
		{"empty fault domain", `{"id":"p1","devices":[{"id":"d1","capacityBytes":1,"faultDomain":""}]}`},
		{"malformed json", `{"id":"p1",`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := Handler()
			rec := do(t, h, http.MethodPost, "/v1/storage-pools", tc.body)
			if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "invalid_request" {
				t.Fatalf("status = %d, code = %s", rec.Code, errorCode(t, rec))
			}
			// No partial state: the pool must not exist afterwards.
			rec = do(t, h, http.MethodGet, "/v1/storage-pools/p1", "")
			if rec.Code != http.StatusNotFound {
				t.Fatalf("partial state leaked: status = %d", rec.Code)
			}
		})
	}
}

func TestCreatePoolConflicts(t *testing.T) {
	h := Handler()
	mustCreatePool(t, h, `{"id":"p1","devices":[{"id":"d1","capacityBytes":10,"faultDomain":"f"}]}`)

	rec := do(t, h, http.MethodPost, "/v1/storage-pools", `{"id":"p1","devices":[{"id":"d2","capacityBytes":1,"faultDomain":"f"}]}`)
	if rec.Code != http.StatusConflict || errorCode(t, rec) != "pool_exists" {
		t.Fatalf("duplicate pool: status = %d, code = %s", rec.Code, errorCode(t, rec))
	}

	rec = do(t, h, http.MethodPost, "/v1/storage-pools", `{"id":"p2","devices":[{"id":"d1","capacityBytes":1,"faultDomain":"f"}]}`)
	if rec.Code != http.StatusConflict || errorCode(t, rec) != "device_in_use" {
		t.Fatalf("cross-pool device: status = %d, code = %s", rec.Code, errorCode(t, rec))
	}

	rec = do(t, h, http.MethodPost, "/v1/storage-pools", `{"id":"p3","devices":[
		{"id":"d3","capacityBytes":1,"faultDomain":"f"},
		{"id":"d3","capacityBytes":1,"faultDomain":"f"}]}`)
	if rec.Code != http.StatusConflict || errorCode(t, rec) != "device_in_use" {
		t.Fatalf("duplicate device in request: status = %d, code = %s", rec.Code, errorCode(t, rec))
	}
	// No partial state: d3 must be usable afterwards.
	mustCreatePool(t, h, `{"id":"p3","devices":[{"id":"d3","capacityBytes":1,"faultDomain":"f"}]}`)
}

func TestReservationLifecycle(t *testing.T) {
	h := Handler()
	mustCreatePool(t, h, `{"id":"p1","devices":[{"id":"d1","capacityBytes":100,"faultDomain":"f"}]}`)

	rec := do(t, h, http.MethodPost, "/v1/storage-pools/p1/reservations", `{"requestId":"r1","bytes":40}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("reserve: status = %d, body = %s", rec.Code, rec.Body.String())
	}

	// Idempotent retry: same requestId and bytes -> 200, no double counting.
	rec = do(t, h, http.MethodPost, "/v1/storage-pools/p1/reservations", `{"requestId":"r1","bytes":40}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("retry: status = %d", rec.Code)
	}
	rec = do(t, h, http.MethodGet, "/v1/storage-pools/p1", "")
	var view struct {
		AllocatedBytes int64 `json:"allocatedBytes"`
		AvailableBytes int64 `json:"availableBytes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.AllocatedBytes != 40 || view.AvailableBytes != 60 {
		t.Fatalf("double counted: %+v", view)
	}

	// Same requestId, different bytes -> 409 idempotency_conflict.
	rec = do(t, h, http.MethodPost, "/v1/storage-pools/p1/reservations", `{"requestId":"r1","bytes":41}`)
	if rec.Code != http.StatusConflict || errorCode(t, rec) != "idempotency_conflict" {
		t.Fatalf("conflict: status = %d, code = %s", rec.Code, errorCode(t, rec))
	}

	// Insufficient capacity.
	rec = do(t, h, http.MethodPost, "/v1/storage-pools/p1/reservations", `{"requestId":"r2","bytes":61}`)
	if rec.Code != http.StatusConflict || errorCode(t, rec) != "insufficient_capacity" {
		t.Fatalf("capacity: status = %d, code = %s", rec.Code, errorCode(t, rec))
	}

	// Delete releases capacity; repeat delete stays 204.
	rec = do(t, h, http.MethodDelete, "/v1/storage-pools/p1/reservations/r1", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete reservation: status = %d", rec.Code)
	}
	rec = do(t, h, http.MethodDelete, "/v1/storage-pools/p1/reservations/r1", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("repeat delete: status = %d", rec.Code)
	}
	rec = do(t, h, http.MethodGet, "/v1/storage-pools/p1", "")
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.AllocatedBytes != 0 || view.AvailableBytes != 100 {
		t.Fatalf("capacity not released: %+v", view)
	}
}

func TestReservationValidation(t *testing.T) {
	h := Handler()
	mustCreatePool(t, h, `{"id":"p1","devices":[{"id":"d1","capacityBytes":10,"faultDomain":"f"}]}`)
	for _, body := range []string{
		`{"requestId":"bad id","bytes":1}`,
		`{"requestId":"r1","bytes":0}`,
		`{"requestId":"r1","bytes":-1}`,
		`{"requestId":"r1","bytes":1.5}`,
		`{"requestId":"r1","bytes":"1"}`,
		`{"requestId":"r1","bytes":1,"extra":true}`,
		`{"bytes":1}`,
	} {
		rec := do(t, h, http.MethodPost, "/v1/storage-pools/p1/reservations", body)
		if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "invalid_request" {
			t.Fatalf("body %s: status = %d, code = %s", body, rec.Code, errorCode(t, rec))
		}
	}
}

func TestPoolDeletion(t *testing.T) {
	h := Handler()
	mustCreatePool(t, h, `{"id":"p1","devices":[{"id":"d1","capacityBytes":10,"faultDomain":"f"}]}`)
	rec := do(t, h, http.MethodPost, "/v1/storage-pools/p1/reservations", `{"requestId":"r1","bytes":5}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("reserve: status = %d", rec.Code)
	}

	rec = do(t, h, http.MethodDelete, "/v1/storage-pools/p1", "")
	if rec.Code != http.StatusConflict || errorCode(t, rec) != "pool_not_empty" {
		t.Fatalf("delete non-empty: status = %d, code = %s", rec.Code, errorCode(t, rec))
	}

	do(t, h, http.MethodDelete, "/v1/storage-pools/p1/reservations/r1", "")
	rec = do(t, h, http.MethodDelete, "/v1/storage-pools/p1", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete empty pool: status = %d", rec.Code)
	}
	// Devices are released once the pool is gone.
	mustCreatePool(t, h, `{"id":"p2","devices":[{"id":"d1","capacityBytes":10,"faultDomain":"f"}]}`)
}

func TestPoolNotFound(t *testing.T) {
	h := Handler()
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "/v1/storage-pools/nope", ""},
		{http.MethodDelete, "/v1/storage-pools/nope", ""},
		{http.MethodPost, "/v1/storage-pools/nope/reservations", `{"requestId":"r1","bytes":1}`},
		{http.MethodDelete, "/v1/storage-pools/nope/reservations/r1", ""},
	} {
		rec := do(t, h, tc.method, tc.path, tc.body)
		if rec.Code != http.StatusNotFound || errorCode(t, rec) != "pool_not_found" {
			t.Fatalf("%s %s: status = %d, code = %s", tc.method, tc.path, rec.Code, errorCode(t, rec))
		}
	}
}

func TestMethodNotAllowedAndUnknownPaths(t *testing.T) {
	h := Handler()
	for _, tc := range []struct {
		method, path, allow string
	}{
		{http.MethodPut, "/v1/storage-pools", "GET, POST"},
		{http.MethodPost, "/v1/storage-pools/p1", "GET, DELETE"},
		{http.MethodGet, "/v1/storage-pools/p1/reservations", "POST"},
		{http.MethodGet, "/v1/storage-pools/p1/reservations/r1", "DELETE"},
	} {
		rec := do(t, h, tc.method, tc.path, "")
		if rec.Code != http.StatusMethodNotAllowed || errorCode(t, rec) != "method_not_allowed" {
			t.Fatalf("%s %s: status = %d, code = %s", tc.method, tc.path, rec.Code, errorCode(t, rec))
		}
		if rec.Header().Get("Allow") != tc.allow {
			t.Fatalf("%s %s: Allow = %q, want %q", tc.method, tc.path, rec.Header().Get("Allow"), tc.allow)
		}
	}
	for _, path := range []string{"/v1", "/v1/unknown", "/v1/storage-pools/p1/extra", "/v1/storage-pools/"} {
		rec := do(t, h, http.MethodGet, path, "")
		if rec.Code != http.StatusNotFound || errorCode(t, rec) != "not_found" {
			t.Fatalf("GET %s: status = %d, code = %s", path, rec.Code, errorCode(t, rec))
		}
	}
}

func TestConcurrentReservationsNeverExceedCapacity(t *testing.T) {
	h := Handler()
	mustCreatePool(t, h, `{"id":"p1","devices":[{"id":"d1","capacityBytes":100,"faultDomain":"f"}]}`)

	var wg sync.WaitGroup
	var mu sync.Mutex
	var succeeded int64
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := fmt.Sprintf(`{"requestId":"r%d","bytes":10}`, i)
			rec := do(t, h, http.MethodPost, "/v1/storage-pools/p1/reservations", body)
			if rec.Code == http.StatusCreated {
				mu.Lock()
				succeeded += 10
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if succeeded > 100 {
		t.Fatalf("allocated %d exceeds raw capacity 100", succeeded)
	}
	rec := do(t, h, http.MethodGet, "/v1/storage-pools/p1", "")
	var view struct {
		AllocatedBytes int64 `json:"allocatedBytes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.AllocatedBytes != succeeded {
		t.Fatalf("allocated = %d, want %d", view.AllocatedBytes, succeeded)
	}
}

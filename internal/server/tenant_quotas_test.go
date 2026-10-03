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

// doTenant issues a request carrying one X-Vaultgrid-Tenant header value.
func doTenant(h http.Handler, method, target, body, tenant string) *httptest.ResponseRecorder {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, bytes.NewBufferString(body))
	}
	if tenant != "" {
		r.Header.Set(tenantHeader, tenant)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func putQuota(t *testing.T, h http.Handler, pool, tenant string, limit int64) *httptest.ResponseRecorder {
	t.Helper()
	return do(h, http.MethodPut, "/v1/storage-pools/"+pool+"/tenant-quotas/"+tenant,
		fmt.Sprintf(`{"limitBytes":%d}`, limit))
}

func mustQuota(t *testing.T, h http.Handler, pool, tenant string, limit int64) {
	t.Helper()
	if rec := putQuota(t, h, pool, tenant, limit); rec.Code != http.StatusCreated {
		t.Fatalf("put quota status = %d body = %s", rec.Code, rec.Body.String())
	}
}

func quotaViewOf(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	return decodeBody(t, rec)
}

func TestTenantQuotaCreateUpdateReplay(t *testing.T) {
	h := Handler()
	createPool(t, h, poolA)

	rec := putQuota(t, h, "pool-a", "team-a", 500)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d body = %s", rec.Code, rec.Body.String())
	}
	v := quotaViewOf(t, rec)
	if v["poolId"] != "pool-a" || v["tenantId"] != "team-a" {
		t.Fatalf("identity fields wrong: %v", v)
	}
	if v["limitBytes"].(float64) != 500 || v["usedBytes"].(float64) != 0 || v["availableBytes"].(float64) != 500 {
		t.Fatalf("byte fields wrong: %v", v)
	}

	// Same-value replay is a 200 no-op.
	rec = putQuota(t, h, "pool-a", "team-a", 500)
	if rec.Code != http.StatusOK {
		t.Fatalf("replay status = %d", rec.Code)
	}
	// A real change is also 200.
	rec = putQuota(t, h, "pool-a", "team-a", 700)
	if rec.Code != http.StatusOK {
		t.Fatalf("update status = %d", rec.Code)
	}
	if v := quotaViewOf(t, rec); v["limitBytes"].(float64) != 700 || v["availableBytes"].(float64) != 700 {
		t.Fatalf("updated view wrong: %v", v)
	}
}

func TestTenantQuotaGetListDelete(t *testing.T) {
	h := Handler()
	createPool(t, h, poolA)
	mustQuota(t, h, "pool-a", "team-b", 100)
	mustQuota(t, h, "pool-a", "team-a", 200)

	rec := do(h, http.MethodGet, "/v1/storage-pools/pool-a/tenant-quotas/team-a", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d", rec.Code)
	}
	if v := quotaViewOf(t, rec); v["tenantId"] != "team-a" || v["limitBytes"].(float64) != 200 {
		t.Fatalf("get view wrong: %v", v)
	}

	rec = do(h, http.MethodGet, "/v1/storage-pools/pool-a/tenant-quotas", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d", rec.Code)
	}
	items := decodeBody(t, rec)["items"].([]any)
	if len(items) != 2 ||
		items[0].(map[string]any)["tenantId"] != "team-a" ||
		items[1].(map[string]any)["tenantId"] != "team-b" {
		t.Fatalf("list not sorted by tenantId: %v", items)
	}

	// Delete is idempotent and always 204.
	for i := 0; i < 2; i++ {
		rec = do(h, http.MethodDelete, "/v1/storage-pools/pool-a/tenant-quotas/team-a", "")
		if rec.Code != http.StatusNoContent {
			t.Fatalf("delete %d status = %d", i, rec.Code)
		}
	}
	wantError(t, do(h, http.MethodGet, "/v1/storage-pools/pool-a/tenant-quotas/team-a", ""),
		http.StatusNotFound, "quota_not_found")
}

func TestTenantQuotaNotFoundAndInvalid(t *testing.T) {
	h := Handler()
	createPool(t, h, poolA)

	wantError(t, do(h, http.MethodGet, "/v1/storage-pools/nope/tenant-quotas", ""),
		http.StatusNotFound, "pool_not_found")
	wantError(t, do(h, http.MethodGet, "/v1/storage-pools/nope/tenant-quotas/team-a", ""),
		http.StatusNotFound, "pool_not_found")
	wantError(t, putQuota(t, h, "nope", "team-a", 100),
		http.StatusNotFound, "pool_not_found")
	wantError(t, do(h, http.MethodDelete, "/v1/storage-pools/nope/tenant-quotas/team-a", ""),
		http.StatusNotFound, "pool_not_found")
	wantError(t, do(h, http.MethodGet, "/v1/storage-pools/pool-a/tenant-quotas/team-a", ""),
		http.StatusNotFound, "quota_not_found")

	// Invalid ids and bodies.
	wantError(t, putQuota(t, h, "pool-a", "bad~tenant", 100), http.StatusBadRequest, "invalid_request")
	wantError(t, putQuota(t, h, "pool-a", "team-a", 0), http.StatusBadRequest, "invalid_request")
	wantError(t, putQuota(t, h, "pool-a", "team-a", -5), http.StatusBadRequest, "invalid_request")
	wantError(t, do(h, http.MethodPut, "/v1/storage-pools/pool-a/tenant-quotas/team-a",
		`{"limitBytes":100,"extra":1}`), http.StatusBadRequest, "invalid_request")
	wantError(t, do(h, http.MethodPut, "/v1/storage-pools/pool-a/tenant-quotas/team-a",
		`{"limitBytes":"100"}`), http.StatusBadRequest, "invalid_request")
	wantError(t, do(h, http.MethodGet, "/v1/storage-pools/pool-a/tenant-quotas/bad~tenant", ""),
		http.StatusBadRequest, "invalid_request")

	// Wrong methods.
	rec := do(h, http.MethodPost, "/v1/storage-pools/pool-a/tenant-quotas", `{}`)
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodGet {
		t.Fatalf("collection POST: code = %d allow = %q", rec.Code, rec.Header().Get("Allow"))
	}
	rec = do(h, http.MethodPost, "/v1/storage-pools/pool-a/tenant-quotas/team-a", `{}`)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("item POST: code = %d", rec.Code)
	}
}

func TestTenantQuotaBelowUsage(t *testing.T) {
	h := Handler()
	createPool(t, h, poolA)
	createVol(t, h, `{"id":"vol-1","poolId":"pool-a","sizeBytes":600}`)

	// The default tenant already uses 600 bytes; a lower limit is rejected.
	wantError(t, putQuota(t, h, "pool-a", "default", 500), http.StatusConflict, "quota_below_usage")
	// An equal limit is fine.
	if rec := putQuota(t, h, "pool-a", "default", 600); rec.Code != http.StatusCreated {
		t.Fatalf("quota at usage status = %d", rec.Code)
	}
	// Lowering an existing quota below usage is also rejected.
	wantError(t, putQuota(t, h, "pool-a", "default", 599), http.StatusConflict, "quota_below_usage")
}

func TestTenantHeaderValidation(t *testing.T) {
	h := Handler()
	createPool(t, h, poolA)

	// Illegal tenant value.
	wantError(t, doTenant(h, http.MethodPost, "/v1/volumes",
		`{"id":"vol-1","poolId":"pool-a","sizeBytes":10}`, "bad tenant"),
		http.StatusBadRequest, "invalid_request")
	// Duplicated tenant header.
	r := httptest.NewRequest(http.MethodPost, "/v1/volumes",
		bytes.NewBufferString(`{"id":"vol-1","poolId":"pool-a","sizeBytes":10}`))
	r.Header["X-Vaultgrid-Tenant"] = []string{"team-a", "team-b"}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	wantError(t, rec, http.StatusBadRequest, "invalid_request")
	// Reservations and buckets validate the header too.
	wantError(t, doTenant(h, http.MethodPost, "/v1/storage-pools/pool-a/reservations",
		`{"requestId":"r1","bytes":10}`, "bad tenant"), http.StatusBadRequest, "invalid_request")
	wantError(t, doTenant(h, http.MethodPost, "/v1/buckets",
		`{"id":"b1","poolId":"pool-a"}`, "bad tenant"), http.StatusBadRequest, "invalid_request")
}

func TestTenantQuotaEnforcementAndRelease(t *testing.T) {
	h := Handler()
	createPool(t, h, poolA)
	mustQuota(t, h, "pool-a", "team-a", 100)

	// Unconfigured tenants are unlimited.
	createVol(t, h, `{"id":"vol-free","poolId":"pool-a","sizeBytes":1000}`)

	// First volume fits exactly.
	rec := doTenant(h, http.MethodPost, "/v1/volumes",
		`{"id":"vol-1","poolId":"pool-a","sizeBytes":60}`, "team-a")
	if rec.Code != http.StatusCreated {
		t.Fatalf("vol-1 status = %d body = %s", rec.Code, rec.Body.String())
	}
	// The next one would exceed the tenant quota even though the pool has room.
	wantError(t, doTenant(h, http.MethodPost, "/v1/volumes",
		`{"id":"vol-2","poolId":"pool-a","sizeBytes":50}`, "team-a"),
		http.StatusConflict, "tenant_quota_exceeded")
	// A failed create leaves no usage behind: 40 more still fits.
	rec = doTenant(h, http.MethodPost, "/v1/volumes",
		`{"id":"vol-2","poolId":"pool-a","sizeBytes":40}`, "team-a")
	if rec.Code != http.StatusCreated {
		t.Fatalf("vol-2 status = %d body = %s", rec.Code, rec.Body.String())
	}

	// usedBytes reflects both volumes.
	rec = do(h, http.MethodGet, "/v1/storage-pools/pool-a/tenant-quotas/team-a", "")
	v := quotaViewOf(t, rec)
	if v["usedBytes"].(float64) != 100 || v["availableBytes"].(float64) != 0 {
		t.Fatalf("usedBytes wrong: %v", v)
	}

	// Deleting a volume releases the tenant's usage.
	rec = do(h, http.MethodDelete, "/v1/volumes/vol-2", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete vol-2 status = %d", rec.Code)
	}
	rec = do(h, http.MethodGet, "/v1/storage-pools/pool-a/tenant-quotas/team-a", "")
	if v := quotaViewOf(t, rec); v["usedBytes"].(float64) != 60 {
		t.Fatalf("usedBytes after delete: %v", v)
	}
}

func TestTenantIdempotency(t *testing.T) {
	h := Handler()
	createPool(t, h, poolA)

	// Volume: same id + same tenant replays, different tenant conflicts.
	rec := doTenant(h, http.MethodPost, "/v1/volumes",
		`{"id":"vol-1","poolId":"pool-a","sizeBytes":10}`, "team-a")
	if rec.Code != http.StatusCreated {
		t.Fatalf("vol-1 status = %d", rec.Code)
	}
	rec = doTenant(h, http.MethodPost, "/v1/volumes",
		`{"id":"vol-1","poolId":"pool-a","sizeBytes":10}`, "team-a")
	if rec.Code != http.StatusOK {
		t.Fatalf("vol-1 replay status = %d", rec.Code)
	}
	wantError(t, doTenant(h, http.MethodPost, "/v1/volumes",
		`{"id":"vol-1","poolId":"pool-a","sizeBytes":10}`, "team-b"),
		http.StatusConflict, "volume_exists")
	// No header means the default tenant, which differs from team-a.
	wantError(t, do(h, http.MethodPost, "/v1/volumes",
		`{"id":"vol-1","poolId":"pool-a","sizeBytes":10}`),
		http.StatusConflict, "volume_exists")

	// Reservation: same requestId + same tenant replays, different tenant conflicts.
	rec = doTenant(h, http.MethodPost, "/v1/storage-pools/pool-a/reservations",
		`{"requestId":"r1","bytes":10}`, "team-a")
	if rec.Code != http.StatusCreated {
		t.Fatalf("reservation status = %d", rec.Code)
	}
	rec = doTenant(h, http.MethodPost, "/v1/storage-pools/pool-a/reservations",
		`{"requestId":"r1","bytes":10}`, "team-a")
	if rec.Code != http.StatusOK {
		t.Fatalf("reservation replay status = %d", rec.Code)
	}
	wantError(t, doTenant(h, http.MethodPost, "/v1/storage-pools/pool-a/reservations",
		`{"requestId":"r1","bytes":10}`, "team-b"),
		http.StatusConflict, "idempotency_conflict")

	// Bucket: same id + same tenant replays, different tenant conflicts.
	rec = doTenant(h, http.MethodPost, "/v1/buckets", `{"id":"b1","poolId":"pool-a"}`, "team-a")
	if rec.Code != http.StatusCreated {
		t.Fatalf("bucket status = %d", rec.Code)
	}
	rec = doTenant(h, http.MethodPost, "/v1/buckets", `{"id":"b1","poolId":"pool-a"}`, "team-a")
	if rec.Code != http.StatusOK {
		t.Fatalf("bucket replay status = %d", rec.Code)
	}
	wantError(t, doTenant(h, http.MethodPost, "/v1/buckets", `{"id":"b1","poolId":"pool-a"}`, "team-b"),
		http.StatusConflict, "bucket_exists")
}

func TestTenantUsageAcrossResourceKinds(t *testing.T) {
	h := Handler()
	createPool(t, h, poolA)

	// Reservation 100 + volume 200 for team-a.
	doTenant(h, http.MethodPost, "/v1/storage-pools/pool-a/reservations",
		`{"requestId":"r1","bytes":100}`, "team-a")
	doTenant(h, http.MethodPost, "/v1/volumes",
		`{"id":"vol-1","poolId":"pool-a","sizeBytes":200}`, "team-a")
	// Snapshot of the volume inherits the volume's tenant (+200).
	rec := do(h, http.MethodPost, "/v1/volumes/vol-1/snapshots",
		`{"id":"snap-1","expectedGeneration":0}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("snapshot status = %d body = %s", rec.Code, rec.Body.String())
	}
	// Bucket for team-a with a 50-byte object; objects inherit the bucket's tenant.
	doTenant(h, http.MethodPost, "/v1/buckets", `{"id":"b1","poolId":"pool-a"}`, "team-a")
	mustPutObject(t, h, "b1", "k1", "12345", nil)
	mustPutObject(t, h, "b1", "k2", "123456789012345678901234567890123456789012345", nil)

	mustQuota(t, h, "pool-a", "team-a", 550)
	rec = do(h, http.MethodGet, "/v1/storage-pools/pool-a/tenant-quotas/team-a", "")
	v := quotaViewOf(t, rec)
	if v["usedBytes"].(float64) != 550 || v["availableBytes"].(float64) != 0 {
		t.Fatalf("aggregate usedBytes wrong: %v", v)
	}

	// Shrinking the object releases usage; growing it again is capped by quota.
	mustPutObject(t, h, "b1", "k1", "1", nil)
	rec = do(h, http.MethodGet, "/v1/storage-pools/pool-a/tenant-quotas/team-a", "")
	if v := quotaViewOf(t, rec); v["usedBytes"].(float64) != 546 {
		t.Fatalf("usedBytes after shrink: %v", v)
	}
	rec = putObject(t, h, "b1", "k1", "123456", nil)
	wantError(t, rec, http.StatusConflict, "tenant_quota_exceeded")

	// Deleting the snapshot releases its 200 bytes.
	rec = do(h, http.MethodDelete, "/v1/snapshots/snap-1", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete snapshot status = %d", rec.Code)
	}
	rec = do(h, http.MethodGet, "/v1/storage-pools/pool-a/tenant-quotas/team-a", "")
	if v := quotaViewOf(t, rec); v["usedBytes"].(float64) != 346 {
		t.Fatalf("usedBytes after snapshot delete: %v", v)
	}
}

func TestCloneTenantCountedInTargetPool(t *testing.T) {
	h := Handler()
	createPool(t, h, poolA)
	createPool(t, h, `{"id":"pool-b","devices":[{"id":"dev9","capacityBytes":500,"faultDomain":"rack1"}]}`)

	doTenant(h, http.MethodPost, "/v1/volumes",
		`{"id":"vol-1","poolId":"pool-a","sizeBytes":200}`, "team-a")
	do(h, http.MethodPost, "/v1/volumes/vol-1/snapshots", `{"id":"snap-1","expectedGeneration":0}`)

	// The clone inherits the snapshot's tenant and charges the target pool.
	mustQuota(t, h, "pool-b", "team-a", 200)
	rec := do(h, http.MethodPost, "/v1/snapshots/snap-1/clones",
		`{"id":"vol-2","poolId":"pool-b"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("clone status = %d body = %s", rec.Code, rec.Body.String())
	}
	rec = do(h, http.MethodGet, "/v1/storage-pools/pool-b/tenant-quotas/team-a", "")
	if v := quotaViewOf(t, rec); v["usedBytes"].(float64) != 200 || v["availableBytes"].(float64) != 0 {
		t.Fatalf("target pool tenant usage wrong: %v", v)
	}
	// A second clone for the same tenant would exceed the target pool's quota.
	wantError(t, do(h, http.MethodPost, "/v1/snapshots/snap-1/clones",
		`{"id":"vol-3","poolId":"pool-b"}`), http.StatusConflict, "tenant_quota_exceeded")
}

func TestSnapshotQuotaExceeded(t *testing.T) {
	h := Handler()
	createPool(t, h, poolA)
	doTenant(h, http.MethodPost, "/v1/volumes",
		`{"id":"vol-1","poolId":"pool-a","sizeBytes":600}`, "team-a")
	mustQuota(t, h, "pool-a", "team-a", 700)

	// The snapshot would push team-a to 1200 bytes, past its 700 quota.
	wantError(t, do(h, http.MethodPost, "/v1/volumes/vol-1/snapshots",
		`{"id":"snap-1","expectedGeneration":0}`), http.StatusConflict, "tenant_quota_exceeded")
}

func TestTenantQuotaDoesNotBlockPoolDelete(t *testing.T) {
	h := Handler()
	createPool(t, h, poolA)
	mustQuota(t, h, "pool-a", "team-a", 100)

	rec := do(h, http.MethodDelete, "/v1/storage-pools/pool-a", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete pool status = %d body = %s", rec.Code, rec.Body.String())
	}
}

func TestTenantQuotaAuditEvents(t *testing.T) {
	h := Handler()
	createPool(t, h, poolA)

	mustQuota(t, h, "pool-a", "team-a", 100)                                      // created
	putQuota(t, h, "pool-a", "team-a", 100)                                       // replay: no event
	putQuota(t, h, "pool-a", "team-a", 200)                                       // updated
	do(h, http.MethodDelete, "/v1/storage-pools/pool-a/tenant-quotas/team-a", "") // deleted
	do(h, http.MethodDelete, "/v1/storage-pools/pool-a/tenant-quotas/team-a", "") // replay: no event

	v := getAuditEvents(t, h, "/v1/audit-events")
	items := v["items"].([]any)
	var actions []string
	for _, raw := range items {
		e := raw.(map[string]any)
		if e["action"] == "tenant-quota.created" || e["action"] == "tenant-quota.updated" || e["action"] == "tenant-quota.deleted" {
			if e["resource"] != "/v1/storage-pools/pool-a/tenant-quotas/team-a" {
				t.Fatalf("quota event resource wrong: %v", e)
			}
			if e["poolId"] != "pool-a" || e["bytesDelta"].(float64) != 0 {
				t.Fatalf("quota event fields wrong: %v", e)
			}
			actions = append(actions, e["action"].(string))
		}
	}
	want := []string{"tenant-quota.created", "tenant-quota.updated", "tenant-quota.deleted"}
	if len(actions) != len(want) {
		t.Fatalf("quota actions = %v, want %v", actions, want)
	}
	for i := range want {
		if actions[i] != want[i] {
			t.Fatalf("quota actions = %v, want %v", actions, want)
		}
	}
}

func TestTenantQuotaConcurrentCreates(t *testing.T) {
	h := Handler()
	createPool(t, h, poolA)
	mustQuota(t, h, "pool-a", "team-a", 100)

	// 16 concurrent creates of 10 bytes each for one tenant: exactly 10 win.
	var wg sync.WaitGroup
	var mu sync.Mutex
	succeeded := 0
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := fmt.Sprintf(`{"id":"vol-%d","poolId":"pool-a","sizeBytes":10}`, i)
			rec := doTenant(h, http.MethodPost, "/v1/volumes", body, "team-a")
			if rec.Code == http.StatusCreated {
				mu.Lock()
				succeeded++
				mu.Unlock()
			} else {
				wantError(t, rec, http.StatusConflict, "tenant_quota_exceeded")
			}
		}(i)
	}
	wg.Wait()
	if succeeded != 10 {
		t.Fatalf("succeeded = %d, want 10", succeeded)
	}
	rec := do(h, http.MethodGet, "/v1/storage-pools/pool-a/tenant-quotas/team-a", "")
	if v := quotaViewOf(t, rec); v["usedBytes"].(float64) != 100 {
		t.Fatalf("usedBytes = %v, want 100", v["usedBytes"])
	}
}

func TestTenantQuotaJSONFieldTypes(t *testing.T) {
	h := Handler()
	createPool(t, h, poolA)
	mustQuota(t, h, "pool-a", "team-a", 100)
	rec := do(h, http.MethodGet, "/v1/storage-pools/pool-a/tenant-quotas/team-a", "")
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("view is not JSON: %v", err)
	}
	for _, field := range []string{"limitBytes", "usedBytes", "availableBytes"} {
		var n int64
		if err := json.Unmarshal(raw[field], &n); err != nil {
			t.Fatalf("%s is not a JSON integer: %s", field, raw[field])
		}
	}
}

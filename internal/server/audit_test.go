package server

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// getAuditPage decodes a GET /v1/audit-events response, requiring 200.
func getAuditPage(t *testing.T, h http.Handler, target string) map[string]any {
	t.Helper()
	rec := do(h, http.MethodGet, target, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d %s", target, rec.Code, rec.Body.String())
	}
	return decodeBody(t, rec)
}

func auditItems(t *testing.T, page map[string]any) []map[string]any {
	t.Helper()
	raw := page["items"].([]any)
	items := make([]map[string]any, len(raw))
	for i, r := range raw {
		items[i] = r.(map[string]any)
	}
	return items
}

func eventEq(t *testing.T, e map[string]any, seq float64, action, resource, poolID string, delta float64) {
	t.Helper()
	want := map[string]any{
		"sequence":   seq,
		"action":     action,
		"resource":   resource,
		"poolId":     poolID,
		"bytesDelta": delta,
	}
	for k, v := range want {
		if e[k] != v {
			t.Fatalf("event[%s] = %v, want %v (event %v)", k, e[k], v, e)
		}
	}
	if len(e) != len(want) {
		t.Fatalf("event has unexpected keys: %v", e)
	}
}

// TestAuditEventsEmpty covers the empty log and its exact response shape.
func TestAuditEventsEmpty(t *testing.T) {
	h := Handler()
	page := getAuditPage(t, h, "/v1/audit-events")
	if len(auditItems(t, page)) != 0 {
		t.Fatalf("items = %v, want empty", page["items"])
	}
	if page["nextAfter"].(float64) != 0 || page["hasMore"].(bool) {
		t.Fatalf("empty page = %v", page)
	}
	// Empty items serialize as [], nextAfter zero and hasMore false.
	body := do(h, http.MethodGet, "/v1/audit-events", "").Body.String()
	if body != `{"items":[],"nextAfter":0,"hasMore":false}`+"\n" {
		t.Fatalf("empty body = %q", body)
	}
}

// TestAuditEventsLifecycle exercises every action, its resource path, owning
// pool and signed capacity delta in commit order.
func TestAuditEventsLifecycle(t *testing.T) {
	h := Handler()
	// reportFixture (capacity_report_test.go) performs, in order:
	// pool-a, pool-b creation; reservation r1=100; volume vol-1=200; snapshot
	// snap-1=200; clone vol-2=200 into pool-b; bucket-1; 50-byte object k;
	// empty bucket-2.
	reportFixture(t, h)

	// Further mutations continue the sequence.
	if rec := do(h, http.MethodPut, "/v1/buckets/bucket-1/objects/k", "abc"); rec.Code != http.StatusOK {
		t.Fatalf("overwrite smaller = %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(h, http.MethodPut, "/v1/buckets/bucket-1/objects/k", "xyz"); rec.Code != http.StatusOK {
		t.Fatalf("overwrite same size = %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(h, http.MethodPut, "/v1/volumes/vol-1/binding", `{"nodeId":"node-1","expectedGeneration":0}`); rec.Code != http.StatusOK {
		t.Fatalf("bind = %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(h, http.MethodDelete, "/v1/volumes/vol-1/binding?expectedGeneration=1", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("unbind = %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(h, http.MethodDelete, "/v1/buckets/bucket-1/objects/k", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete object = %d", rec.Code)
	}
	if rec := do(h, http.MethodDelete, "/v1/buckets/bucket-1", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete bucket-1 = %d", rec.Code)
	}
	if rec := do(h, http.MethodDelete, "/v1/buckets/bucket-2", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete bucket-2 = %d", rec.Code)
	}
	if rec := do(h, http.MethodDelete, "/v1/snapshots/snap-1", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete snapshot = %d", rec.Code)
	}
	if rec := do(h, http.MethodDelete, "/v1/volumes/vol-2", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete clone volume = %d", rec.Code)
	}
	if rec := do(h, http.MethodDelete, "/v1/volumes/vol-1", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete volume = %d", rec.Code)
	}
	if rec := do(h, http.MethodDelete, "/v1/storage-pools/pool-a/reservations/r1", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete reservation = %d", rec.Code)
	}
	if rec := do(h, http.MethodDelete, "/v1/storage-pools/pool-a", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete pool-a = %d", rec.Code)
	}
	if rec := do(h, http.MethodDelete, "/v1/storage-pools/pool-b", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete pool-b = %d", rec.Code)
	}

	items := auditItems(t, getAuditPage(t, h, "/v1/audit-events?limit=1000"))
	want := []struct {
		action, resource, pool string
		delta                  float64
	}{
		{"pool.created", "/v1/storage-pools/pool-a", "pool-a", 0},
		{"pool.created", "/v1/storage-pools/pool-b", "pool-b", 0},
		{"reservation.created", "/v1/storage-pools/pool-a/reservations/r1", "pool-a", 100},
		{"volume.created", "/v1/volumes/vol-1", "pool-a", 200},
		{"snapshot.created", "/v1/snapshots/snap-1", "pool-a", 200},
		{"clone.created", "/v1/volumes/vol-2", "pool-b", 200},
		{"bucket.created", "/v1/buckets/bucket-1", "pool-a", 0},
		{"object.created", "/v1/buckets/bucket-1/objects/k", "pool-a", 50},
		{"bucket.created", "/v1/buckets/bucket-2", "pool-b", 0},
		// 50 bytes -> 3 bytes ("abc") releases 47.
		{"object.overwritten", "/v1/buckets/bucket-1/objects/k", "pool-a", -47},
		// 3 bytes -> 3 bytes ("xyz") same-size overwrite still records delta 0.
		{"object.overwritten", "/v1/buckets/bucket-1/objects/k", "pool-a", 0},
		{"volume.bound", "/v1/volumes/vol-1/binding", "pool-a", 0},
		{"volume.unbound", "/v1/volumes/vol-1/binding", "pool-a", 0},
		{"object.deleted", "/v1/buckets/bucket-1/objects/k", "pool-a", -3},
		{"bucket.deleted", "/v1/buckets/bucket-1", "pool-a", 0},
		{"bucket.deleted", "/v1/buckets/bucket-2", "pool-b", 0},
		{"snapshot.deleted", "/v1/snapshots/snap-1", "pool-a", -200},
		{"volume.deleted", "/v1/volumes/vol-2", "pool-b", -200},
		{"volume.deleted", "/v1/volumes/vol-1", "pool-a", -200},
		{"reservation.deleted", "/v1/storage-pools/pool-a/reservations/r1", "pool-a", -100},
		{"pool.deleted", "/v1/storage-pools/pool-a", "pool-a", 0},
		{"pool.deleted", "/v1/storage-pools/pool-b", "pool-b", 0},
	}
	if len(items) != len(want) {
		t.Fatalf("got %d events, want %d: %v", len(items), len(want), items)
	}
	for i, w := range want {
		eventEq(t, items[i], float64(i+1), w.action, w.resource, w.pool, w.delta)
	}
}

// TestAuditObjectResourceWithSlash verifies the resource is the full public
// path, including keys containing slashes.
func TestAuditObjectResourceWithSlash(t *testing.T) {
	h := Handler()
	createPool(t, h, poolA)
	if rec := do(h, http.MethodPost, "/v1/buckets", `{"id":"bucket-1","poolId":"pool-a"}`); rec.Code != http.StatusCreated {
		t.Fatalf("create bucket = %d", rec.Code)
	}
	if rec := do(h, http.MethodPut, "/v1/buckets/bucket-1/objects/a/b.txt", "xy"); rec.Code != http.StatusCreated {
		t.Fatalf("put = %d %s", rec.Code, rec.Body.String())
	}
	items := auditItems(t, getAuditPage(t, h, "/v1/audit-events"))
	last := items[len(items)-1]
	eventEq(t, last, float64(len(items)), "object.created", "/v1/buckets/bucket-1/objects/a/b.txt", "pool-a", 2)
}

// TestAuditEventsSuppressed makes sure failures, read-only requests and
// successful no-ops leave no trace.
func TestAuditEventsSuppressed(t *testing.T) {
	h := Handler()
	createPool(t, h, poolA)
	do(h, http.MethodPost, "/v1/volumes", `{"id":"vol-1","poolId":"pool-a","sizeBytes":200}`)
	do(h, http.MethodPost, "/v1/buckets", `{"id":"bucket-1","poolId":"pool-a"}`)

	baseline := len(auditItems(t, getAuditPage(t, h, "/v1/audit-events?limit=1000")))

	// Read-only endpoints never record.
	do(h, http.MethodGet, "/v1/storage-pools", "")
	do(h, http.MethodGet, "/v1/storage-pools/pool-a", "")
	do(h, http.MethodGet, "/v1/volumes", "")
	do(h, http.MethodGet, "/v1/capacity-report", "")
	do(h, http.MethodGet, "/v1/buckets/bucket-1/objects", "")

	// Idempotent retries succeed without changing state.
	do(h, http.MethodPost, "/v1/storage-pools/pool-a/reservations", `{"requestId":"r1","bytes":100}`)
	do(h, http.MethodPost, "/v1/storage-pools/pool-a/reservations", `{"requestId":"r1","bytes":100}`)
	do(h, http.MethodPost, "/v1/volumes", `{"id":"vol-1","poolId":"pool-a","sizeBytes":200}`)
	do(h, http.MethodPost, "/v1/volumes/vol-1/snapshots", `{"id":"snap-1","expectedGeneration":0}`)
	do(h, http.MethodPost, "/v1/volumes/vol-1/snapshots", `{"id":"snap-1","expectedGeneration":0}`)
	do(h, http.MethodPost, "/v1/snapshots/snap-1/clones", `{"id":"vol-2","poolId":"pool-a"}`)
	do(h, http.MethodPost, "/v1/snapshots/snap-1/clones", `{"id":"vol-2","poolId":"pool-a"}`)
	do(h, http.MethodPost, "/v1/buckets", `{"id":"bucket-1","poolId":"pool-a"}`)

	// Binding same node at the new generation is a no-op; unbinding an
	// already-unbound volume succeeds without changing state.
	do(h, http.MethodPut, "/v1/volumes/vol-2/binding", `{"nodeId":"n1","expectedGeneration":0}`)
	do(h, http.MethodPut, "/v1/volumes/vol-2/binding", `{"nodeId":"n1","expectedGeneration":1}`)
	do(h, http.MethodDelete, "/v1/volumes/vol-1/binding?expectedGeneration=0", "")

	// Deleting things that never existed still returns 204 but changes nothing.
	do(h, http.MethodDelete, "/v1/storage-pools/pool-a/reservations/nope", "")
	do(h, http.MethodDelete, "/v1/buckets/bucket-1/objects/missing", "")

	// Failures of every flavour leave no event.
	do(h, http.MethodPost, "/v1/storage-pools/pool-a/reservations", `{"requestId":"r1","bytes":999}`) // idempotency_conflict
	do(h, http.MethodPost, "/v1/storage-pools/pool-a/reservations", `{"requestId":"r2","bytes":100000}`)
	// vol-1 is unbound, so this succeeds (and records an event below); an
	// existing snapshot of a deleted volume does not block the delete.
	do(h, http.MethodDelete, "/v1/volumes/vol-1", "")
	// vol-2 is bound: its deletion is rejected without an event.
	do(h, http.MethodDelete, "/v1/volumes/vol-2", "")
	do(h, http.MethodPost, "/v1/volumes", `{"id":"vol-x","poolId":"missing","sizeBytes":1}`)
	do(h, http.MethodDelete, "/v1/storage-pools/missing", "")
	do(h, http.MethodDelete, "/v1/volumes/nope", "")
	do(h, http.MethodDelete, "/v1/snapshots/nope", "")
	do(h, http.MethodDelete, "/v1/buckets/nope", "")
	// Object write rejected for insufficient capacity (bucket still exists).
	if rec := do(h, http.MethodPut, "/v1/buckets/bucket-1/objects/k", strings.Repeat("z", 5000)); rec.Code != http.StatusConflict {
		t.Fatalf("oversized put = %d %s", rec.Code, rec.Body.String())
	}
	do(h, http.MethodPost, "/v1/volumes", `{"id":"bad","poolId":"pool-a","sizeBytes":0}`)
	do(h, http.MethodDelete, "/v1/buckets/bucket-1", "")         // empty -> 204 (records below)
	do(h, http.MethodPut, "/v1/buckets/bucket-1/objects/k", "x") // bucket gone -> 404

	got := auditItems(t, getAuditPage(t, h, "/v1/audit-events?limit=1000"))
	// Newly recorded events after baseline:
	//   reservation r1 (the retry is a no-op),
	//   snapshot snap-1 (retry no-op),
	//   clone vol-2 (retry no-op),
	//   bind vol-2,
	//   delete vol-1 (unbound, so 204 even though snap-1 exists),
	//   delete bucket-1 (empty -> 204).
	want := baseline + 6
	if len(got) != want {
		t.Fatalf("event count = %d, want %d; tail events: %v", len(got), want, got[baseline:])
	}
}

// TestAuditEventsPagination walks the log with after/limit and checks
// nextAfter/hasMore.
func TestAuditEventsPagination(t *testing.T) {
	h := Handler()
	createPool(t, h, poolA)
	for i := 1; i <= 5; i++ {
		body := fmt.Sprintf(`{"requestId":"r%d","bytes":10}`, i)
		if rec := reserve(t, h, "pool-a", body); rec.Code != http.StatusCreated {
			t.Fatalf("reserve r%d = %d", i, rec.Code)
		}
	}
	// Events: 1=pool.created, then 2..6 reservations.

	first := getAuditPage(t, h, "/v1/audit-events?limit=2")
	items := auditItems(t, first)
	if len(items) != 2 {
		t.Fatalf("first page = %v", items)
	}
	eventEq(t, items[0], 1, "pool.created", "/v1/storage-pools/pool-a", "pool-a", 0)
	if items[1]["sequence"].(float64) != 2 {
		t.Fatalf("second seq = %v", items[1])
	}
	if first["nextAfter"].(float64) != 2 || !first["hasMore"].(bool) {
		t.Fatalf("first page cursor = nextAfter %v hasMore %v", first["nextAfter"], first["hasMore"])
	}

	second := getAuditPage(t, h, "/v1/audit-events?after=2&limit=2")
	items = auditItems(t, second)
	if len(items) != 2 || items[0]["sequence"].(float64) != 3 || items[1]["sequence"].(float64) != 4 {
		t.Fatalf("second page = %v", items)
	}
	if second["nextAfter"].(float64) != 4 || !second["hasMore"].(bool) {
		t.Fatalf("second page cursor = %v %v", second["nextAfter"], second["hasMore"])
	}

	third := getAuditPage(t, h, "/v1/audit-events?after=4&limit=2")
	items = auditItems(t, third)
	if len(items) != 2 || items[0]["sequence"].(float64) != 5 || items[1]["sequence"].(float64) != 6 {
		t.Fatalf("third page = %v", items)
	}
	// Exactly fills the last page with nothing after it.
	if third["nextAfter"].(float64) != 6 || third["hasMore"].(bool) {
		t.Fatalf("third page cursor = %v %v", third["nextAfter"], third["hasMore"])
	}

	// after at the last sequence returns an empty page echoing after.
	tail := getAuditPage(t, h, "/v1/audit-events?after=6")
	if len(auditItems(t, tail)) != 0 || tail["nextAfter"].(float64) != 6 || tail["hasMore"].(bool) {
		t.Fatalf("tail page = %v", tail)
	}
	// after beyond the maximum sequence behaves the same.
	past := getAuditPage(t, h, "/v1/audit-events?after=999")
	if len(auditItems(t, past)) != 0 || past["nextAfter"].(float64) != 999 || past["hasMore"].(bool) {
		t.Fatalf("past-end page = %v", past)
	}

	// Default limit (100) returns everything.
	all := auditItems(t, getAuditPage(t, h, "/v1/audit-events"))
	if len(all) != 6 {
		t.Fatalf("default page len = %d", len(all))
	}
}

// TestAuditEventsPoolFilter checks pool scoping, including hasMore within the
// filtered subset and interleaved pools.
func TestAuditEventsPoolFilter(t *testing.T) {
	h := Handler()
	reportFixture(t, h) // see TestAuditEventsLifecycle for the event order.

	page := getAuditPage(t, h, "/v1/audit-events?poolId=pool-b&limit=1000")
	items := auditItems(t, page)
	want := []struct {
		seq      float64
		action   string
		resource string
		delta    float64
	}{
		{2, "pool.created", "/v1/storage-pools/pool-b", 0},
		{6, "clone.created", "/v1/volumes/vol-2", 200},
		{9, "bucket.created", "/v1/buckets/bucket-2", 0},
	}
	if len(items) != len(want) {
		t.Fatalf("pool-b items = %v", items)
	}
	for i, w := range want {
		eventEq(t, items[i], w.seq, w.action, w.resource, "pool-b", w.delta)
	}
	if page["nextAfter"].(float64) != 9 || page["hasMore"].(bool) {
		t.Fatalf("pool-b cursor = %v %v", page["nextAfter"], page["hasMore"])
	}

	// Filtered pagination: pool-b has three matches; limit 1 pages through them
	// and only reports hasMore while filtered matches remain.
	p1 := getAuditPage(t, h, "/v1/audit-events?poolId=pool-b&limit=1")
	if len(auditItems(t, p1)) != 1 || p1["nextAfter"].(float64) != 2 || !p1["hasMore"].(bool) {
		t.Fatalf("filtered p1 = %v", p1)
	}
	p2 := getAuditPage(t, h, "/v1/audit-events?poolId=pool-b&after=2&limit=1")
	if len(auditItems(t, p2)) != 1 || p2["nextAfter"].(float64) != 6 || !p2["hasMore"].(bool) {
		t.Fatalf("filtered p2 = %v", p2)
	}
	p3 := getAuditPage(t, h, "/v1/audit-events?poolId=pool-b&after=6&limit=1")
	if len(auditItems(t, p3)) != 1 || p3["nextAfter"].(float64) != 9 || p3["hasMore"].(bool) {
		t.Fatalf("filtered p3 = %v", p3)
	}
	p4 := getAuditPage(t, h, "/v1/audit-events?poolId=pool-b&after=9&limit=1")
	if len(auditItems(t, p4)) != 0 || p4["nextAfter"].(float64) != 9 || p4["hasMore"].(bool) {
		t.Fatalf("filtered p4 = %v", p4)
	}

	// Combined with after.
	mix := getAuditPage(t, h, "/v1/audit-events?after=6&poolId=pool-b")
	mixItems := auditItems(t, mix)
	if len(mixItems) != 1 || mixItems[0]["sequence"].(float64) != 9 {
		t.Fatalf("mixed filter = %v", mixItems)
	}

	// Well-formed id but no such pool.
	wantError(t, do(h, http.MethodGet, "/v1/audit-events?poolId=missing", ""), http.StatusNotFound, "pool_not_found")
}

// TestAuditEventsQueryErrors covers parameter validation.
func TestAuditEventsQueryErrors(t *testing.T) {
	h := Handler()
	createPool(t, h, poolA)
	for _, target := range []string{
		"/v1/audit-events?after=-1",
		"/v1/audit-events?after=abc",
		"/v1/audit-events?after=1.5",
		"/v1/audit-events?after=",
		"/v1/audit-events?after=1&after=2",
		"/v1/audit-events?limit=0",
		"/v1/audit-events?limit=1001",
		"/v1/audit-events?limit=-1",
		"/v1/audit-events?limit=abc",
		"/v1/audit-events?limit=",
		"/v1/audit-events?limit=1&limit=2",
		"/v1/audit-events?poolId=bad%20id",
		"/v1/audit-events?poolId=",
		"/v1/audit-events?poolId=pool-a&poolId=pool-a",
		"/v1/audit-events?unknown=1",
		"/v1/audit-events?after",
		"/v1/audit-events?limit",
		"/v1/audit-events?after=1&extra",
	} {
		wantError(t, do(h, http.MethodGet, target, ""), http.StatusBadRequest, "invalid_request")
	}

	// Boundary values are accepted.
	for _, target := range []string{
		"/v1/audit-events?after=0",
		"/v1/audit-events?limit=1",
		"/v1/audit-events?limit=1000",
		"/v1/audit-events?after=0&limit=1&poolId=pool-a",
	} {
		if rec := do(h, http.MethodGet, target, ""); rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d %s", target, rec.Code, rec.Body.String())
		}
	}
}

// TestAuditEventsMethodAndRouting pins 405/Allow and sub-path behaviour.
func TestAuditEventsMethodAndRouting(t *testing.T) {
	h := Handler()
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch, http.MethodHead} {
		rec := do(h, method, "/v1/audit-events", "")
		if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET" {
			t.Fatalf("%s = %d allow=%q", method, rec.Code, rec.Header().Get("Allow"))
		}
		wantError(t, rec, http.StatusMethodNotAllowed, "method_not_allowed")
	}
	wantError(t, do(h, http.MethodGet, "/v1/audit-events/extra", ""), http.StatusNotFound, "not_found")
	wantError(t, do(h, http.MethodGet, "/v1/audit-events/", ""), http.StatusNotFound, "not_found")
}

// TestAuditEventsConcurrentCommitOrder hammers concurrent successful
// mutations; the log must number them 1..N contiguously in commit order with
// no duplicates or gaps.
func TestAuditEventsConcurrentCommitOrder(t *testing.T) {
	h := Handler()
	createPool(t, h, `{"id":"cp","devices":[{"id":"d","capacityBytes":100000,"faultDomain":"fd"}]}`)

	const n = 100
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			body := fmt.Sprintf(`{"requestId":"r%d","bytes":10}`, i)
			if rec := reserve(t, h, "cp", body); rec.Code != http.StatusCreated {
				t.Errorf("reserve r%d = %d", i, rec.Code)
			}
		}(i)
	}
	wg.Wait()

	items := auditItems(t, getAuditPage(t, h, "/v1/audit-events?limit=1000"))
	// pool.created plus n reservations.
	if len(items) != n+1 {
		t.Fatalf("events = %d, want %d", len(items), n+1)
	}
	seen := make(map[int64]bool, n+1)
	for i, e := range items {
		seq := int64(e["sequence"].(float64))
		if seq != int64(i+1) {
			t.Fatalf("sequence gap/order at %d: %d", i, seq)
		}
		if seen[seq] {
			t.Fatalf("duplicate sequence %d", seq)
		}
		seen[seq] = true
		if i > 0 && e["action"] != "reservation.created" {
			t.Fatalf("unexpected action at %d: %v", i, e["action"])
		}
		if e["poolId"] != "cp" && i > 0 {
			t.Fatalf("unexpected pool at %d: %v", i, e["poolId"])
		}
	}
}

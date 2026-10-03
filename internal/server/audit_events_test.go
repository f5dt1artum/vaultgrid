package server

import (
	"fmt"
	"net/http"
	"sync"
	"testing"
)

// auditFixture drives one of every audited action, interleaved with failures,
// idempotent retries and successful no-ops, none of which may add an event.
//
// Successful events, in commit order:
//
//	 1 pool.created        pool-a /v1/storage-pools/pool-a                0
//	 2 pool.created        pool-b /v1/storage-pools/pool-b                0
//	 3 reservation.created pool-a .../reservations/r1                   100
//	 4 volume.created      pool-a /v1/volumes/vol-1                     200
//	 5 volume.bound        pool-a /v1/volumes/vol-1/binding               0
//	 6 volume.unbound      pool-a /v1/volumes/vol-1/binding               0
//	 7 snapshot.created    pool-a /v1/snapshots/snap-1                   200
//	 8 clone.created       pool-b /v1/volumes/vol-2                      200
//	 9 bucket.created      pool-a /v1/buckets/bucket-1                     0
//	10 object.created      pool-a /v1/buckets/bucket-1/objects/k           3
//	11 object.overwritten  pool-a .../objects/k (same size)                0
//	12 object.overwritten  pool-a .../objects/k (shrunk)                  -2
//	13 object.deleted      pool-a .../objects/k                            -1
//	14 bucket.deleted      pool-a /v1/buckets/bucket-1                     0
//	15 snapshot.deleted    pool-a /v1/snapshots/snap-1                  -200
//	16 volume.deleted      pool-a /v1/volumes/vol-1                      -200
//	17 volume.deleted      pool-b /v1/volumes/vol-2                      -200
//	18 reservation.deleted pool-a .../reservations/r1                   -100
//	19 pool.deleted        pool-a /v1/storage-pools/pool-a                 0
//	20 pool.deleted        pool-b /v1/storage-pools/pool-b                 0
func auditFixture(t *testing.T, h http.Handler) {
	t.Helper()
	createPool(t, h, poolA)
	createPool(t, h, `{"id":"pool-b","devices":[{"id":"dev3","capacityBytes":800,"faultDomain":"rack1"}]}`)

	if rec := reserve(t, h, "pool-a", `{"requestId":"r1","bytes":100}`); rec.Code != http.StatusCreated {
		t.Fatalf("reserve = %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(h, http.MethodPost, "/v1/volumes", `{"id":"vol-1","poolId":"pool-a","sizeBytes":200}`); rec.Code != http.StatusCreated {
		t.Fatalf("create volume = %d %s", rec.Code, rec.Body.String())
	}

	// Failures and idempotent retries before the bind: no events.
	if rec := do(h, http.MethodPost, "/v1/volumes", `{"id":"vol-1","poolId":"pool-a","sizeBytes":200}`); rec.Code != http.StatusOK {
		t.Fatalf("volume idempotent retry = %d %s", rec.Code, rec.Body.String())
	}
	wantError(t, reserve(t, h, "pool-a", `{"requestId":"rX-big","bytes":2000}`), http.StatusConflict, "insufficient_capacity")
	wantError(t, do(h, http.MethodPut, "/v1/volumes/vol-1/binding", `{"nodeId":"n","expectedGeneration":9}`), http.StatusConflict, "stale_generation")

	bind := `{"nodeId":"node-1","expectedGeneration":0}`
	if rec := do(h, http.MethodPut, "/v1/volumes/vol-1/binding", bind); rec.Code != http.StatusOK {
		t.Fatalf("bind = %d %s", rec.Code, rec.Body.String())
	}
	// Same-node, matching-generation retry is a successful no-op.
	if rec := do(h, http.MethodPut, "/v1/volumes/vol-1/binding", `{"nodeId":"node-1","expectedGeneration":1}`); rec.Code != http.StatusOK {
		t.Fatalf("bind retry = %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(h, http.MethodDelete, "/v1/volumes/vol-1/binding?expectedGeneration=1", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("unbind = %d %s", rec.Code, rec.Body.String())
	}
	// Unbinding an already-unbound volume is a successful no-op.
	if rec := do(h, http.MethodDelete, "/v1/volumes/vol-1/binding?expectedGeneration=2", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("unbind no-op = %d %s", rec.Code, rec.Body.String())
	}

	if rec := do(h, http.MethodPost, "/v1/volumes/vol-1/snapshots", `{"id":"snap-1","expectedGeneration":2}`); rec.Code != http.StatusCreated {
		t.Fatalf("snapshot = %d %s", rec.Code, rec.Body.String())
	}
	// Idempotent snapshot retry, and deleting an unknown reservation: no events.
	if rec := do(h, http.MethodPost, "/v1/volumes/vol-1/snapshots", `{"id":"snap-1","expectedGeneration":2}`); rec.Code != http.StatusOK {
		t.Fatalf("snapshot retry = %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(h, http.MethodDelete, "/v1/storage-pools/pool-a/reservations/rX", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete missing reservation = %d", rec.Code)
	}

	if rec := do(h, http.MethodPost, "/v1/snapshots/snap-1/clones", `{"id":"vol-2","poolId":"pool-b"}`); rec.Code != http.StatusCreated {
		t.Fatalf("clone = %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(h, http.MethodPost, "/v1/snapshots/snap-1/clones", `{"id":"vol-2","poolId":"pool-b"}`); rec.Code != http.StatusOK {
		t.Fatalf("clone retry = %d %s", rec.Code, rec.Body.String())
	}

	if rec := do(h, http.MethodPost, "/v1/buckets", `{"id":"bucket-1","poolId":"pool-a"}`); rec.Code != http.StatusCreated {
		t.Fatalf("bucket = %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(h, http.MethodPost, "/v1/buckets", `{"id":"bucket-1","poolId":"pool-a"}`); rec.Code != http.StatusOK {
		t.Fatalf("bucket retry = %d %s", rec.Code, rec.Body.String())
	}
	objPath := "/v1/buckets/bucket-1/objects/k"
	if rec := do(h, http.MethodPut, objPath, "xxx"); rec.Code != http.StatusCreated {
		t.Fatalf("put object = %d %s", rec.Code, rec.Body.String())
	}
	// Same-size overwrite: delta 0 but still an object.overwritten event.
	if rec := do(h, http.MethodPut, objPath, "yyy"); rec.Code != http.StatusOK {
		t.Fatalf("overwrite = %d %s", rec.Code, rec.Body.String())
	}
	// Shrinking overwrite: delta -2.
	if rec := do(h, http.MethodPut, objPath, "z"); rec.Code != http.StatusOK {
		t.Fatalf("shrink overwrite = %d %s", rec.Code, rec.Body.String())
	}
	// Deleting a missing object is a successful no-op.
	if rec := do(h, http.MethodDelete, "/v1/buckets/bucket-1/objects/missing", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete missing object = %d", rec.Code)
	}
	// Pool still full of state: deletion fails without an event.
	wantError(t, do(h, http.MethodDelete, "/v1/storage-pools/pool-a", ""), http.StatusConflict, "pool_not_empty")

	if rec := do(h, http.MethodDelete, objPath, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete object = %d", rec.Code)
	}
	if rec := do(h, http.MethodDelete, objPath, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("re-delete object = %d", rec.Code)
	}
	if rec := do(h, http.MethodDelete, "/v1/buckets/bucket-1", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete bucket = %d", rec.Code)
	}
	if rec := do(h, http.MethodDelete, "/v1/snapshots/snap-1", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete snapshot = %d", rec.Code)
	}
	if rec := do(h, http.MethodDelete, "/v1/volumes/vol-1", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete volume = %d", rec.Code)
	}
	if rec := do(h, http.MethodDelete, "/v1/volumes/vol-2", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete clone volume = %d", rec.Code)
	}
	if rec := do(h, http.MethodDelete, "/v1/storage-pools/pool-a/reservations/r1", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete reservation = %d", rec.Code)
	}
	if rec := do(h, http.MethodDelete, "/v1/storage-pools/pool-a", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete pool-a = %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(h, http.MethodDelete, "/v1/storage-pools/pool-b", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete pool-b = %d %s", rec.Code, rec.Body.String())
	}
}

type expectedEvent struct {
	sequence float64
	action   string
	resource string
	poolID   string
	delta    float64
}

func expectedAuditFixture() []expectedEvent {
	sp := "/v1/storage-pools"
	return []expectedEvent{
		{1, "pool.created", sp + "/pool-a", "pool-a", 0},
		{2, "pool.created", sp + "/pool-b", "pool-b", 0},
		{3, "reservation.created", sp + "/pool-a/reservations/r1", "pool-a", 100},
		{4, "volume.created", "/v1/volumes/vol-1", "pool-a", 200},
		{5, "volume.bound", "/v1/volumes/vol-1/binding", "pool-a", 0},
		{6, "volume.unbound", "/v1/volumes/vol-1/binding", "pool-a", 0},
		{7, "snapshot.created", "/v1/snapshots/snap-1", "pool-a", 200},
		{8, "clone.created", "/v1/volumes/vol-2", "pool-b", 200},
		{9, "bucket.created", "/v1/buckets/bucket-1", "pool-a", 0},
		{10, "object.created", "/v1/buckets/bucket-1/objects/k", "pool-a", 3},
		{11, "object.overwritten", "/v1/buckets/bucket-1/objects/k", "pool-a", 0},
		{12, "object.overwritten", "/v1/buckets/bucket-1/objects/k", "pool-a", -2},
		{13, "object.deleted", "/v1/buckets/bucket-1/objects/k", "pool-a", -1},
		{14, "bucket.deleted", "/v1/buckets/bucket-1", "pool-a", 0},
		{15, "snapshot.deleted", "/v1/snapshots/snap-1", "pool-a", -200},
		{16, "volume.deleted", "/v1/volumes/vol-1", "pool-a", -200},
		{17, "volume.deleted", "/v1/volumes/vol-2", "pool-b", -200},
		{18, "reservation.deleted", sp + "/pool-a/reservations/r1", "pool-a", -100},
		{19, "pool.deleted", sp + "/pool-a", "pool-a", 0},
		{20, "pool.deleted", sp + "/pool-b", "pool-b", 0},
	}
}

func getAuditEvents(t *testing.T, h http.Handler, target string) map[string]any {
	t.Helper()
	rec := do(h, http.MethodGet, target, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d %s", target, rec.Code, rec.Body.String())
	}
	return decodeBody(t, rec)
}

func checkEvent(t *testing.T, raw any, want expectedEvent) {
	t.Helper()
	e := raw.(map[string]any)
	got := expectedEvent{
		sequence: e["sequence"].(float64),
		action:   e["action"].(string),
		resource: e["resource"].(string),
		poolID:   e["poolId"].(string),
		delta:    e["bytesDelta"].(float64),
	}
	if got != want {
		t.Fatalf("event = %+v, want %+v", got, want)
	}
}

func TestAuditEventsEmpty(t *testing.T) {
	h := Handler()
	rec := do(h, http.MethodGet, "/v1/audit-events", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if rec.Body.String() != "{\"items\":[],\"nextAfter\":0,\"hasMore\":false}\n" {
		t.Fatalf("empty body = %q", rec.Body.String())
	}
}

func TestAuditEventsFullLifecycle(t *testing.T) {
	h := Handler()
	auditFixture(t, h)

	v := getAuditEvents(t, h, "/v1/audit-events")
	if v["nextAfter"].(float64) != 20 || v["hasMore"].(bool) {
		t.Fatalf("envelope tail = nextAfter %v hasMore %v", v["nextAfter"], v["hasMore"])
	}
	items := v["items"].([]any)
	if len(items) != 20 {
		t.Fatalf("got %d events, want 20: %v", len(items), items)
	}
	// Each event carries exactly the five documented fields.
	for _, raw := range items {
		e := raw.(map[string]any)
		if len(e) != 5 {
			t.Fatalf("event has %d keys: %v", len(e), e)
		}
	}
	for i, want := range expectedAuditFixture() {
		checkEvent(t, items[i], want)
	}

	// The signed deltas of a pool net to zero once everything is torn down.
	v = getAuditEvents(t, h, "/v1/audit-events?limit=1000")
	var sumA, sumB float64
	for _, raw := range v["items"].([]any) {
		e := raw.(map[string]any)
		switch e["poolId"] {
		case "pool-a":
			sumA += e["bytesDelta"].(float64)
		case "pool-b":
			sumB += e["bytesDelta"].(float64)
		}
	}
	if sumA != 0 || sumB != 0 {
		t.Fatalf("delta sums: pool-a %v pool-b %v", sumA, sumB)
	}
}

func TestAuditEventsPagination(t *testing.T) {
	h := Handler()
	auditFixture(t, h)
	want := expectedAuditFixture()

	// Pages of 5 walk the whole log; hasMore is true until the final page.
	v := getAuditEvents(t, h, "/v1/audit-events?limit=5")
	page := v["items"].([]any)
	if len(page) != 5 || !v["hasMore"].(bool) || v["nextAfter"].(float64) != 5 {
		t.Fatalf("page 1 = %v", v)
	}
	for i := 0; i < 5; i++ {
		checkEvent(t, page[i], want[i])
	}

	v = getAuditEvents(t, h, "/v1/audit-events?after=5&limit=5")
	page = v["items"].([]any)
	if len(page) != 5 || !v["hasMore"].(bool) || v["nextAfter"].(float64) != 10 {
		t.Fatalf("page 2 = %v", v)
	}
	for i := 0; i < 5; i++ {
		checkEvent(t, page[i], want[5+i])
	}

	// Final partial page: 5 items, hasMore false, nextAfter = last sequence.
	v = getAuditEvents(t, h, "/v1/audit-events?after=15&limit=5")
	page = v["items"].([]any)
	if len(page) != 5 || v["hasMore"].(bool) || v["nextAfter"].(float64) != 20 {
		t.Fatalf("final page = %v", v)
	}
	for i := 0; i < 5; i++ {
		checkEvent(t, page[i], want[15+i])
	}

	// after at the last sequence returns an empty page echoing after.
	v = getAuditEvents(t, h, "/v1/audit-events?after=20")
	items := v["items"].([]any)
	if len(items) != 0 || v["hasMore"].(bool) || v["nextAfter"].(float64) != 20 {
		t.Fatalf("tail page = %v", v)
	}

	// after beyond the maximum sequence: empty page, nextAfter echoes after.
	v = getAuditEvents(t, h, "/v1/audit-events?after=9999")
	items = v["items"].([]any)
	if len(items) != 0 || v["hasMore"].(bool) || v["nextAfter"].(float64) != 9999 {
		t.Fatalf("beyond-max page = %v", v)
	}
}

func TestAuditEventsPoolFilter(t *testing.T) {
	h := Handler()
	// Events on two surviving pools; pool-b receives only a cross-pool clone.
	createPool(t, h, poolA)
	createPool(t, h, `{"id":"pool-b","devices":[{"id":"dev3","capacityBytes":800,"faultDomain":"rack1"}]}`)
	if rec := do(h, http.MethodPost, "/v1/volumes", `{"id":"vol-1","poolId":"pool-a","sizeBytes":200}`); rec.Code != http.StatusCreated {
		t.Fatalf("create volume = %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(h, http.MethodPost, "/v1/volumes/vol-1/snapshots", `{"id":"snap-1","expectedGeneration":0}`); rec.Code != http.StatusCreated {
		t.Fatalf("create snapshot = %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(h, http.MethodPost, "/v1/snapshots/snap-1/clones", `{"id":"vol-2","poolId":"pool-b"}`); rec.Code != http.StatusCreated {
		t.Fatalf("create clone = %d %s", rec.Code, rec.Body.String())
	}

	// Global order: 1 pool.created pool-a, 2 pool.created pool-b,
	// 3 volume.created pool-a, 4 snapshot.created pool-a, 5 clone.created pool-b.
	v := getAuditEvents(t, h, "/v1/audit-events?poolId=pool-b")
	items := v["items"].([]any)
	want := []expectedEvent{
		{2, "pool.created", "/v1/storage-pools/pool-b", "pool-b", 0},
		{5, "clone.created", "/v1/volumes/vol-2", "pool-b", 200},
	}
	if len(items) != 2 {
		t.Fatalf("pool-b events = %v", items)
	}
	for i, w := range want {
		checkEvent(t, items[i], w)
	}
	if v["nextAfter"].(float64) != 5 || v["hasMore"].(bool) {
		t.Fatalf("pool-b envelope = %v", v)
	}

	// Filter composes with after and limit; numbering stays global.
	v = getAuditEvents(t, h, "/v1/audit-events?poolId=pool-a&after=2&limit=1")
	items = v["items"].([]any)
	if len(items) != 1 || !v["hasMore"].(bool) || v["nextAfter"].(float64) != 3 {
		t.Fatalf("filtered page = %v", v)
	}
	checkEvent(t, items[0], expectedEvent{3, "volume.created", "/v1/volumes/vol-1", "pool-a", 200})

	// Next page skips past the pool-b clone at seq 5.
	v = getAuditEvents(t, h, "/v1/audit-events?poolId=pool-a&after=3&limit=5")
	items = v["items"].([]any)
	if len(items) != 1 || v["hasMore"].(bool) || v["nextAfter"].(float64) != 4 {
		t.Fatalf("filtered second page = %v", v)
	}
	checkEvent(t, items[0], expectedEvent{4, "snapshot.created", "/v1/snapshots/snap-1", "pool-a", 200})
}

func TestAuditEventsQueryErrors(t *testing.T) {
	h := Handler()
	createPool(t, h, poolA)
	for _, target := range []string{
		"/v1/audit-events?after=-1",
		"/v1/audit-events?after=abc",
		"/v1/audit-events?after=1.5",
		"/v1/audit-events?after=",
		"/v1/audit-events?after=%2B1",
		"/v1/audit-events?after=0x1",
		"/v1/audit-events?limit=0",
		"/v1/audit-events?limit=1001",
		"/v1/audit-events?limit=-1",
		"/v1/audit-events?limit=x",
		"/v1/audit-events?limit=",
		"/v1/audit-events?limit=1%20",
		"/v1/audit-events?poolId=bad%20id",
		"/v1/audit-events?poolId=",
		"/v1/audit-events?after=0&after=1",
		"/v1/audit-events?limit=1&limit=2",
		"/v1/audit-events?poolId=pool-a&poolId=pool-a",
		"/v1/audit-events?unknown=1",
		"/v1/audit-events?after",
		"/v1/audit-events?after=1&extra",
	} {
		wantError(t, do(h, http.MethodGet, target, ""), http.StatusBadRequest, "invalid_request")
	}

	// Well-formed poolId that does not exist.
	wantError(t, do(h, http.MethodGet, "/v1/audit-events?poolId=missing", ""), http.StatusNotFound, "pool_not_found")
	wantError(t, do(h, http.MethodGet, "/v1/audit-events?after=1&poolId=missing&limit=10", ""), http.StatusNotFound, "pool_not_found")
}

func TestAuditEventsMethodAndPath(t *testing.T) {
	h := Handler()
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch, http.MethodHead} {
		rec := do(h, method, "/v1/audit-events", "")
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s = %d, want 405", method, rec.Code)
		}
		if allow := rec.Header().Get("Allow"); allow != "GET" {
			t.Fatalf("%s Allow = %q, want GET", method, allow)
		}
		wantError(t, rec, http.StatusMethodNotAllowed, "method_not_allowed")
	}
	for _, target := range []string{"/v1/audit-events/", "/v1/audit-events/extra", "/v1/audit-events/1/2"} {
		wantError(t, do(h, http.MethodGet, target, ""), http.StatusNotFound, "not_found")
	}
}

func TestAuditEventsFailuresLeaveNoEvent(t *testing.T) {
	h := Handler()
	createPool(t, h, poolA)

	// A stream of unsuccessful or read-only requests must stay invisible.
	reserve(t, h, "pool-a", `{"requestId":"r1","bytes":9000}`) // 409 insufficient_capacity
	wantError(t, do(h, http.MethodPost, "/v1/volumes", `{"id":"v1","poolId":"pool-a","sizeBytes":9000}`), http.StatusConflict, "insufficient_capacity")
	wantError(t, do(h, http.MethodPost, "/v1/volumes", `{"id":"v1","poolId":"missing","sizeBytes":1}`), http.StatusNotFound, "pool_not_found")
	wantError(t, do(h, http.MethodPost, "/v1/volumes", `{"id":"v1","poolId":"pool-a"}`), http.StatusBadRequest, "invalid_request")
	wantError(t, do(h, http.MethodDelete, "/v1/volumes/v1", ""), http.StatusNotFound, "volume_not_found")
	wantError(t, do(h, http.MethodDelete, "/v1/snapshots/s1", ""), http.StatusNotFound, "snapshot_not_found")
	wantError(t, do(h, http.MethodDelete, "/v1/buckets/b1", ""), http.StatusNotFound, "bucket_not_found")
	do(h, http.MethodGet, "/v1/volumes", "")
	do(h, http.MethodGet, "/v1/capacity-report", "")
	do(h, http.MethodGet, "/healthz", "")

	v := getAuditEvents(t, h, "/v1/audit-events")
	items := v["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("want only the pool.created event, got %v", items)
	}
	checkEvent(t, items[0], expectedEvent{1, "pool.created", "/v1/storage-pools/pool-a", "pool-a", 0})
}

// TestAuditEventsConcurrentCommits hammers reservations while reading the
// log. Committed sequences must be dense from 1 with no duplicates or gaps,
// every page must be internally consistent, and the deltas must reconcile
// with the live capacity report.
func TestAuditEventsConcurrentCommits(t *testing.T) {
	h := Handler()
	createPool(t, h, `{"id":"cp","devices":[{"id":"d","capacityBytes":100000,"faultDomain":"fd"}]}`)

	const goroutines, iterations = 8, 40
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				id := fmt.Sprintf("r%d-%d", i, j)
				rec := reserve(t, h, "cp", fmt.Sprintf(`{"requestId":%q,"bytes":1}`, id))
				if rec.Code != http.StatusCreated {
					t.Errorf("reserve = %d %s", rec.Code, rec.Body.String())
					return
				}
				if rec := do(h, http.MethodDelete, "/v1/storage-pools/cp/reservations/"+id, ""); rec.Code != http.StatusNoContent {
					t.Errorf("delete = %d", rec.Code)
					return
				}
			}
		}(i)
	}

	// Readers race the writers above.
	for i := 0; i < 100; i++ {
		v := getAuditEvents(t, h, "/v1/audit-events?limit=1000")
		items := v["items"].([]any)
		var prev float64
		for n, raw := range items {
			seq := raw.(map[string]any)["sequence"].(float64)
			if seq != prev+1 {
				t.Fatalf("page item %d sequence %v follows %v (gap/dup)", n, seq, prev)
			}
			prev = seq
		}
		// nextAfter is the last item (or after=0 on an empty page).
		if len(items) > 0 && v["nextAfter"] != prev {
			t.Fatalf("nextAfter %v != last sequence %v", v["nextAfter"], prev)
		}
		if v["hasMore"].(bool) {
			t.Fatalf("limit 1000 should cover all events, hasMore=true (page %d)", len(items))
		}
	}
	wg.Wait()

	v := getAuditEvents(t, h, "/v1/audit-events?limit=1000")
	items := v["items"].([]any)
	if len(items) != 1+goroutines*iterations*2 {
		t.Fatalf("event count = %d, want %d", len(items), 1+goroutines*iterations*2)
	}
	var created, deleted, sumDelta float64
	for i, raw := range items {
		e := raw.(map[string]any)
		if e["sequence"].(float64) != float64(i+1) {
			t.Fatalf("sequence gap at %d: %v", i, e["sequence"])
		}
		if e["poolId"] != "cp" {
			t.Fatalf("unexpected poolId: %v", e["poolId"])
		}
		sumDelta += e["bytesDelta"].(float64)
		switch e["action"] {
		case "pool.created":
			if i != 0 || e["bytesDelta"].(float64) != 0 {
				t.Fatalf("unexpected first event: %v", e)
			}
		case "reservation.created":
			if e["bytesDelta"].(float64) != 1 {
				t.Fatalf("created delta = %v", e["bytesDelta"])
			}
			created++
		case "reservation.deleted":
			if e["bytesDelta"].(float64) != -1 {
				t.Fatalf("deleted delta = %v", e["bytesDelta"])
			}
			deleted++
		default:
			t.Fatalf("unexpected action %q", e["action"])
		}
	}
	if created != float64(goroutines*iterations) || deleted != created || sumDelta != 0 {
		t.Fatalf("created %v deleted %v delta sum %v", created, deleted, sumDelta)
	}

	// The audit deltas reconcile with the live capacity view.
	report := getReport(t, h, "/v1/capacity-report?poolId=cp")
	item := report["items"].([]any)[0].(map[string]any)
	if item["allocatedBytes"].(float64) != 0 {
		t.Fatalf("allocated = %v, want 0", item["allocatedBytes"])
	}
}

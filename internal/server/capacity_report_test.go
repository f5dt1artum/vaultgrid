package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// reportFixture builds two pools exercising every chargeable category:
// pool-a (raw 1500): reservation 100, volume 200, snapshot 200, object 50.
// pool-b (raw 800): clone 200 into the pool, plus an empty bucket (not metered).
func reportFixture(t *testing.T, h http.Handler) {
	t.Helper()
	createPool(t, h, poolA)
	createPool(t, h, `{"id":"pool-b","devices":[{"id":"dev3","capacityBytes":800,"faultDomain":"rack1"}]}`)

	if rec := reserve(t, h, "pool-a", `{"requestId":"r1","bytes":100}`); rec.Code != http.StatusCreated {
		t.Fatalf("reserve = %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(h, http.MethodPost, "/v1/volumes", `{"id":"vol-1","poolId":"pool-a","sizeBytes":200}`); rec.Code != http.StatusCreated {
		t.Fatalf("create volume = %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(h, http.MethodPost, "/v1/volumes/vol-1/snapshots", `{"id":"snap-1","expectedGeneration":0}`); rec.Code != http.StatusCreated {
		t.Fatalf("create snapshot = %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(h, http.MethodPost, "/v1/snapshots/snap-1/clones", `{"id":"vol-2","poolId":"pool-b"}`); rec.Code != http.StatusCreated {
		t.Fatalf("create clone = %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(h, http.MethodPost, "/v1/buckets", `{"id":"bucket-1","poolId":"pool-a"}`); rec.Code != http.StatusCreated {
		t.Fatalf("create bucket = %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(h, http.MethodPut, "/v1/buckets/bucket-1/objects/k", strings.Repeat("x", 50)); rec.Code != http.StatusCreated {
		t.Fatalf("put object = %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(h, http.MethodPost, "/v1/buckets", `{"id":"bucket-2","poolId":"pool-b"}`); rec.Code != http.StatusCreated {
		t.Fatalf("create empty bucket = %d %s", rec.Code, rec.Body.String())
	}
}

func getReport(t *testing.T, h http.Handler, target string) map[string]any {
	t.Helper()
	rec := do(h, http.MethodGet, target, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d %s", target, rec.Code, rec.Body.String())
	}
	return decodeBody(t, rec)
}

// checkItem verifies the per-pool invariants and returns the row as a map.
func checkItem(t *testing.T, item map[string]any, poolID string, raw, reservation, volume, snapshot, object float64) {
	t.Helper()
	want := map[string]any{
		"poolId": poolID, "rawCapacityBytes": raw, "reservationBytes": reservation,
		"volumeBytes": volume, "snapshotBytes": snapshot, "objectBytes": object,
		"allocatedBytes": reservation + volume + snapshot + object,
		"availableBytes": raw - (reservation + volume + snapshot + object),
	}
	for k, v := range want {
		if item[k] != v {
			t.Fatalf("item[%s] = %v, want %v (row %v)", k, item[k], v, item)
		}
	}
}

func checkSummary(t *testing.T, summary map[string]any, raw, reservation, volume, snapshot, object float64) {
	t.Helper()
	want := map[string]any{
		"rawCapacityBytes": raw, "reservationBytes": reservation,
		"volumeBytes": volume, "snapshotBytes": snapshot, "objectBytes": object,
		"allocatedBytes": reservation + volume + snapshot + object,
		"availableBytes": raw - (reservation + volume + snapshot + object),
	}
	for k, v := range want {
		if summary[k] != v {
			t.Fatalf("summary[%s] = %v, want %v (summary %v)", k, summary[k], v, summary)
		}
	}
	if _, hasPool := summary["poolId"]; hasPool {
		t.Fatalf("summary must not carry poolId: %v", summary)
	}
}

func TestCapacityReportEmpty(t *testing.T) {
	h := Handler()
	v := getReport(t, h, "/v1/capacity-report")
	checkSummary(t, v["summary"].(map[string]any), 0, 0, 0, 0, 0)
	items := v["items"].([]any)
	if len(items) != 0 {
		t.Fatalf("items = %v, want empty", items)
	}
	if !strings.Contains(rec(h, "/v1/capacity-report"), `"items":[]`) {
		t.Fatalf("items must serialize as []")
	}
}

// rec fetches the raw body of a GET for exact-shape assertions.
func rec(h http.Handler, target string) string {
	r := do(h, http.MethodGet, target, "")
	return r.Body.String()
}

func TestCapacityReportJSON(t *testing.T) {
	h := Handler()
	reportFixture(t, h)

	v := getReport(t, h, "/v1/capacity-report")
	items := v["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("items = %v", items)
	}
	// Items are sorted by pool id ascending.
	checkItem(t, items[0].(map[string]any), "pool-a", 1500, 100, 200, 200, 50)
	checkItem(t, items[1].(map[string]any), "pool-b", 800, 0, 200, 0, 0)
	checkSummary(t, v["summary"].(map[string]any), 2300, 100, 400, 200, 50)

	// Explicit format=json matches the default.
	v2 := getReport(t, h, "/v1/capacity-report?format=json")
	checkSummary(t, v2["summary"].(map[string]any), 2300, 100, 400, 200, 50)
}

func TestCapacityReportPoolFilter(t *testing.T) {
	h := Handler()
	reportFixture(t, h)

	v := getReport(t, h, "/v1/capacity-report?poolId=pool-b")
	items := v["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items = %v", items)
	}
	item := items[0].(map[string]any)
	checkItem(t, item, "pool-b", 800, 0, 200, 0, 0)
	// With a single pool filtered, the summary equals that pool's numbers.
	summary := v["summary"].(map[string]any)
	for k, val := range item {
		if k == "poolId" {
			continue
		}
		if summary[k] != val {
			t.Fatalf("summary[%s] = %v, want %v", k, summary[k], val)
		}
	}

	// Combined with an explicit format.
	v = getReport(t, h, "/v1/capacity-report?format=json&poolId=pool-a")
	checkItem(t, v["items"].([]any)[0].(map[string]any), "pool-a", 1500, 100, 200, 200, 50)
}

func TestCapacityReportCSV(t *testing.T) {
	h := Handler()
	reportFixture(t, h)

	rec := do(h, http.MethodGet, "/v1/capacity-report?format=csv", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("csv status = %d %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/csv; charset=utf-8" {
		t.Fatalf("Content-Type = %q", ct)
	}
	want := "poolId,rawCapacityBytes,reservationBytes,volumeBytes,snapshotBytes,objectBytes,allocatedBytes,availableBytes\n" +
		"pool-a,1500,100,200,200,50,550,950\n" +
		"pool-b,800,0,200,0,0,200,600\n"
	if rec.Body.String() != want {
		t.Fatalf("csv body = %q, want %q", rec.Body.String(), want)
	}

	// Filtered CSV carries only that pool's row.
	rec = do(h, http.MethodGet, "/v1/capacity-report?format=csv&poolId=pool-b", "")
	want = "poolId,rawCapacityBytes,reservationBytes,volumeBytes,snapshotBytes,objectBytes,allocatedBytes,availableBytes\n" +
		"pool-b,800,0,200,0,0,200,600\n"
	if rec.Body.String() != want {
		t.Fatalf("filtered csv body = %q, want %q", rec.Body.String(), want)
	}
}

func TestCapacityReportCSVEmpty(t *testing.T) {
	h := Handler()
	rec := do(h, http.MethodGet, "/v1/capacity-report?format=csv", "")
	want := "poolId,rawCapacityBytes,reservationBytes,volumeBytes,snapshotBytes,objectBytes,allocatedBytes,availableBytes\n"
	if rec.Code != http.StatusOK || rec.Body.String() != want {
		t.Fatalf("empty csv = %d %q, want %q", rec.Code, rec.Body.String(), want)
	}
}

func TestCapacityReportErrors(t *testing.T) {
	h := Handler()
	createPool(t, h, poolA)

	// Bad format, bad poolId shape, duplicates, unknown and value-less params.
	for _, target := range []string{
		"/v1/capacity-report?format=xml",
		"/v1/capacity-report?format=",
		"/v1/capacity-report?format=JSON",
		"/v1/capacity-report?poolId=bad%20id",
		"/v1/capacity-report?poolId=",
		"/v1/capacity-report?format=json&format=csv",
		"/v1/capacity-report?poolId=pool-a&poolId=pool-a",
		"/v1/capacity-report?unknown=1",
		"/v1/capacity-report?format",
		"/v1/capacity-report?format=json&extra",
	} {
		wantError(t, do(h, http.MethodGet, target, ""), http.StatusBadRequest, "invalid_request")
	}

	// Well-formed but unknown pool.
	wantError(t, do(h, http.MethodGet, "/v1/capacity-report?poolId=missing", ""), http.StatusNotFound, "pool_not_found")

	// Only GET is allowed.
	rec := do(h, http.MethodPost, "/v1/capacity-report", "")
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET" {
		t.Fatalf("POST = %d allow=%q", rec.Code, rec.Header().Get("Allow"))
	}
	wantError(t, rec, http.StatusMethodNotAllowed, "method_not_allowed")

	// Sub-paths are not found.
	wantError(t, do(h, http.MethodGet, "/v1/capacity-report/extra", ""), http.StatusNotFound, "not_found")
	wantError(t, do(h, http.MethodGet, "/v1/capacity-report/", ""), http.StatusNotFound, "not_found")
}

// TestCapacityReportConsistentUnderConcurrency hammers the store with
// mutations while reports are read; every report must describe a single
// coherent state: per-pool allocated is the sum of the four classes,
// available is raw minus allocated, nothing is negative, and the summary is
// the exact sum of the items.
func TestCapacityReportConsistentUnderConcurrency(t *testing.T) {
	h := Handler()
	createPool(t, h, `{"id":"cp","devices":[{"id":"d","capacityBytes":100000,"faultDomain":"fd"}]}`)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("r%d", i)
			for {
				select {
				case <-stop:
					return
				default:
				}
				reserve(t, h, "cp", fmt.Sprintf(`{"requestId":%q,"bytes":10}`, id))
				do(h, http.MethodDelete, "/v1/storage-pools/cp/reservations/"+id, "")
			}
		}(i)
	}

	for i := 0; i < 200; i++ {
		v := getReport(t, h, "/v1/capacity-report")
		items := v["items"].([]any)
		var sumAlloc, sumAvail, sumRaw float64
		for _, raw := range items {
			item := raw.(map[string]any)
			alloc := item["allocatedBytes"].(float64)
			parts := item["reservationBytes"].(float64) + item["volumeBytes"].(float64) +
				item["snapshotBytes"].(float64) + item["objectBytes"].(float64)
			if alloc != parts {
				t.Fatalf("allocated %v != category sum %v in %v", alloc, parts, item)
			}
			avail := item["availableBytes"].(float64)
			if avail != item["rawCapacityBytes"].(float64)-alloc || avail < 0 {
				t.Fatalf("available %v inconsistent in %v", avail, item)
			}
			sumAlloc += alloc
			sumAvail += avail
			sumRaw += item["rawCapacityBytes"].(float64)
		}
		summary := v["summary"].(map[string]any)
		if summary["allocatedBytes"].(float64) != sumAlloc ||
			summary["availableBytes"].(float64) != sumAvail ||
			summary["rawCapacityBytes"].(float64) != sumRaw {
			t.Fatalf("summary %v does not match items %v", summary, items)
		}
	}
	close(stop)
	wg.Wait()
}

// TestCapacityReportReflectsLifecycle ensures the report tracks charges and
// releases across the existing endpoints without lag or double counting.
func TestCapacityReportReflectsLifecycle(t *testing.T) {
	h := Handler()
	createPool(t, h, poolA)

	reserve(t, h, "pool-a", `{"requestId":"r1","bytes":100}`)
	do(h, http.MethodPost, "/v1/volumes", `{"id":"vol-1","poolId":"pool-a","sizeBytes":200}`)

	v := getReport(t, h, "/v1/capacity-report")
	checkItem(t, v["items"].([]any)[0].(map[string]any), "pool-a", 1500, 100, 200, 0, 0)

	// Idempotent retry does not double count.
	reserve(t, h, "pool-a", `{"requestId":"r1","bytes":100}`)
	v = getReport(t, h, "/v1/capacity-report")
	checkItem(t, v["items"].([]any)[0].(map[string]any), "pool-a", 1500, 100, 200, 0, 0)

	// Deletions release their class.
	do(h, http.MethodDelete, "/v1/storage-pools/pool-a/reservations/r1", "")
	do(h, http.MethodDelete, "/v1/volumes/vol-1", "")
	v = getReport(t, h, "/v1/capacity-report")
	checkItem(t, v["items"].([]any)[0].(map[string]any), "pool-a", 1500, 0, 0, 0, 0)
	checkSummary(t, v["summary"].(map[string]any), 1500, 0, 0, 0, 0)
}

// TestCapacityReportJSONShape guards the exact field set of both formats.
func TestCapacityReportJSONShape(t *testing.T) {
	h := Handler()
	reportFixture(t, h)
	body := rec(h, "/v1/capacity-report?poolId=pool-a")

	var decoded map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatalf("report not JSON: %v", err)
	}
	if len(decoded) != 2 || decoded["summary"] == nil || decoded["items"] == nil {
		t.Fatalf("top-level keys = %v", decoded)
	}
	var item map[string]json.RawMessage
	var items []json.RawMessage
	if err := json.Unmarshal(decoded["items"], &items); err != nil || len(items) != 1 {
		t.Fatalf("items = %s", decoded["items"])
	}
	if err := json.Unmarshal(items[0], &item); err != nil {
		t.Fatalf("item not JSON: %v", err)
	}
	wantKeys := []string{"poolId", "rawCapacityBytes", "reservationBytes", "volumeBytes",
		"snapshotBytes", "objectBytes", "allocatedBytes", "availableBytes"}
	if len(item) != len(wantKeys) {
		t.Fatalf("item keys = %v", item)
	}
	for _, k := range wantKeys {
		if item[k] == nil {
			t.Fatalf("item missing key %q: %v", k, item)
		}
	}
}

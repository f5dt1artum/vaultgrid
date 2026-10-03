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

// reportNumbers mirrors the numeric fields of a report row for assertions.
type reportNumbers struct {
	RawCapacityBytes int64
	ReservationBytes int64
	VolumeBytes      int64
	SnapshotBytes    int64
	ObjectBytes      int64
	AllocatedBytes   int64
	AvailableBytes   int64
}

func decodeReport(t *testing.T, rec *httptest.ResponseRecorder) (reportNumbers, []map[string]any) {
	t.Helper()
	var v struct {
		Summary reportNumbers    `json:"summary"`
		Items   []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("report body %q is not JSON: %v", rec.Body.String(), err)
	}
	return v.Summary, v.Items
}

func itemNumbers(t *testing.T, item map[string]any) reportNumbers {
	t.Helper()
	get := func(key string) int64 {
		n, ok := item[key].(float64)
		if !ok {
			t.Fatalf("item missing numeric field %q: %v", key, item)
		}
		return int64(n)
	}
	return reportNumbers{
		RawCapacityBytes: get("rawCapacityBytes"),
		ReservationBytes: get("reservationBytes"),
		VolumeBytes:      get("volumeBytes"),
		SnapshotBytes:    get("snapshotBytes"),
		ObjectBytes:      get("objectBytes"),
		AllocatedBytes:   get("allocatedBytes"),
		AvailableBytes:   get("availableBytes"),
	}
}

// checkInvariants verifies the per-row accounting identities every report
// row and the summary must satisfy.
func checkInvariants(t *testing.T, label string, n reportNumbers) {
	t.Helper()
	sum := n.ReservationBytes + n.VolumeBytes + n.SnapshotBytes + n.ObjectBytes
	if n.AllocatedBytes != sum {
		t.Fatalf("%s: allocatedBytes %d != category sum %d", label, n.AllocatedBytes, sum)
	}
	if n.AvailableBytes != n.RawCapacityBytes-n.AllocatedBytes {
		t.Fatalf("%s: availableBytes %d != raw %d - allocated %d", label, n.AvailableBytes, n.RawCapacityBytes, n.AllocatedBytes)
	}
	if n.AvailableBytes < 0 || n.AllocatedBytes < 0 {
		t.Fatalf("%s: negative figures: %+v", label, n)
	}
}

// seedReportPool creates a pool with one reservation, one volume, one
// snapshot of it and one object, returning the pool id.
func seedReportPool(t *testing.T, h http.Handler, id string, capacity int64) {
	t.Helper()
	createPool(t, h, fmt.Sprintf(`{"id":%q,"devices":[{"id":"dev-%s","capacityBytes":%d,"faultDomain":"fd"}]}`, id, id, capacity))
	if rec := do(h, http.MethodPost, "/v1/storage-pools/"+id+"/reservations", `{"requestId":"req-1","bytes":100}`); rec.Code != http.StatusCreated {
		t.Fatalf("reservation status = %d body = %s", rec.Code, rec.Body.String())
	}
	if rec := do(h, http.MethodPost, "/v1/volumes", fmt.Sprintf(`{"id":"vol-%s","poolId":%q,"sizeBytes":200}`, id, id)); rec.Code != http.StatusCreated {
		t.Fatalf("volume status = %d body = %s", rec.Code, rec.Body.String())
	}
	if rec := do(h, http.MethodPost, "/v1/volumes/vol-"+id+"/snapshots", `{"id":"snap-`+id+`","expectedGeneration":0}`); rec.Code != http.StatusCreated {
		t.Fatalf("snapshot status = %d body = %s", rec.Code, rec.Body.String())
	}
	if rec := do(h, http.MethodPost, "/v1/buckets", fmt.Sprintf(`{"id":"bkt-%s","poolId":%q}`, id, id)); rec.Code != http.StatusCreated {
		t.Fatalf("bucket status = %d body = %s", rec.Code, rec.Body.String())
	}
	if rec := do(h, http.MethodPut, "/v1/buckets/bkt-"+id+"/objects/obj", strings.Repeat("x", 50)); rec.Code != http.StatusCreated {
		t.Fatalf("object status = %d body = %s", rec.Code, rec.Body.String())
	}
}

func TestCapacityReportEmpty(t *testing.T) {
	h := Handler()
	rec := do(h, http.MethodGet, "/v1/capacity-report", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	summary, items := decodeReport(t, rec)
	if summary != (reportNumbers{}) {
		t.Fatalf("summary = %+v, want all zero", summary)
	}
	if len(items) != 0 {
		t.Fatalf("items = %v, want empty", items)
	}
	if !strings.Contains(rec.Body.String(), `"items":[]`) {
		t.Fatalf("items must be an empty array, body = %s", rec.Body.String())
	}
}

func TestCapacityReportEmptyCSV(t *testing.T) {
	h := Handler()
	rec := do(h, http.MethodGet, "/v1/capacity-report?format=csv", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/csv; charset=utf-8" {
		t.Fatalf("Content-Type = %q", ct)
	}
	if rec.Body.String() != capacityReportCSVHeader+"\n" {
		t.Fatalf("csv body = %q, want header only", rec.Body.String())
	}
}

func TestCapacityReportJSON(t *testing.T) {
	h := Handler()
	seedReportPool(t, h, "pool-b", 1000)
	seedReportPool(t, h, "pool-a", 2000)

	rec := do(h, http.MethodGet, "/v1/capacity-report", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	summary, items := decodeReport(t, rec)
	if len(items) != 2 {
		t.Fatalf("items = %v", items)
	}
	if items[0]["poolId"] != "pool-a" || items[1]["poolId"] != "pool-b" {
		t.Fatalf("items not sorted by poolId: %v", items)
	}
	var total reportNumbers
	for i, item := range items {
		n := itemNumbers(t, item)
		checkInvariants(t, fmt.Sprintf("item %v", item["poolId"]), n)
		want := reportNumbers{RawCapacityBytes: []int64{2000, 1000}[i], ReservationBytes: 100, VolumeBytes: 200, SnapshotBytes: 200, ObjectBytes: 50}
		if n != (reportNumbers{RawCapacityBytes: want.RawCapacityBytes, ReservationBytes: 100, VolumeBytes: 200, SnapshotBytes: 200, ObjectBytes: 50, AllocatedBytes: 550, AvailableBytes: want.RawCapacityBytes - 550}) {
			t.Fatalf("item %v = %+v", item["poolId"], n)
		}
		total.RawCapacityBytes += n.RawCapacityBytes
		total.ReservationBytes += n.ReservationBytes
		total.VolumeBytes += n.VolumeBytes
		total.SnapshotBytes += n.SnapshotBytes
		total.ObjectBytes += n.ObjectBytes
		total.AllocatedBytes += n.AllocatedBytes
		total.AvailableBytes += n.AvailableBytes
	}
	if summary != total {
		t.Fatalf("summary %+v != item total %+v", summary, total)
	}
	checkInvariants(t, "summary", summary)
}

func TestCapacityReportPoolFilter(t *testing.T) {
	h := Handler()
	seedReportPool(t, h, "pool-a", 2000)
	seedReportPool(t, h, "pool-b", 1000)

	rec := do(h, http.MethodGet, "/v1/capacity-report?poolId=pool-b", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	summary, items := decodeReport(t, rec)
	if len(items) != 1 || items[0]["poolId"] != "pool-b" {
		t.Fatalf("items = %v", items)
	}
	n := itemNumbers(t, items[0])
	if summary != n {
		t.Fatalf("summary %+v != single pool %+v", summary, n)
	}
	if n.RawCapacityBytes != 1000 || n.AllocatedBytes != 550 || n.AvailableBytes != 450 {
		t.Fatalf("pool-b figures = %+v", n)
	}
}

func TestCapacityReportCSV(t *testing.T) {
	h := Handler()
	seedReportPool(t, h, "pool-b", 1000)
	seedReportPool(t, h, "pool-a", 2000)

	rec := do(h, http.MethodGet, "/v1/capacity-report?format=csv", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	want := capacityReportCSVHeader + "\n" +
		"pool-a,2000,100,200,200,50,550,1450\n" +
		"pool-b,1000,100,200,200,50,550,450\n"
	if rec.Body.String() != want {
		t.Fatalf("csv body = %q, want %q", rec.Body.String(), want)
	}

	rec = do(h, http.MethodGet, "/v1/capacity-report?format=csv&poolId=pool-b", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	want = capacityReportCSVHeader + "\n" + "pool-b,1000,100,200,200,50,550,450\n"
	if rec.Body.String() != want {
		t.Fatalf("filtered csv body = %q, want %q", rec.Body.String(), want)
	}
}

func TestCapacityReportErrors(t *testing.T) {
	h := Handler()
	seedReportPool(t, h, "pool-a", 2000)

	bad := []string{
		"/v1/capacity-report?format=xml",
		"/v1/capacity-report?format=",
		"/v1/capacity-report?poolId=",
		"/v1/capacity-report?poolId=bad%20id",
		"/v1/capacity-report?format=json&format=csv",
		"/v1/capacity-report?poolId=pool-a&poolId=pool-a",
		"/v1/capacity-report?unknown=1",
		"/v1/capacity-report?format",
		"/v1/capacity-report?poolId",
	}
	for _, target := range bad {
		if rec := do(h, http.MethodGet, target, ""); rec.Code != http.StatusBadRequest {
			t.Fatalf("GET %s status = %d, want 400 (body=%s)", target, rec.Code, rec.Body.String())
		} else {
			wantError(t, rec, http.StatusBadRequest, "invalid_request")
		}
	}

	wantError(t, do(h, http.MethodGet, "/v1/capacity-report?poolId=missing", ""), http.StatusNotFound, "pool_not_found")
	wantError(t, do(h, http.MethodGet, "/v1/capacity-report/extra", ""), http.StatusNotFound, "not_found")

	rec := do(h, http.MethodPost, "/v1/capacity-report", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d, want 405", rec.Code)
	}
	if rec.Header().Get("Allow") != http.MethodGet {
		t.Fatalf("Allow = %q, want GET", rec.Header().Get("Allow"))
	}
	wantError(t, rec, http.StatusMethodNotAllowed, "method_not_allowed")
}

// TestCapacityReportConcurrent hammers the store with mutations while
// readers fetch the report; every snapshot must satisfy the accounting
// identities and never show negative figures.
func TestCapacityReportConcurrent(t *testing.T) {
	h := Handler()
	createPool(t, h, `{"id":"pool-a","devices":[{"id":"dev-a","capacityBytes":100000,"faultDomain":"fd"}]}`)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for n := 0; ; n++ {
				select {
				case <-stop:
					return
				default:
				}
				id := fmt.Sprintf("w%d-%d", worker, n)
				if rec := do(h, http.MethodPost, "/v1/volumes", fmt.Sprintf(`{"id":%q,"poolId":"pool-a","sizeBytes":10}`, id)); rec.Code == http.StatusCreated {
					do(h, http.MethodDelete, "/v1/volumes/"+id, "")
				}
			}
		}(i)
	}
	for i := 0; i < 50; i++ {
		rec := do(h, http.MethodGet, "/v1/capacity-report", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
		}
		summary, items := decodeReport(t, rec)
		checkInvariants(t, "summary", summary)
		var allocated, available, raw int64
		for _, item := range items {
			n := itemNumbers(t, item)
			checkInvariants(t, "item", n)
			allocated += n.AllocatedBytes
			available += n.AvailableBytes
			raw += n.RawCapacityBytes
		}
		if summary.AllocatedBytes != allocated || summary.AvailableBytes != available || summary.RawCapacityBytes != raw {
			t.Fatalf("summary %+v inconsistent with items %+v", summary, items)
		}
	}
	close(stop)
	wg.Wait()
}

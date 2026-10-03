package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func enableVersioning(t *testing.T, h http.Handler, bucket string) {
	t.Helper()
	if rec := do(h, http.MethodPut, "/v1/buckets/"+bucket+"/versioning", `{"enabled":true}`); rec.Code != http.StatusOK {
		t.Fatalf("enable versioning = %d %s", rec.Code, rec.Body.String())
	}
}

func TestVersioningConfig(t *testing.T) {
	h := Handler()
	createPool(t, h, bucketPool)
	createBucket(t, h, `{"id":"b1","poolId":"bp"}`)

	// Disabled by default.
	rec := do(h, http.MethodGet, "/v1/buckets/b1/versioning", "")
	if rec.Code != http.StatusOK || decodeBody(t, rec)["enabled"] != false {
		t.Fatalf("default versioning = %d %s", rec.Code, rec.Body.String())
	}

	// Only {"enabled":true} is accepted.
	for _, body := range []string{
		`{"enabled":false}`, `{}`, `{"enabled":1}`, `{"enabled":true,"extra":1}`, `not json`,
	} {
		wantError(t, do(h, http.MethodPut, "/v1/buckets/b1/versioning", body), http.StatusBadRequest, "invalid_request")
	}
	// Query parameters are rejected; a missing bucket is a 404.
	wantError(t, do(h, http.MethodPut, "/v1/buckets/b1/versioning?x=1", `{"enabled":true}`), http.StatusBadRequest, "invalid_request")
	wantError(t, do(h, http.MethodGet, "/v1/buckets/b1/versioning?x=1", ""), http.StatusBadRequest, "invalid_request")
	wantError(t, do(h, http.MethodGet, "/v1/buckets/nope/versioning", ""), http.StatusNotFound, "bucket_not_found")
	wantError(t, do(h, http.MethodPut, "/v1/buckets/nope/versioning", `{"enabled":true}`), http.StatusNotFound, "bucket_not_found")

	enableVersioning(t, h, "b1")
	if rec := do(h, http.MethodGet, "/v1/buckets/b1/versioning", ""); decodeBody(t, rec)["enabled"] != true {
		t.Fatalf("versioning after enable = %s", rec.Body.String())
	}
	// Idempotent: still 200 and only one audit event.
	enableVersioning(t, h, "b1")
	v := getAuditEvents(t, h, "/v1/audit-events?limit=1000")
	var count int
	for _, raw := range v["items"].([]any) {
		if raw.(map[string]any)["action"] == "bucket.versioning-enabled" {
			count++
			e := raw.(map[string]any)
			if e["resource"] != "/v1/buckets/b1/versioning" || e["poolId"] != "bp" || e["bytesDelta"].(float64) != 0 {
				t.Fatalf("versioning event = %v", e)
			}
		}
	}
	if count != 1 {
		t.Fatalf("versioning-enabled events = %d, want 1", count)
	}

	// Method routing.
	rec = do(h, http.MethodDelete, "/v1/buckets/b1/versioning", "")
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET, PUT" {
		t.Fatalf("DELETE versioning = %d allow=%q", rec.Code, rec.Header().Get("Allow"))
	}
}

func TestVersionedPutCreatesVersions(t *testing.T) {
	h := Handler()
	createPool(t, h, bucketPool)
	createBucket(t, h, `{"id":"b1","poolId":"bp"}`)
	enableVersioning(t, h, "b1")

	put := func(body string) *httptest.ResponseRecorder {
		return putObject(t, h, "b1", "k", body, map[string]string{"X-Vaultgrid-Meta-A": "m"})
	}
	r1 := put("aaa")
	if r1.Code != http.StatusCreated {
		t.Fatalf("first version status = %d %s", r1.Code, r1.Body.String())
	}
	r2 := put("bbbbbb")
	if r2.Code != http.StatusOK {
		t.Fatalf("overwrite version status = %d %s", r2.Code, r2.Body.String())
	}
	// Both versions are unique, non-empty and echoed by header and body.
	id1 := decodeBody(t, r1)["versionId"].(string)
	id2 := decodeBody(t, r2)["versionId"].(string)
	if id1 == "" || id2 == "" || id1 == id2 {
		t.Fatalf("version ids = %q %q", id1, id2)
	}
	if r1.Header().Get(versionHeader) != id1 || r2.Header().Get(versionHeader) != id2 {
		t.Fatalf("version header = %q %q", r1.Header().Get(versionHeader), r2.Header().Get(versionHeader))
	}

	// Every data version is charged in full (3 + 6 = 9).
	if p := getPoolView(t, h, "bp"); p["allocatedBytes"].(float64) != 9 {
		t.Fatalf("pool allocated = %v, want 9", p)
	}
	b := decodeBody(t, do(h, http.MethodGet, "/v1/buckets/b1", ""))
	if b["objectCount"].(float64) != 1 || b["bytesUsed"].(float64) != 9 {
		t.Fatalf("bucket counters = %v", b)
	}

	// Default read returns the latest; an explicit versionId returns the old
	// data version with its header.
	if rec := do(h, http.MethodGet, "/v1/buckets/b1/objects/k", ""); rec.Body.String() != "bbbbbb" {
		t.Fatalf("latest body = %q", rec.Body.String())
	}
	rec := do(h, http.MethodGet, "/v1/buckets/b1/objects/k?versionId="+id1, "")
	if rec.Code != http.StatusOK || rec.Body.String() != "aaa" {
		t.Fatalf("old version = %d %q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get(versionHeader) != id1 || rec.Header().Get("ETag") != sha256ETag("aaa") {
		t.Fatalf("old version headers = %v", rec.Header())
	}
	// HEAD works the same way but without a body.
	rec = do(h, http.MethodHead, "/v1/buckets/b1/objects/k?versionId="+id2, "")
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 || rec.Header().Get(versionHeader) != id2 {
		t.Fatalf("head version = %d %v", rec.Code, rec.Header())
	}
}

func TestVersionedReadsDeleteMarkerAndErrors(t *testing.T) {
	h := Handler()
	createPool(t, h, bucketPool)
	createBucket(t, h, `{"id":"b1","poolId":"bp"}`)
	enableVersioning(t, h, "b1")
	r1 := mustPutObject(t, h, "b1", "k", "aaa", nil)
	id1 := decodeBody(t, r1)["versionId"].(string)

	// Delete creates a marker: default reads 404 object_not_found but the old
	// data version remains addressable.
	if rec := do(h, http.MethodDelete, "/v1/buckets/b1/objects/k", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d", rec.Code)
	}
	wantError(t, do(h, http.MethodGet, "/v1/buckets/b1/objects/k", ""), http.StatusNotFound, "object_not_found")
	wantError(t, do(h, http.MethodHead, "/v1/buckets/b1/objects/k", ""), http.StatusNotFound, "object_not_found")
	if rec := do(h, http.MethodGet, "/v1/buckets/b1/objects/k?versionId="+id1, ""); rec.Code != http.StatusOK || rec.Body.String() != "aaa" {
		t.Fatalf("data version after marker = %d %q", rec.Code, rec.Body.String())
	}

	// Object disappears from the ordinary listing; bytes stay charged.
	if rec := do(h, http.MethodGet, "/v1/buckets/b1/objects", ""); len(decodeBody(t, rec)["items"].([]any)) != 0 {
		t.Fatalf("visible listing should be empty: %s", rec.Body.String())
	}
	b := decodeBody(t, do(h, http.MethodGet, "/v1/buckets/b1", ""))
	if b["objectCount"].(float64) != 0 || b["bytesUsed"].(float64) != 3 {
		t.Fatalf("counters behind marker = %v", b)
	}

	// A delete marker's versionId yields object_version_not_found.
	ov := decodeBody(t, do(h, http.MethodGet, "/v1/buckets/b1/object-versions", ""))["items"].([]any)
	if len(ov) != 2 {
		t.Fatalf("versions = %v", ov)
	}
	marker := ov[0].(map[string]any)
	if marker["deleteMarker"] != true || marker["isLatest"] != true || marker["versionId"] == id1 {
		t.Fatalf("marker row = %v", marker)
	}
	markerID := marker["versionId"].(string)
	wantError(t, do(h, http.MethodGet, "/v1/buckets/b1/objects/k?versionId="+markerID, ""), http.StatusNotFound, "object_version_not_found")
	wantError(t, do(h, http.MethodGet, "/v1/buckets/b1/objects/k?versionId=deadbeef", ""), http.StatusNotFound, "object_version_not_found")

	// Malformed versionId queries are 400.
	for _, q := range []string{"?versionId=", "?versionId=x&versionId=y", "?versionId=x&prefix=k", "?foo=1", "?versionId"} {
		wantError(t, do(h, http.MethodGet, "/v1/buckets/b1/objects/k"+q, ""), http.StatusBadRequest, "invalid_request")
	}
	// A non-versioned bucket rejects versionId queries entirely.
	createBucket(t, h, `{"id":"b2","poolId":"bp"}`)
	mustPutObject(t, h, "b2", "k", "z", nil)
	wantError(t, do(h, http.MethodGet, "/v1/buckets/b2/objects/k?versionId=abc", ""), http.StatusBadRequest, "invalid_request")
}

func TestVersionedDeleteSemantics(t *testing.T) {
	h := Handler()
	createPool(t, h, bucketPool)
	createBucket(t, h, `{"id":"b1","poolId":"bp"}`)
	enableVersioning(t, h, "b1")
	r1 := mustPutObject(t, h, "b1", "k", "aaa", nil)
	r2 := mustPutObject(t, h, "b1", "k", "bbbbbb", nil)
	id1 := decodeBody(t, r1)["versionId"].(string)
	id2 := decodeBody(t, r2)["versionId"].(string)

	if p := getPoolView(t, h, "bp"); p["allocatedBytes"].(float64) != 9 {
		t.Fatalf("allocated before = %v", p)
	}
	// Deleting an old data version permanently frees only its bytes.
	if rec := do(h, http.MethodDelete, "/v1/buckets/b1/objects/k?versionId="+id1, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete old version = %d %s", rec.Code, rec.Body.String())
	}
	if p := getPoolView(t, h, "bp"); p["allocatedBytes"].(float64) != 6 {
		t.Fatalf("allocated after data delete = %v", p)
	}
	wantError(t, do(h, http.MethodDelete, "/v1/buckets/b1/objects/k?versionId="+id1, ""), http.StatusNotFound, "object_version_not_found")
	wantError(t, do(h, http.MethodDelete, "/v1/buckets/b1/objects/k?versionId=unknown", ""), http.StatusNotFound, "object_version_not_found")
	// The visible object is untouched.
	if rec := do(h, http.MethodGet, "/v1/buckets/b1/objects/k", ""); rec.Body.String() != "bbbbbb" {
		t.Fatalf("visible changed = %q", rec.Body.String())
	}

	// A marker delete frees nothing and removes the marker row.
	if rec := do(h, http.MethodDelete, "/v1/buckets/b1/objects/k", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("marker delete = %d", rec.Code)
	}
	ov := decodeBody(t, do(h, http.MethodGet, "/v1/buckets/b1/object-versions", ""))["items"].([]any)
	markerID := ov[0].(map[string]any)["versionId"].(string)
	if ov[0].(map[string]any)["deleteMarker"] != true {
		t.Fatalf("latest row = %v", ov[0])
	}
	if rec := do(h, http.MethodDelete, "/v1/buckets/b1/objects/k?versionId="+markerID, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete marker = %d", rec.Code)
	}
	if p := getPoolView(t, h, "bp"); p["allocatedBytes"].(float64) != 6 {
		t.Fatalf("allocated after marker delete = %v", p)
	}
	// Removing the last data version drops the key; the bucket is deletable.
	if rec := do(h, http.MethodDelete, "/v1/buckets/b1/objects/k?versionId="+id2, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete last version = %d", rec.Code)
	}
	ov = decodeBody(t, do(h, http.MethodGet, "/v1/buckets/b1/object-versions", ""))["items"].([]any)
	if len(ov) != 0 {
		t.Fatalf("versions remain: %v", ov)
	}
	if rec := do(h, http.MethodDelete, "/v1/buckets/b1", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete empty bucket = %d %s", rec.Code, rec.Body.String())
	}

	// Versions or markers block bucket deletion.
	createBucket(t, h, `{"id":"b3","poolId":"bp"}`)
	enableVersioning(t, h, "b3")
	mustPutObject(t, h, "b3", "k", "x", nil)
	do(h, http.MethodDelete, "/v1/buckets/b3/objects/k", "") // leaves data version + marker
	wantError(t, do(h, http.MethodDelete, "/v1/buckets/b3", ""), http.StatusConflict, "bucket_not_empty")

	// Bad queries on delete are 400.
	for _, q := range []string{"?versionId=", "?versionId=x&versionId=y", "?versionId=x&y=1", "?y=1"} {
		wantError(t, do(h, http.MethodDelete, "/v1/buckets/b3/objects/k"+q, ""), http.StatusBadRequest, "invalid_request")
	}
}

func TestVersionedDeleteMarkerNoOp(t *testing.T) {
	h := Handler()
	createPool(t, h, bucketPool)
	createBucket(t, h, `{"id":"b1","poolId":"bp"}`)
	enableVersioning(t, h, "b1")
	mustPutObject(t, h, "b1", "k", "aaa", nil)
	if rec := do(h, http.MethodDelete, "/v1/buckets/b1/objects/k", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("first delete = %d", rec.Code)
	}
	before := len(getAuditEvents(t, h, "/v1/audit-events?limit=1000")["items"].([]any))
	// Latest is already a marker: repeats are 204 with no new marker or event.
	for i := 0; i < 3; i++ {
		if rec := do(h, http.MethodDelete, "/v1/buckets/b1/objects/k", ""); rec.Code != http.StatusNoContent {
			t.Fatalf("repeat marker delete = %d", rec.Code)
		}
	}
	after := len(getAuditEvents(t, h, "/v1/audit-events?limit=1000")["items"].([]any))
	if after != before {
		t.Fatalf("audit events grew %d -> %d", before, after)
	}
	ov := decodeBody(t, do(h, http.MethodGet, "/v1/buckets/b1/object-versions", ""))["items"].([]any)
	if len(ov) != 2 { // one data version + one marker
		t.Fatalf("versions = %v", ov)
	}
}

func TestVersionedConditionalWrites(t *testing.T) {
	h := Handler()
	createPool(t, h, bucketPool)
	createBucket(t, h, `{"id":"b1","poolId":"bp"}`)
	enableVersioning(t, h, "b1")
	r1 := mustPutObject(t, h, "b1", "k", "v1", nil)
	etag1 := decodeBody(t, r1)["etag"].(string)

	// If-Match judges only the visible version.
	if rec := putObject(t, h, "b1", "k", "v2", map[string]string{"If-Match": etag1}); rec.Code != http.StatusOK {
		t.Fatalf("if-match = %d %s", rec.Code, rec.Body.String())
	}
	wantError(t, putObject(t, h, "b1", "k", "v3", map[string]string{"If-Match": etag1}), http.StatusPreconditionFailed, "precondition_failed")

	// A delete marker hides the key: If-None-Match: * succeeds again.
	do(h, http.MethodDelete, "/v1/buckets/b1/objects/k", "")
	if rec := putObject(t, h, "b1", "k", "v4", map[string]string{"If-None-Match": "*"}); rec.Code != http.StatusCreated {
		t.Fatalf("if-none-match after marker = %d %s", rec.Code, rec.Body.String())
	}
	wantError(t, putObject(t, h, "b1", "k", "v5", map[string]string{"If-None-Match": "*"}), http.StatusPreconditionFailed, "precondition_failed")
}

func TestObjectVersionsListing(t *testing.T) {
	h := Handler()
	createPool(t, h, bucketPool)
	createBucket(t, h, `{"id":"b1","poolId":"bp"}`)
	enableVersioning(t, h, "b1")
	r1 := mustPutObject(t, h, "b1", "a", "one", map[string]string{"X-Vaultgrid-Meta-M": "1"})
	mustPutObject(t, h, "b1", "b", "two", nil)
	r3 := mustPutObject(t, h, "b1", "a", "three", nil)
	do(h, http.MethodDelete, "/v1/buckets/b1/objects/a", "")
	id1 := decodeBody(t, r1)["versionId"].(string)
	id3 := decodeBody(t, r3)["versionId"].(string)

	rec := do(h, http.MethodGet, "/v1/buckets/b1/object-versions", "")
	items := decodeBody(t, rec)["items"].([]any)
	// Rows: key a newest-first (marker, v3 "three", v1 "one"), then key b.
	if len(items) != 4 {
		t.Fatalf("rows = %d: %v", len(items), items)
	}
	row := func(i int) map[string]any { return items[i].(map[string]any) }
	if row(0)["key"] != "a" || row(0)["deleteMarker"] != true || row(0)["isLatest"] != true ||
		row(0)["sizeBytes"].(float64) != 0 || row(0)["etag"] != "" {
		t.Fatalf("row0 = %v", row(0))
	}
	if row(1)["key"] != "a" || row(1)["isLatest"] != false || row(1)["versionId"] != id3 ||
		row(1)["sizeBytes"].(float64) != 5 || row(1)["deleteMarker"] != false ||
		row(1)["etag"] != sha256ETag("three") {
		t.Fatalf("row1 = %v", row(1))
	}
	if row(2)["versionId"] != id1 || row(2)["sizeBytes"].(float64) != 3 ||
		row(2)["metadata"].(map[string]any)["M"] != "1" {
		t.Fatalf("row2 = %v", row(2))
	}
	if row(3)["key"] != "b" || row(3)["isLatest"] != true || row(3)["sizeBytes"].(float64) != 3 {
		t.Fatalf("row3 = %v", row(3))
	}

	// Only one prefix parameter is accepted.
	rec = do(h, http.MethodGet, "/v1/buckets/b1/object-versions?prefix=a", "")
	items = decodeBody(t, rec)["items"].([]any)
	if len(items) != 3 {
		t.Fatalf("prefix rows = %v", items)
	}
	for _, q := range []string{"?prefix=a&prefix=b", "?foo=1", "?prefix=a&foo=1"} {
		wantError(t, do(h, http.MethodGet, "/v1/buckets/b1/object-versions"+q, ""), http.StatusBadRequest, "invalid_request")
	}
	wantError(t, do(h, http.MethodGet, "/v1/buckets/nope/object-versions", ""), http.StatusNotFound, "bucket_not_found")
	if rec := do(h, http.MethodPost, "/v1/buckets/b1/object-versions", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST object-versions = %d", rec.Code)
	}
}

func TestVersionedCapacityAndQuotaFailures(t *testing.T) {
	h := Handler()
	createPool(t, h, bucketPool) // 1000
	createBucket(t, h, `{"id":"b1","poolId":"bp"}`)
	enableVersioning(t, h, "b1")
	mustPutObject(t, h, "b1", "k", strings.Repeat("x", 600), nil)
	// A second version is charged in full; one that does not fit fails and
	// leaves both the store and the counters untouched.
	rec := putObject(t, h, "b1", "k", strings.Repeat("y", 500), nil)
	wantError(t, rec, http.StatusConflict, "insufficient_capacity")
	if p := getPoolView(t, h, "bp"); p["allocatedBytes"].(float64) != 600 {
		t.Fatalf("allocated after failed version = %v", p)
	}
	if ov := decodeBody(t, do(h, http.MethodGet, "/v1/buckets/b1/object-versions", ""))["items"].([]any); len(ov) != 1 {
		t.Fatalf("failed write left a version: %v", ov)
	}
	mustPutObject(t, h, "b1", "k", strings.Repeat("y", 400), nil)
	if p := getPoolView(t, h, "bp"); p["allocatedBytes"].(float64) != 1000 {
		t.Fatalf("allocated = %v", p)
	}
}

func TestEnableVersioningMigratesExistingObjects(t *testing.T) {
	h := Handler()
	createPool(t, h, bucketPool)
	createBucket(t, h, `{"id":"b1","poolId":"bp"}`)
	mustPutObject(t, h, "b1", "k", "abc", map[string]string{"X-Vaultgrid-Meta-A": "m"})
	if p := getPoolView(t, h, "bp"); p["allocatedBytes"].(float64) != 3 {
		t.Fatalf("allocated before enable = %v", p)
	}
	enableVersioning(t, h, "b1")
	// Bytes are still charged once; the existing object is now a data version.
	if p := getPoolView(t, h, "bp"); p["allocatedBytes"].(float64) != 3 {
		t.Fatalf("allocated after enable = %v", p)
	}
	ov := decodeBody(t, do(h, http.MethodGet, "/v1/buckets/b1/object-versions", ""))["items"].([]any)
	if len(ov) != 1 {
		t.Fatalf("migrated rows = %v", ov)
	}
	row := ov[0].(map[string]any)
	if row["versionId"].(string) == "" || row["isLatest"] != true || row["deleteMarker"] != false ||
		row["sizeBytes"].(float64) != 3 || row["metadata"].(map[string]any)["A"] != "m" {
		t.Fatalf("migrated row = %v", row)
	}
	id := row["versionId"].(string)
	if rec := do(h, http.MethodGet, "/v1/buckets/b1/objects/k?versionId="+id, ""); rec.Code != http.StatusOK || rec.Body.String() != "abc" {
		t.Fatalf("read migrated = %d %q", rec.Code, rec.Body.String())
	}
}

func TestVersionedAuditActions(t *testing.T) {
	h := Handler()
	createPool(t, h, bucketPool)
	createBucket(t, h, `{"id":"b1","poolId":"bp"}`)
	enableVersioning(t, h, "b1")
	r1 := mustPutObject(t, h, "b1", "k", "abc", nil)
	r2 := mustPutObject(t, h, "b1", "k", "defg", nil)
	id1 := decodeBody(t, r1)["versionId"].(string)
	id2 := decodeBody(t, r2)["versionId"].(string)
	do(h, http.MethodDelete, "/v1/buckets/b1/objects/k", "")
	ov := decodeBody(t, do(h, http.MethodGet, "/v1/buckets/b1/object-versions", ""))["items"].([]any)
	markerID := ov[0].(map[string]any)["versionId"].(string)
	do(h, http.MethodDelete, "/v1/buckets/b1/objects/k?versionId="+markerID, "")
	do(h, http.MethodDelete, "/v1/buckets/b1/objects/k?versionId="+id1, "")
	do(h, http.MethodDelete, "/v1/buckets/b1/objects/k?versionId="+id2, "")

	items := getAuditEvents(t, h, "/v1/audit-events?limit=1000")["items"].([]any)
	var got []string
	for _, raw := range items {
		e := raw.(map[string]any)
		if strings.HasPrefix(e["action"].(string), "object.") || e["action"] == "bucket.versioning-enabled" {
			got = append(got, fmt.Sprintf("%s/%v", e["action"], e["bytesDelta"]))
		}
	}
	want := []string{
		"bucket.versioning-enabled/0",
		"object.version-created/3",
		"object.version-created/4",
		"object.delete-marker-created/0",
		"object.version-deleted/0", // marker
		"object.version-deleted/-3",
		"object.version-deleted/-4",
	}
	if len(got) != len(want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event %d = %q, want %q (all: %v)", i, got[i], want[i], got)
		}
	}
}

func TestConcurrentVersionedPutsSerialize(t *testing.T) {
	h := Handler()
	createPool(t, h, `{"id":"cp","devices":[{"id":"d","capacityBytes":100000,"faultDomain":"fd"}]}`)
	createBucket(t, h, `{"id":"b1","poolId":"cp"}`)
	enableVersioning(t, h, "b1")

	const n = 30
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			putObject(t, h, "b1", "k", "xxxx", nil)
			do(h, http.MethodDelete, "/v1/buckets/b1/objects/other", "")
		}()
	}
	wg.Wait()

	// Whatever the serial order, counters must reconcile exactly with the
	// surviving data versions and markers.
	ov := decodeBody(t, do(h, http.MethodGet, "/v1/buckets/b1/object-versions", ""))["items"].([]any)
	var dataBytes int64
	for _, raw := range ov {
		row := raw.(map[string]any)
		if row["deleteMarker"] == false {
			dataBytes += int64(row["sizeBytes"].(float64))
		}
	}
	p := getPoolView(t, h, "cp")
	if p["allocatedBytes"].(float64) != float64(dataBytes) {
		t.Fatalf("allocated %v != version bytes %d (rows=%d)", p["allocatedBytes"], dataBytes, len(ov))
	}
	b := decodeBody(t, do(h, http.MethodGet, "/v1/buckets/b1", ""))
	if b["bytesUsed"].(float64) != float64(dataBytes) {
		t.Fatalf("bucket bytesUsed %v != %d", b["bytesUsed"], dataBytes)
	}
}

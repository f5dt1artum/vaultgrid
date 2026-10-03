package server

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// enableVersioning enables versioning on bucket and fails the test on any
// non-200 response.
func enableVersioning(t *testing.T, h http.Handler, bucket string) {
	t.Helper()
	rec := do(h, http.MethodPut, "/v1/buckets/"+bucket+"/versioning", `{"enabled":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("enable versioning status = %d body = %s", rec.Code, rec.Body.String())
	}
	if decodeBody(t, rec)["enabled"] != true {
		t.Fatalf("enable versioning body = %s", rec.Body.String())
	}
}

// auditActionsOf collects the actions of all audit events in commit order.
func auditActionsOf(t *testing.T, h http.Handler) []string {
	t.Helper()
	rec := do(h, http.MethodGet, "/v1/audit-events?limit=1000", "")
	items := decodeBody(t, rec)["items"].([]any)
	actions := make([]string, 0, len(items))
	for _, it := range items {
		actions = append(actions, it.(map[string]any)["action"].(string))
	}
	return actions
}

func countAction(actions []string, action string) int {
	n := 0
	for _, a := range actions {
		if a == action {
			n++
		}
	}
	return n
}

func TestVersioningEndpoint(t *testing.T) {
	h := Handler()
	createPool(t, h, bucketPool)
	createBucket(t, h, `{"id":"b1","poolId":"bp"}`)

	// A fresh bucket reports disabled.
	rec := do(h, http.MethodGet, "/v1/buckets/b1/versioning", "")
	if rec.Code != http.StatusOK || decodeBody(t, rec)["enabled"] != false {
		t.Fatalf("get versioning = %d %s", rec.Code, rec.Body.String())
	}

	// Only {"enabled":true} is accepted.
	wantError(t, do(h, http.MethodPut, "/v1/buckets/b1/versioning", `{"enabled":false}`), http.StatusBadRequest, "invalid_request")
	wantError(t, do(h, http.MethodPut, "/v1/buckets/b1/versioning", `{}`), http.StatusBadRequest, "invalid_request")
	wantError(t, do(h, http.MethodPut, "/v1/buckets/b1/versioning", `{"enabled":true,"extra":1}`), http.StatusBadRequest, "invalid_request")
	wantError(t, do(h, http.MethodPut, "/v1/buckets/b1/versioning", `not-json`), http.StatusBadRequest, "invalid_request")
	wantError(t, do(h, http.MethodGet, "/v1/buckets/nope/versioning", ""), http.StatusNotFound, "bucket_not_found")
	wantError(t, do(h, http.MethodPut, "/v1/buckets/nope/versioning", `{"enabled":true}`), http.StatusNotFound, "bucket_not_found")
	if rec := do(h, http.MethodDelete, "/v1/buckets/b1/versioning", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("delete versioning status = %d", rec.Code)
	}

	// Enabling is idempotent, always 200, audited only on the transition.
	enableVersioning(t, h, "b1")
	enableVersioning(t, h, "b1")
	rec = do(h, http.MethodGet, "/v1/buckets/b1/versioning", "")
	if decodeBody(t, rec)["enabled"] != true {
		t.Fatalf("get versioning after enable = %s", rec.Body.String())
	}
	// It cannot be turned back off.
	wantError(t, do(h, http.MethodPut, "/v1/buckets/b1/versioning", `{"enabled":false}`), http.StatusBadRequest, "invalid_request")

	if n := countAction(auditActionsOf(t, h), "bucket.versioning-enabled"); n != 1 {
		t.Fatalf("bucket.versioning-enabled count = %d, want 1", n)
	}
}

func TestVersionedPutGetAndHeaders(t *testing.T) {
	h := Handler()
	createPool(t, h, bucketPool)
	createBucket(t, h, `{"id":"b1","poolId":"bp"}`)

	// Before enabling, writes behave as before: no version id anywhere.
	rec := mustPutObject(t, h, "b1", "k", "v0", nil)
	if rec.Header().Get("X-Vaultgrid-Version-Id") != "" {
		t.Fatalf("unversioned put returned a version id: %q", rec.Header().Get("X-Vaultgrid-Version-Id"))
	}
	if _, present := decodeBody(t, rec)["versionId"]; present {
		t.Fatalf("unversioned put body carries versionId: %s", rec.Body.String())
	}

	enableVersioning(t, h, "b1")

	// The pre-existing object became an addressable version.
	rec = do(h, http.MethodGet, "/v1/buckets/b1/object-versions", "")
	items := decodeBody(t, rec)["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["versionId"] == "" || items[0].(map[string]any)["isLatest"] != true {
		t.Fatalf("versions after enable = %v", items)
	}

	// Writes now generate unique non-empty version ids, in header and body.
	r1 := mustPutObject(t, h, "b1", "k", "v1", nil)
	r2 := mustPutObject(t, h, "b1", "k", "v2", nil)
	id1, id2 := r1.Header().Get("X-Vaultgrid-Version-Id"), r2.Header().Get("X-Vaultgrid-Version-Id")
	if id1 == "" || id2 == "" || id1 == id2 {
		t.Fatalf("version ids = %q, %q", id1, id2)
	}
	if decodeBody(t, r1)["versionId"] != id1 || decodeBody(t, r2)["versionId"] != id2 {
		t.Fatalf("put bodies = %s / %s", r1.Body.String(), r2.Body.String())
	}

	// Plain GET serves the newest generation and carries the version header.
	rec = do(h, http.MethodGet, "/v1/buckets/b1/objects/k", "")
	if rec.Body.String() != "v2" || rec.Header().Get("X-Vaultgrid-Version-Id") != id2 {
		t.Fatalf("get latest = %q version %q", rec.Body.String(), rec.Header().Get("X-Vaultgrid-Version-Id"))
	}
	// A specific version is addressable.
	rec = do(h, http.MethodGet, "/v1/buckets/b1/objects/k?versionId="+id1, "")
	if rec.Body.String() != "v1" || rec.Header().Get("X-Vaultgrid-Version-Id") != id1 {
		t.Fatalf("get v1 = %q version %q", rec.Body.String(), rec.Header().Get("X-Vaultgrid-Version-Id"))
	}
	// HEAD behaves the same.
	rec = do(h, http.MethodHead, "/v1/buckets/b1/objects/k?versionId="+id1, "")
	if rec.Code != http.StatusOK || rec.Header().Get("X-Vaultgrid-Version-Id") != id1 || rec.Body.Len() != 0 {
		t.Fatalf("head v1 = %d version %q body %q", rec.Code, rec.Header().Get("X-Vaultgrid-Version-Id"), rec.Body.String())
	}
	wantError(t, do(h, http.MethodGet, "/v1/buckets/b1/objects/k?versionId=nope", ""), http.StatusNotFound, "object_version_not_found")

	// Malformed version queries.
	for _, q := range []string{"versionId=", "versionId", "versionId=a&versionId=b", "versionId=" + id1 + "&prefix=x", "foo=bar"} {
		wantError(t, do(h, http.MethodGet, "/v1/buckets/b1/objects/k?"+q, ""), http.StatusBadRequest, "invalid_request")
		wantError(t, do(h, http.MethodDelete, "/v1/buckets/b1/objects/k?"+q, ""), http.StatusBadRequest, "invalid_request")
	}
}

func TestVersionedDeleteMarkerFlow(t *testing.T) {
	h := Handler()
	createPool(t, h, bucketPool)
	createBucket(t, h, `{"id":"b1","poolId":"bp"}`)
	enableVersioning(t, h, "b1")
	r1 := mustPutObject(t, h, "b1", "k", "data", nil)
	id1 := r1.Header().Get("X-Vaultgrid-Version-Id")

	// DELETE without a version adds a delete marker.
	if rec := do(h, http.MethodDelete, "/v1/buckets/b1/objects/k", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d", rec.Code)
	}
	wantError(t, do(h, http.MethodGet, "/v1/buckets/b1/objects/k", ""), http.StatusNotFound, "object_not_found")
	wantError(t, do(h, http.MethodHead, "/v1/buckets/b1/objects/k", ""), http.StatusNotFound, "object_not_found")

	// A second DELETE finds the marker already on top: 204, no new marker.
	if rec := do(h, http.MethodDelete, "/v1/buckets/b1/objects/k", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete retry status = %d", rec.Code)
	}
	rec := do(h, http.MethodGet, "/v1/buckets/b1/object-versions", "")
	items := decodeBody(t, rec)["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("version count after marker retry = %d, want 2", len(items))
	}
	latest := items[0].(map[string]any)
	if latest["deleteMarker"] != true || latest["isLatest"] != true || latest["versionId"] == "" {
		t.Fatalf("latest entry = %v", latest)
	}
	older := items[1].(map[string]any)
	if older["deleteMarker"] != false || older["isLatest"] != false || older["versionId"] != id1 {
		t.Fatalf("older entry = %v", older)
	}

	// Addressing the marker itself is not a readable version.
	markerID := latest["versionId"].(string)
	wantError(t, do(h, http.MethodGet, "/v1/buckets/b1/objects/k?versionId="+markerID, ""), http.StatusNotFound, "object_version_not_found")
	// The hidden data version is still readable by id.
	rec = do(h, http.MethodGet, "/v1/buckets/b1/objects/k?versionId="+id1, "")
	if rec.Body.String() != "data" {
		t.Fatalf("get hidden version = %q", rec.Body.String())
	}

	// The ordinary listing hides the key and the bucket counts no objects.
	rec = do(h, http.MethodGet, "/v1/buckets/b1/objects", "")
	if got := decodeBody(t, rec)["items"].([]any); len(got) != 0 {
		t.Fatalf("visible listing = %v", got)
	}
	rec = do(h, http.MethodGet, "/v1/buckets/b1", "")
	if decodeBody(t, rec)["objectCount"].(float64) != 0 {
		t.Fatalf("objectCount with marker on top = %s", rec.Body.String())
	}

	// Permanently deleting the marker exposes the data version again.
	if rec := do(h, http.MethodDelete, "/v1/buckets/b1/objects/k?versionId="+markerID, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete marker status = %d", rec.Code)
	}
	rec = do(h, http.MethodGet, "/v1/buckets/b1/objects/k", "")
	if rec.Body.String() != "data" {
		t.Fatalf("get after marker removal = %q", rec.Body.String())
	}
	// Deleting an unknown version reports the version error.
	wantError(t, do(h, http.MethodDelete, "/v1/buckets/b1/objects/k?versionId="+markerID, ""), http.StatusNotFound, "object_version_not_found")
	wantError(t, do(h, http.MethodDelete, "/v1/buckets/b1/objects/k?versionId=nope", ""), http.StatusNotFound, "object_version_not_found")

	// Permanently deleting the last data version empties the key.
	if rec := do(h, http.MethodDelete, "/v1/buckets/b1/objects/k?versionId="+id1, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete data version status = %d", rec.Code)
	}
	wantError(t, do(h, http.MethodGet, "/v1/buckets/b1/objects/k", ""), http.StatusNotFound, "object_not_found")
	rec = do(h, http.MethodGet, "/v1/buckets/b1/object-versions", "")
	if got := decodeBody(t, rec)["items"].([]any); len(got) != 0 {
		t.Fatalf("versions after full deletion = %v", got)
	}
}

func TestVersionedCapacityAndQuota(t *testing.T) {
	h := Handler()
	createPool(t, h, `{"id":"bp","devices":[{"id":"bdev1","capacityBytes":100,"faultDomain":"rack1"}]}`)
	createBucket(t, h, `{"id":"b1","poolId":"bp"}`)
	enableVersioning(t, h, "b1")

	// Every data version charges the pool and the tenant.
	mustPutObject(t, h, "b1", "k", strings.Repeat("a", 40), nil)
	mustPutObject(t, h, "b1", "k", strings.Repeat("b", 40), nil)
	rec := do(h, http.MethodGet, "/v1/buckets/b1", "")
	if got := decodeBody(t, rec)["bytesUsed"].(float64); got != 80 {
		t.Fatalf("bytesUsed = %v, want 80", got)
	}
	rec = do(h, http.MethodGet, "/v1/storage-pools/bp", "")
	if got := decodeBody(t, rec)["allocatedBytes"].(float64); got != 80 {
		t.Fatalf("allocatedBytes = %v, want 80", got)
	}
	rec = do(h, http.MethodGet, "/v1/storage-pools/bp/tenant-quotas", "")
	// No quota configured yet; set one and verify usedBytes counts versions.
	if rec := do(h, http.MethodPut, "/v1/storage-pools/bp/tenant-quotas/default", `{"limitBytes":90}`); rec.Code != http.StatusCreated {
		t.Fatalf("put quota = %d %s", rec.Code, rec.Body.String())
	}
	rec = do(h, http.MethodGet, "/v1/storage-pools/bp/tenant-quotas/default", "")
	if got := decodeBody(t, rec)["usedBytes"].(float64); got != 80 {
		t.Fatalf("tenant usedBytes = %v, want 80", got)
	}

	// A new version charging 30 more bytes would break the 90 quota.
	wantError(t, putObject(t, h, "b1", "k", strings.Repeat("c", 30), nil), http.StatusConflict, "tenant_quota_exceeded")
	// A delete marker charges nothing and is always allowed.
	if rec := do(h, http.MethodDelete, "/v1/buckets/b1/objects/k", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d", rec.Code)
	}
	// Recreating on top of the marker charges the full size again.
	wantError(t, putObject(t, h, "b1", "k", strings.Repeat("d", 30), nil), http.StatusConflict, "tenant_quota_exceeded")

	// Permanent deletion of a data version releases its bytes.
	rec = do(h, http.MethodGet, "/v1/buckets/b1/object-versions", "")
	items := decodeBody(t, rec)["items"].([]any)
	var dataID string
	for _, it := range items {
		v := it.(map[string]any)
		if v["deleteMarker"] == false {
			dataID = v["versionId"].(string)
			break
		}
	}
	if rec := do(h, http.MethodDelete, "/v1/buckets/b1/objects/k?versionId="+dataID, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete version = %d", rec.Code)
	}
	rec = do(h, http.MethodGet, "/v1/storage-pools/bp/tenant-quotas/default", "")
	if got := decodeBody(t, rec)["usedBytes"].(float64); got != 40 {
		t.Fatalf("tenant usedBytes after version delete = %v, want 40", got)
	}
}

func TestVersionedConditionalWrites(t *testing.T) {
	h := Handler()
	createPool(t, h, bucketPool)
	createBucket(t, h, `{"id":"b1","poolId":"bp"}`)
	enableVersioning(t, h, "b1")
	mustPutObject(t, h, "b1", "k", "v1", nil)
	etag1 := sha256ETag("v1")

	// If-Match judges the visible generation.
	if rec := putObject(t, h, "b1", "k", "v2", map[string]string{"If-Match": etag1}); rec.Code != http.StatusOK {
		t.Fatalf("if-match put = %d", rec.Code)
	}
	wantError(t, putObject(t, h, "b1", "k", "v3", map[string]string{"If-Match": etag1}), http.StatusPreconditionFailed, "precondition_failed")
	wantError(t, putObject(t, h, "b1", "k", "v3", map[string]string{"If-None-Match": "*"}), http.StatusPreconditionFailed, "precondition_failed")

	// With a delete marker on top the key counts as absent.
	if rec := do(h, http.MethodDelete, "/v1/buckets/b1/objects/k", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d", rec.Code)
	}
	wantError(t, putObject(t, h, "b1", "k", "v3", map[string]string{"If-Match": sha256ETag("v2")}), http.StatusPreconditionFailed, "precondition_failed")
	if rec := putObject(t, h, "b1", "k", "v3", map[string]string{"If-None-Match": "*"}); rec.Code != http.StatusCreated {
		t.Fatalf("if-none-match put over marker = %d", rec.Code)
	}
}

func TestObjectVersionsListing(t *testing.T) {
	h := Handler()
	createPool(t, h, bucketPool)
	createBucket(t, h, `{"id":"b1","poolId":"bp"}`)
	enableVersioning(t, h, "b1")
	mustPutObject(t, h, "b1", "a/1", "one", map[string]string{"X-Vaultgrid-Meta-Author": "ann"})
	mustPutObject(t, h, "b1", "a/1", "two", nil)
	mustPutObject(t, h, "b1", "b/1", "three", nil)
	if rec := do(h, http.MethodDelete, "/v1/buckets/b1/objects/b/1", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d", rec.Code)
	}

	rec := do(h, http.MethodGet, "/v1/buckets/b1/object-versions", "")
	items := decodeBody(t, rec)["items"].([]any)
	if len(items) != 4 {
		t.Fatalf("version entries = %d, want 4", len(items))
	}
	// Key ascending; within a key, newest first.
	k0 := items[0].(map[string]any)
	k1 := items[1].(map[string]any)
	k2 := items[2].(map[string]any)
	k3 := items[3].(map[string]any)
	if k0["key"] != "a/1" || k1["key"] != "a/1" || k2["key"] != "b/1" || k3["key"] != "b/1" {
		t.Fatalf("key order = %v %v %v %v", k0["key"], k1["key"], k2["key"], k3["key"])
	}
	if k0["isLatest"] != true || k0["sizeBytes"].(float64) != 3 || k0["etag"] != sha256ETag("two") {
		t.Fatalf("latest a/1 = %v", k0)
	}
	if k1["isLatest"] != false || k1["metadata"].(map[string]any)["Author"] != "ann" {
		t.Fatalf("older a/1 = %v", k1)
	}
	if k2["deleteMarker"] != true || k2["isLatest"] != true || k3["deleteMarker"] != false {
		t.Fatalf("b/1 entries = %v / %v", k2, k3)
	}

	// Prefix filtering.
	rec = do(h, http.MethodGet, "/v1/buckets/b1/object-versions?prefix=a/", "")
	if got := decodeBody(t, rec)["items"].([]any); len(got) != 2 {
		t.Fatalf("prefixed entries = %v", got)
	}
	// Only one prefix parameter is accepted.
	wantError(t, do(h, http.MethodGet, "/v1/buckets/b1/object-versions?prefix=a&prefix=b", ""), http.StatusBadRequest, "invalid_request")
	wantError(t, do(h, http.MethodGet, "/v1/buckets/b1/object-versions?versionId=x", ""), http.StatusBadRequest, "invalid_request")
	wantError(t, do(h, http.MethodGet, "/v1/buckets/nope/object-versions", ""), http.StatusNotFound, "bucket_not_found")
	if rec := do(h, http.MethodPost, "/v1/buckets/b1/object-versions", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("post object-versions = %d", rec.Code)
	}
}

func TestVersionedBucketDeletion(t *testing.T) {
	h := Handler()
	createPool(t, h, bucketPool)
	createBucket(t, h, `{"id":"b1","poolId":"bp"}`)
	enableVersioning(t, h, "b1")
	mustPutObject(t, h, "b1", "k", "data", nil)
	if rec := do(h, http.MethodDelete, "/v1/buckets/b1/objects/k", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d", rec.Code)
	}

	// Hidden generations and markers still block bucket deletion.
	wantError(t, do(h, http.MethodDelete, "/v1/buckets/b1", ""), http.StatusConflict, "bucket_not_empty")

	// Removing every generation frees the bucket.
	rec := do(h, http.MethodGet, "/v1/buckets/b1/object-versions", "")
	for _, it := range decodeBody(t, rec)["items"].([]any) {
		vid := it.(map[string]any)["versionId"].(string)
		if rec := do(h, http.MethodDelete, "/v1/buckets/b1/objects/k?versionId="+vid, ""); rec.Code != http.StatusNoContent {
			t.Fatalf("delete version %s = %d", vid, rec.Code)
		}
	}
	if rec := do(h, http.MethodDelete, "/v1/buckets/b1", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete bucket = %d", rec.Code)
	}
}

func TestUnversionedBucketUnchanged(t *testing.T) {
	h := Handler()
	createPool(t, h, bucketPool)
	createBucket(t, h, `{"id":"b1","poolId":"bp"}`)
	mustPutObject(t, h, "b1", "k", "data", nil)

	// Queries on object reads and deletes stay invalid without versioning.
	wantError(t, do(h, http.MethodGet, "/v1/buckets/b1/objects/k?versionId=x", ""), http.StatusBadRequest, "invalid_request")
	wantError(t, do(h, http.MethodDelete, "/v1/buckets/b1/objects/k?versionId=x", ""), http.StatusBadRequest, "invalid_request")

	// Overwrite replaces and releases; delete is permanent.
	mustPutObject(t, h, "b1", "k", "xx", nil)
	rec := do(h, http.MethodGet, "/v1/buckets/b1", "")
	if got := decodeBody(t, rec)["bytesUsed"].(float64); got != 2 {
		t.Fatalf("bytesUsed = %v, want 2", got)
	}
	if rec := do(h, http.MethodDelete, "/v1/buckets/b1/objects/k", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d", rec.Code)
	}
	wantError(t, do(h, http.MethodGet, "/v1/buckets/b1/objects/k", ""), http.StatusNotFound, "object_not_found")
	rec = do(h, http.MethodGet, "/v1/buckets/b1/object-versions", "")
	if got := decodeBody(t, rec)["items"].([]any); len(got) != 0 {
		t.Fatalf("unversioned versions = %v", got)
	}
}

func TestVersioningAuditEvents(t *testing.T) {
	h := Handler()
	createPool(t, h, bucketPool)
	createBucket(t, h, `{"id":"b1","poolId":"bp"}`)
	enableVersioning(t, h, "b1")
	r := mustPutObject(t, h, "b1", "k", "data", nil)
	id := r.Header().Get("X-Vaultgrid-Version-Id")
	if rec := do(h, http.MethodDelete, "/v1/buckets/b1/objects/k", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d", rec.Code)
	}
	// Failures and no-op retries record nothing.
	wantError(t, do(h, http.MethodDelete, "/v1/buckets/b1/objects/k?versionId=nope", ""), http.StatusNotFound, "object_version_not_found")
	if rec := do(h, http.MethodDelete, "/v1/buckets/b1/objects/k", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete retry = %d", rec.Code)
	}
	if rec := do(h, http.MethodDelete, "/v1/buckets/b1/objects/k?versionId="+id, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete version = %d", rec.Code)
	}

	actions := auditActionsOf(t, h)
	want := []string{
		"pool.created", "bucket.created", "bucket.versioning-enabled",
		"object.version-created", "object.delete-marker-created", "object.version-deleted",
	}
	if fmt.Sprint(actions) != fmt.Sprint(want) {
		t.Fatalf("audit actions = %v, want %v", actions, want)
	}

	// bytesDelta: version-created +4, marker 0, version-deleted -4.
	rec := do(h, http.MethodGet, "/v1/audit-events?limit=1000", "")
	items := decodeBody(t, rec)["items"].([]any)
	deltas := map[string]float64{}
	for _, it := range items {
		e := it.(map[string]any)
		deltas[e["action"].(string)] = e["bytesDelta"].(float64)
	}
	if deltas["object.version-created"] != 4 || deltas["object.delete-marker-created"] != 0 || deltas["object.version-deleted"] != -4 {
		t.Fatalf("bytesDeltas = %v", deltas)
	}
}

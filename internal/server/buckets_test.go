package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

const bucketPool = `{"id":"bp","devices":[{"id":"bdev","capacityBytes":1000,"faultDomain":"rack1"}]}`

func createBucket(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := do(h, http.MethodPost, "/v1/buckets", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create bucket status = %d body = %s", rec.Code, rec.Body.String())
	}
	return rec
}

func putObject(t *testing.T, h http.Handler, bucket, key, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPut, "/v1/buckets/"+bucket+"/objects/"+key, bytes.NewBufferString(body))
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func quotedSHA256(content string) string {
	sum := sha256.Sum256([]byte(content))
	return `"` + hex.EncodeToString(sum[:]) + `"`
}

func TestCreateBucketRepresentation(t *testing.T) {
	h := Handler()
	createPool(t, h, bucketPool)

	rec := createBucket(t, h, `{"id":"b1","poolId":"bp"}`)
	b := decodeBody(t, rec)
	if b["id"] != "b1" || b["poolId"] != "bp" {
		t.Fatalf("bucket view = %v", b)
	}
	if b["objectCount"].(float64) != 0 || b["bytesUsed"].(float64) != 0 {
		t.Fatalf("fresh bucket should be empty: %v", b)
	}
}

func TestCreateBucketIdempotencyAndConflicts(t *testing.T) {
	h := Handler()
	createPool(t, h, bucketPool)
	createPool(t, h, `{"id":"bp2","devices":[{"id":"bdev2","capacityBytes":100,"faultDomain":"rack1"}]}`)
	createBucket(t, h, `{"id":"b1","poolId":"bp"}`)

	// Identical retry: 200, same representation.
	rec := do(h, http.MethodPost, "/v1/buckets", `{"id":"b1","poolId":"bp"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("retry status = %d %s", rec.Code, rec.Body.String())
	}
	if b := decodeBody(t, rec); b["id"] != "b1" || b["poolId"] != "bp" {
		t.Fatalf("retry view = %v", b)
	}
	// Same id pointing at another pool conflicts.
	wantError(t, do(h, http.MethodPost, "/v1/buckets", `{"id":"b1","poolId":"bp2"}`), http.StatusConflict, "bucket_exists")
	// Missing pool.
	wantError(t, do(h, http.MethodPost, "/v1/buckets", `{"id":"b2","poolId":"nope"}`), http.StatusNotFound, "pool_not_found")
	// Invalid ids, unknown fields, malformed JSON.
	wantError(t, do(h, http.MethodPost, "/v1/buckets", `{"id":"bad id","poolId":"bp"}`), http.StatusBadRequest, "invalid_request")
	wantError(t, do(h, http.MethodPost, "/v1/buckets", `{"id":"b3","poolId":"bp","extra":1}`), http.StatusBadRequest, "invalid_request")
	wantError(t, do(h, http.MethodPost, "/v1/buckets", `{"id":"b3"`), http.StatusBadRequest, "invalid_request")
}

func TestListAndGetBuckets(t *testing.T) {
	h := Handler()
	createPool(t, h, bucketPool)
	createBucket(t, h, `{"id":"b2","poolId":"bp"}`)
	createBucket(t, h, `{"id":"b1","poolId":"bp"}`)

	rec := do(h, http.MethodGet, "/v1/buckets", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d", rec.Code)
	}
	items := decodeBody(t, rec)["items"].([]any)
	if len(items) != 2 || items[0].(map[string]any)["id"] != "b1" || items[1].(map[string]any)["id"] != "b2" {
		t.Fatalf("list not sorted by id: %v", items)
	}

	rec = do(h, http.MethodGet, "/v1/buckets/b2", "")
	if rec.Code != http.StatusOK || decodeBody(t, rec)["id"] != "b2" {
		t.Fatalf("get bucket = %d %s", rec.Code, rec.Body.String())
	}
	wantError(t, do(h, http.MethodGet, "/v1/buckets/missing", ""), http.StatusNotFound, "bucket_not_found")
	wantError(t, do(h, http.MethodGet, "/v1/buckets/bad%20id", ""), http.StatusBadRequest, "invalid_request")
}

func TestDeleteBucket(t *testing.T) {
	h := Handler()
	createPool(t, h, bucketPool)
	createBucket(t, h, `{"id":"b1","poolId":"bp"}`)
	putObject(t, h, "b1", "k", "data", nil)

	// Non-empty bucket cannot be deleted.
	wantError(t, do(h, http.MethodDelete, "/v1/buckets/b1", ""), http.StatusConflict, "bucket_not_empty")
	// Empty it, then delete succeeds.
	if rec := do(h, http.MethodDelete, "/v1/buckets/b1/objects/k", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete object = %d", rec.Code)
	}
	if rec := do(h, http.MethodDelete, "/v1/buckets/b1", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete bucket = %d", rec.Code)
	}
	wantError(t, do(h, http.MethodDelete, "/v1/buckets/b1", ""), http.StatusNotFound, "bucket_not_found")
}

func TestBucketKeepsPoolNonDeletable(t *testing.T) {
	h := Handler()
	createPool(t, h, bucketPool)
	createBucket(t, h, `{"id":"b1","poolId":"bp"}`)

	// Even an empty bucket blocks pool deletion.
	wantError(t, do(h, http.MethodDelete, "/v1/storage-pools/bp", ""), http.StatusConflict, "pool_not_empty")
	if rec := do(h, http.MethodDelete, "/v1/buckets/b1", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete bucket = %d", rec.Code)
	}
	if rec := do(h, http.MethodDelete, "/v1/storage-pools/bp", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete pool = %d", rec.Code)
	}
}

func TestPutObjectAndCapacity(t *testing.T) {
	h := Handler()
	createPool(t, h, bucketPool)
	createBucket(t, h, `{"id":"b1","poolId":"bp"}`)

	rec := putObject(t, h, "b1", "dir/a.txt", "hello", map[string]string{
		"X-Vaultgrid-Meta-Author": "me",
		"X-Vaultgrid-Meta-Tag":    "v1",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("put status = %d %s", rec.Code, rec.Body.String())
	}
	etag := quotedSHA256("hello")
	if rec.Header().Get("ETag") != etag {
		t.Fatalf("ETag header = %q, want %q", rec.Header().Get("ETag"), etag)
	}
	o := decodeBody(t, rec)
	if o["key"] != "dir/a.txt" || o["sizeBytes"].(float64) != 5 || o["etag"] != etag {
		t.Fatalf("object view = %v", o)
	}
	meta := o["metadata"].(map[string]any)
	if meta["author"] != "me" || meta["tag"] != "v1" {
		t.Fatalf("metadata = %v", meta)
	}

	// Object bytes charge the pool alongside everything else.
	p := decodeBody(t, do(h, http.MethodGet, "/v1/storage-pools/bp", ""))
	if p["allocatedBytes"].(float64) != 5 || p["availableBytes"].(float64) != 995 {
		t.Fatalf("pool after put = %v", p)
	}
	b := decodeBody(t, do(h, http.MethodGet, "/v1/buckets/b1", ""))
	if b["objectCount"].(float64) != 1 || b["bytesUsed"].(float64) != 5 {
		t.Fatalf("bucket after put = %v", b)
	}

	// Overwrite: 200, only the size delta is charged, metadata replaced.
	rec = putObject(t, h, "b1", "dir/a.txt", "hello world!", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("overwrite status = %d", rec.Code)
	}
	o = decodeBody(t, rec)
	if o["sizeBytes"].(float64) != 12 || o["etag"] != quotedSHA256("hello world!") {
		t.Fatalf("overwrite view = %v", o)
	}
	if len(o["metadata"].(map[string]any)) != 0 {
		t.Fatalf("overwrite should replace metadata wholesale: %v", o["metadata"])
	}
	p = decodeBody(t, do(h, http.MethodGet, "/v1/storage-pools/bp", ""))
	if p["allocatedBytes"].(float64) != 12 {
		t.Fatalf("pool after overwrite = %v", p)
	}

	// Empty content is allowed.
	rec = putObject(t, h, "b1", "empty", "", nil)
	if rec.Code != http.StatusCreated || decodeBody(t, rec)["sizeBytes"].(float64) != 0 {
		t.Fatalf("empty put = %d %s", rec.Code, rec.Body.String())
	}
}

func TestGetAndHeadObject(t *testing.T) {
	h := Handler()
	createPool(t, h, bucketPool)
	createBucket(t, h, `{"id":"b1","poolId":"bp"}`)
	putObject(t, h, "b1", "k", "payload", map[string]string{"X-Vaultgrid-Meta-Author": "me"})

	rec := do(h, http.MethodGet, "/v1/buckets/b1/objects/k", "")
	if rec.Code != http.StatusOK || rec.Body.String() != "payload" {
		t.Fatalf("get = %d %q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("ETag") != quotedSHA256("payload") {
		t.Fatalf("get ETag = %q", rec.Header().Get("ETag"))
	}
	if rec.Header().Get("X-Vaultgrid-Meta-Author") != "me" {
		t.Fatalf("get meta header = %q", rec.Header().Get("X-Vaultgrid-Meta-Author"))
	}

	rec = do(h, http.MethodHead, "/v1/buckets/b1/objects/k", "")
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Fatalf("head = %d body %q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("ETag") != quotedSHA256("payload") || rec.Header().Get("Content-Length") != "7" {
		t.Fatalf("head headers = %v", rec.Header())
	}

	wantError(t, do(h, http.MethodGet, "/v1/buckets/b1/objects/missing", ""), http.StatusNotFound, "object_not_found")
	wantError(t, do(h, http.MethodGet, "/v1/buckets/nope/objects/k", ""), http.StatusNotFound, "bucket_not_found")
	wantError(t, do(h, http.MethodHead, "/v1/buckets/b1/objects/missing", ""), http.StatusNotFound, "object_not_found")
}

func TestDeleteObjectIdempotent(t *testing.T) {
	h := Handler()
	createPool(t, h, bucketPool)
	createBucket(t, h, `{"id":"b1","poolId":"bp"}`)
	putObject(t, h, "b1", "k", "12345", nil)

	if rec := do(h, http.MethodDelete, "/v1/buckets/b1/objects/k", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d", rec.Code)
	}
	p := decodeBody(t, do(h, http.MethodGet, "/v1/storage-pools/bp", ""))
	if p["allocatedBytes"].(float64) != 0 {
		t.Fatalf("pool after delete = %v", p)
	}
	// Repeat deletes (even of never-existing keys) stay 204 while the bucket lives.
	if rec := do(h, http.MethodDelete, "/v1/buckets/b1/objects/k", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("re-delete = %d", rec.Code)
	}
	if rec := do(h, http.MethodDelete, "/v1/buckets/b1/objects/never", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete missing = %d", rec.Code)
	}
	wantError(t, do(h, http.MethodDelete, "/v1/buckets/nope/objects/k", ""), http.StatusNotFound, "bucket_not_found")
}

func TestObjectKeyValidation(t *testing.T) {
	h := Handler()
	createPool(t, h, bucketPool)
	createBucket(t, h, `{"id":"b1","poolId":"bp"}`)

	// Slashes and percent-decoding (once) are fine.
	rec := putObject(t, h, "b1", "a/b%20c/d", "x", nil)
	if rec.Code != http.StatusCreated || decodeBody(t, rec)["key"] != "a/b c/d" {
		t.Fatalf("decoded key = %d %s", rec.Code, rec.Body.String())
	}
	// The decoded form addresses the same object.
	if rec := do(h, http.MethodGet, "/v1/buckets/b1/objects/a/b%20c/d", ""); rec.Code != http.StatusOK {
		t.Fatalf("get decoded key = %d", rec.Code)
	}

	// Empty key, control characters, invalid UTF-8, oversize. (A raw "%zz"
	// request target is rejected by the HTTP server before routing.)
	for _, path := range []string{
		"/v1/buckets/b1/objects/",
		"/v1/buckets/b1/objects/a%00b",
		"/v1/buckets/b1/objects/a%1Fb",
		"/v1/buckets/b1/objects/%7Fb",
		"/v1/buckets/b1/objects/%ff%fe",
		"/v1/buckets/b1/objects/" + strings.Repeat("x", 1025),
	} {
		r := httptest.NewRequest(http.MethodPut, path, bytes.NewBufferString("x"))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("PUT %q = %d, want 400", path, rec.Code)
		}
	}
	// A multi-byte key counts UTF-8 bytes: 512 x 2 bytes = 1024 is the limit.
	key := strings.Repeat("é", 512)
	rec = putObject(t, h, "b1", key, "x", nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("1024-byte key = %d %s", rec.Code, rec.Body.String())
	}
	rec = putObject(t, h, "b1", key+"é", "x", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("1026-byte key = %d", rec.Code)
	}
}

func TestObjectMetadataValidation(t *testing.T) {
	h := Handler()
	createPool(t, h, bucketPool)
	createBucket(t, h, `{"id":"b1","poolId":"bp"}`)

	// Bare prefix header is invalid.
	r := httptest.NewRequest(http.MethodPut, "/v1/buckets/b1/objects/k", bytes.NewBufferString("x"))
	r.Header["X-Vaultgrid-Meta-"] = []string{"v"}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty meta suffix = %d", rec.Code)
	}
	// Header names are case-insensitive; stored keys are lowercased.
	rec = putObject(t, h, "b1", "k", "x", map[string]string{"x-vaultgrid-meta-MixedCase": "v"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("meta put = %d", rec.Code)
	}
	if meta := decodeBody(t, rec)["metadata"].(map[string]any); meta["mixedcase"] != "v" {
		t.Fatalf("metadata = %v", meta)
	}
}

func TestListObjects(t *testing.T) {
	h := Handler()
	createPool(t, h, bucketPool)
	createBucket(t, h, `{"id":"b1","poolId":"bp"}`)
	putObject(t, h, "b1", "logs/b", "1", nil)
	putObject(t, h, "b1", "logs/a", "22", nil)
	putObject(t, h, "b1", "data/c", "333", nil)

	rec := do(h, http.MethodGet, "/v1/buckets/b1/objects", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d", rec.Code)
	}
	items := decodeBody(t, rec)["items"].([]any)
	if len(items) != 3 {
		t.Fatalf("items = %v", items)
	}
	got := []string{items[0].(map[string]any)["key"].(string), items[1].(map[string]any)["key"].(string), items[2].(map[string]any)["key"].(string)}
	if got[0] != "data/c" || got[1] != "logs/a" || got[2] != "logs/b" {
		t.Fatalf("keys not sorted: %v", got)
	}
	first := items[0].(map[string]any)
	if first["sizeBytes"].(float64) != 3 || first["etag"] != quotedSHA256("333") {
		t.Fatalf("summary = %v", first)
	}

	// Prefix filter.
	rec = do(h, http.MethodGet, "/v1/buckets/b1/objects?prefix=logs/", "")
	items = decodeBody(t, rec)["items"].([]any)
	if len(items) != 2 || items[0].(map[string]any)["key"] != "logs/a" {
		t.Fatalf("prefixed items = %v", items)
	}
	// Encoded prefix value.
	rec = do(h, http.MethodGet, "/v1/buckets/b1/objects?prefix=logs%2Fa", "")
	items = decodeBody(t, rec)["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["key"] != "logs/a" {
		t.Fatalf("encoded prefix items = %v", items)
	}
	// Unknown or duplicated parameters are rejected.
	wantError(t, do(h, http.MethodGet, "/v1/buckets/b1/objects?marker=x", ""), http.StatusBadRequest, "invalid_request")
	wantError(t, do(h, http.MethodGet, "/v1/buckets/b1/objects?prefix=a&prefix=b", ""), http.StatusBadRequest, "invalid_request")
	wantError(t, do(h, http.MethodGet, "/v1/buckets/b1/objects?prefix", ""), http.StatusBadRequest, "invalid_request")
	wantError(t, do(h, http.MethodGet, "/v1/buckets/nope/objects", ""), http.StatusNotFound, "bucket_not_found")
}

func TestObjectCapacitySharingAndFailure(t *testing.T) {
	h := Handler()
	createPool(t, h, bucketPool) // 1000 bytes
	createBucket(t, h, `{"id":"b1","poolId":"bp"}`)
	createVol(t, h, `{"id":"vol1","poolId":"bp","sizeBytes":600}`)
	do(h, http.MethodPost, "/v1/storage-pools/bp/reservations", `{"requestId":"r1","bytes":100}`)
	putObject(t, h, "b1", "k", strings.Repeat("x", 200), nil)

	p := decodeBody(t, do(h, http.MethodGet, "/v1/storage-pools/bp", ""))
	if p["allocatedBytes"].(float64) != 900 || p["availableBytes"].(float64) != 100 {
		t.Fatalf("shared capacity = %v", p)
	}

	// Growing the object beyond the remaining capacity fails and preserves
	// the old object and metadata.
	putObject(t, h, "b1", "meta-kept", "old", map[string]string{"X-Vaultgrid-Meta-A": "1"})
	rec := putObject(t, h, "b1", "meta-kept", strings.Repeat("y", 200), map[string]string{"X-Vaultgrid-Meta-A": "2"})
	wantError(t, rec, http.StatusConflict, "insufficient_capacity")
	rec = do(h, http.MethodGet, "/v1/buckets/b1/objects/meta-kept", "")
	if rec.Body.String() != "old" || rec.Header().Get("X-Vaultgrid-Meta-A") != "1" {
		t.Fatalf("failed overwrite changed state: %q %q", rec.Body.String(), rec.Header().Get("X-Vaultgrid-Meta-A"))
	}
	p = decodeBody(t, do(h, http.MethodGet, "/v1/storage-pools/bp", ""))
	if p["allocatedBytes"].(float64) != 903 {
		t.Fatalf("pool after failed overwrite = %v", p)
	}

	// Shrinking always fits.
	rec = putObject(t, h, "b1", "k", "tiny", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("shrink = %d", rec.Code)
	}
	p = decodeBody(t, do(h, http.MethodGet, "/v1/storage-pools/bp", ""))
	if p["allocatedBytes"].(float64) != 707 {
		t.Fatalf("pool after shrink = %v", p)
	}
}

func TestConditionalWrites(t *testing.T) {
	h := Handler()
	createPool(t, h, bucketPool)
	createBucket(t, h, `{"id":"b1","poolId":"bp"}`)
	putObject(t, h, "b1", "k", "v1", nil)
	etag := quotedSHA256("v1")

	// If-Match with the current ETag succeeds.
	rec := putObject(t, h, "b1", "k", "v2", map[string]string{"If-Match": etag})
	if rec.Code != http.StatusOK {
		t.Fatalf("if-match = %d %s", rec.Code, rec.Body.String())
	}
	// Stale ETag fails.
	wantError(t, putObject(t, h, "b1", "k", "v3", map[string]string{"If-Match": etag}), http.StatusPreconditionFailed, "precondition_failed")
	// If-Match on a missing object fails.
	wantError(t, putObject(t, h, "b1", "new", "x", map[string]string{"If-Match": quotedSHA256("x")}), http.StatusPreconditionFailed, "precondition_failed")
	// If-None-Match: * blocks overwrites but allows creates.
	wantError(t, putObject(t, h, "b1", "k", "v3", map[string]string{"If-None-Match": "*"}), http.StatusPreconditionFailed, "precondition_failed")
	rec = putObject(t, h, "b1", "fresh", "x", map[string]string{"If-None-Match": "*"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("if-none-match create = %d", rec.Code)
	}
	// Both headers, or any other form, are 400.
	wantError(t, putObject(t, h, "b1", "k", "x", map[string]string{"If-Match": quotedSHA256("v2"), "If-None-Match": "*"}), http.StatusBadRequest, "invalid_request")
	wantError(t, putObject(t, h, "b1", "k", "x", map[string]string{"If-Match": "*"}), http.StatusBadRequest, "invalid_request")
	wantError(t, putObject(t, h, "b1", "k", "x", map[string]string{"If-Match": "not-an-etag"}), http.StatusBadRequest, "invalid_request")
	wantError(t, putObject(t, h, "b1", "k", "x", map[string]string{"If-None-Match": quotedSHA256("v2")}), http.StatusBadRequest, "invalid_request")
	// Failed preconditions leave state untouched.
	rec = do(h, http.MethodGet, "/v1/buckets/b1/objects/k", "")
	if rec.Body.String() != "v2" {
		t.Fatalf("content after failed conditionals = %q", rec.Body.String())
	}
}

func TestObjectQueryParamsRejected(t *testing.T) {
	h := Handler()
	createPool(t, h, bucketPool)
	createBucket(t, h, `{"id":"b1","poolId":"bp"}`)
	putObject(t, h, "b1", "k", "x", nil)

	wantError(t, putObject(t, h, "b1", "k?x=1", "y", nil), http.StatusBadRequest, "invalid_request")
	wantError(t, do(h, http.MethodGet, "/v1/buckets/b1/objects/k?x=1", ""), http.StatusBadRequest, "invalid_request")
	wantError(t, do(h, http.MethodDelete, "/v1/buckets/b1/objects/k?x=1", ""), http.StatusBadRequest, "invalid_request")
	wantError(t, do(h, http.MethodGet, "/v1/buckets?x=1", ""), http.StatusBadRequest, "invalid_request")
	wantError(t, do(h, http.MethodGet, "/v1/buckets/b1?x=1", ""), http.StatusBadRequest, "invalid_request")
}

func TestBucketMethodAndPathRouting(t *testing.T) {
	h := Handler()
	createPool(t, h, bucketPool)
	createBucket(t, h, `{"id":"b1","poolId":"bp"}`)

	rec := do(h, http.MethodPatch, "/v1/buckets", "")
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET, POST" {
		t.Fatalf("PATCH /v1/buckets = %d allow=%q", rec.Code, rec.Header().Get("Allow"))
	}
	rec = do(h, http.MethodPost, "/v1/buckets/b1", "")
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET, DELETE" {
		t.Fatalf("POST /v1/buckets/b1 = %d allow=%q", rec.Code, rec.Header().Get("Allow"))
	}
	rec = do(h, http.MethodPut, "/v1/buckets/b1/objects", "")
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET" {
		t.Fatalf("PUT objects collection = %d allow=%q", rec.Code, rec.Header().Get("Allow"))
	}
	rec = do(h, http.MethodPatch, "/v1/buckets/b1/objects/k", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PATCH object = %d", rec.Code)
	}
	wantError(t, do(h, http.MethodGet, "/v1/buckets/b1/unknown", ""), http.StatusNotFound, "not_found")
}

func TestConcurrentObjectWritesStayConsistent(t *testing.T) {
	h := Handler()
	createPool(t, h, bucketPool) // 1000 bytes
	createBucket(t, h, `{"id":"b1","poolId":"bp"}`)

	// 16 goroutines race to write distinct 100-byte objects into a pool with
	// room for exactly 10 alongside nothing else.
	var wg sync.WaitGroup
	results := make([]int, 16)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprintf("obj-%02d", i)
			rec := putObject(t, h, "b1", key, strings.Repeat("x", 100), nil)
			results[i] = rec.Code
		}(i)
	}
	wg.Wait()

	created := 0
	for _, code := range results {
		if code == http.StatusCreated {
			created++
		} else if code != http.StatusConflict {
			t.Fatalf("unexpected status %d", code)
		}
	}
	if created != 10 {
		t.Fatalf("created = %d, want exactly 10", created)
	}
	p := decodeBody(t, do(h, http.MethodGet, "/v1/storage-pools/bp", ""))
	if p["allocatedBytes"].(float64) != 1000 {
		t.Fatalf("pool allocated = %v", p)
	}
	b := decodeBody(t, do(h, http.MethodGet, "/v1/buckets/b1", ""))
	if b["objectCount"].(float64) != 10 || b["bytesUsed"].(float64) != 1000 {
		t.Fatalf("bucket after race = %v", b)
	}

	// Concurrent deletes of the same objects never double-release.
	keys := decodeBody(t, do(h, http.MethodGet, "/v1/buckets/b1/objects", ""))["items"].([]any)
	wg = sync.WaitGroup{}
	for _, item := range keys {
		key := item.(map[string]any)["key"].(string)
		for range 2 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if rec := do(h, http.MethodDelete, "/v1/buckets/b1/objects/"+key, ""); rec.Code != http.StatusNoContent {
					t.Errorf("delete %s = %d", key, rec.Code)
				}
			}()
		}
	}
	wg.Wait()
	p = decodeBody(t, do(h, http.MethodGet, "/v1/storage-pools/bp", ""))
	if p["allocatedBytes"].(float64) != 0 {
		t.Fatalf("pool after deletes = %v", p)
	}
	b = decodeBody(t, do(h, http.MethodGet, "/v1/buckets/b1", ""))
	if b["objectCount"].(float64) != 0 || b["bytesUsed"].(float64) != 0 {
		t.Fatalf("bucket after deletes = %v", b)
	}
}

func TestConcurrentPutDeleteSameKey(t *testing.T) {
	h := Handler()
	createPool(t, h, bucketPool)
	createBucket(t, h, `{"id":"b1","poolId":"bp"}`)

	// Writers and deleters race on one key; the final state must be coherent:
	// either the object exists with matching charges, or it is gone entirely.
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := strings.Repeat("x", 50+i)
			if i%2 == 0 {
				putObject(t, h, "b1", "k", body, nil)
			} else {
				do(h, http.MethodDelete, "/v1/buckets/b1/objects/k", "")
			}
		}(i)
	}
	wg.Wait()

	b := decodeBody(t, do(h, http.MethodGet, "/v1/buckets/b1", ""))
	p := decodeBody(t, do(h, http.MethodGet, "/v1/storage-pools/bp", ""))
	if b["objectCount"].(float64) == 0 {
		if b["bytesUsed"].(float64) != 0 || p["allocatedBytes"].(float64) != 0 {
			t.Fatalf("object gone but charges remain: bucket=%v pool=%v", b, p)
		}
		return
	}
	if b["bytesUsed"] != p["allocatedBytes"] {
		t.Fatalf("bucket/pool charge mismatch: bucket=%v pool=%v", b, p)
	}
	var list struct {
		Items []struct {
			Key       string `json:"key"`
			SizeBytes int64  `json:"sizeBytes"`
		} `json:"items"`
	}
	rec := do(h, http.MethodGet, "/v1/buckets/b1/objects", "")
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("list decode: %v", err)
	}
	if len(list.Items) != 1 || list.Items[0].SizeBytes != int64(b["bytesUsed"].(float64)) {
		t.Fatalf("list/bucket mismatch: %v vs %v", list.Items, b)
	}
}

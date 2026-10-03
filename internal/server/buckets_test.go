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

const bucketPool = `{"id":"bp","devices":[{"id":"bdev1","capacityBytes":1000,"faultDomain":"rack1"}]}`

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

func mustPutObject(t *testing.T, h http.Handler, bucket, key, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	rec := putObject(t, h, bucket, key, body, headers)
	if rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
		t.Fatalf("put object status = %d body = %s", rec.Code, rec.Body.String())
	}
	return rec
}

func sha256ETag(content string) string {
	sum := sha256.Sum256([]byte(content))
	return `"` + hex.EncodeToString(sum[:]) + `"`
}

func TestCreateBucketRepresentation(t *testing.T) {
	h := Handler()
	createPool(t, h, bucketPool)
	rec := createBucket(t, h, `{"id":"b1","poolId":"bp"}`)
	b := decodeBody(t, rec)
	if b["id"] != "b1" || b["poolId"] != "bp" {
		t.Fatalf("bucket identity = %v", b)
	}
	if b["objectCount"].(float64) != 0 || b["bytesUsed"].(float64) != 0 {
		t.Fatalf("bucket counters = %v", b)
	}
}

func TestCreateBucketIdempotentAndConflicts(t *testing.T) {
	h := Handler()
	createPool(t, h, bucketPool)
	createPool(t, h, `{"id":"bp2","devices":[{"id":"bdev2","capacityBytes":100,"faultDomain":"rack1"}]}`)
	createBucket(t, h, `{"id":"b1","poolId":"bp"}`)

	rec := do(h, http.MethodPost, "/v1/buckets", `{"id":"b1","poolId":"bp"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("retry status = %d body = %s", rec.Code, rec.Body.String())
	}
	wantError(t, do(h, http.MethodPost, "/v1/buckets", `{"id":"b1","poolId":"bp2"}`), http.StatusConflict, "bucket_exists")
	wantError(t, do(h, http.MethodPost, "/v1/buckets", `{"id":"b2","poolId":"nope"}`), http.StatusNotFound, "pool_not_found")
	wantError(t, do(h, http.MethodPost, "/v1/buckets", `{"id":"bad id","poolId":"bp"}`), http.StatusBadRequest, "invalid_request")
	wantError(t, do(h, http.MethodPost, "/v1/buckets", `{"id":"b3","poolId":"bp","extra":1}`), http.StatusBadRequest, "invalid_request")
	wantError(t, do(h, http.MethodPost, "/v1/buckets", `{"id":"b3"}`), http.StatusBadRequest, "invalid_request")
}

func TestListAndGetBuckets(t *testing.T) {
	h := Handler()
	createPool(t, h, bucketPool)
	createBucket(t, h, `{"id":"b2","poolId":"bp"}`)
	createBucket(t, h, `{"id":"b1","poolId":"bp"}`)

	rec := do(h, http.MethodGet, "/v1/buckets", "")
	items := decodeBody(t, rec)["items"].([]any)
	if len(items) != 2 || items[0].(map[string]any)["id"] != "b1" || items[1].(map[string]any)["id"] != "b2" {
		t.Fatalf("bucket list not sorted by id: %v", items)
	}

	rec = do(h, http.MethodGet, "/v1/buckets/b2", "")
	if rec.Code != http.StatusOK || decodeBody(t, rec)["id"] != "b2" {
		t.Fatalf("get bucket = %d %s", rec.Code, rec.Body.String())
	}
	wantError(t, do(h, http.MethodGet, "/v1/buckets/nope", ""), http.StatusNotFound, "bucket_not_found")
}

func TestDeleteBucket(t *testing.T) {
	h := Handler()
	createPool(t, h, bucketPool)
	createBucket(t, h, `{"id":"b1","poolId":"bp"}`)
	mustPutObject(t, h, "b1", "k1", "data", nil)

	wantError(t, do(h, http.MethodDelete, "/v1/buckets/b1", ""), http.StatusConflict, "bucket_not_empty")
	wantError(t, do(h, http.MethodDelete, "/v1/buckets/nope", ""), http.StatusNotFound, "bucket_not_found")

	if rec := do(h, http.MethodDelete, "/v1/buckets/b1/objects/k1", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete object status = %d", rec.Code)
	}
	if rec := do(h, http.MethodDelete, "/v1/buckets/b1", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete bucket status = %d", rec.Code)
	}
	wantError(t, do(h, http.MethodGet, "/v1/buckets/b1", ""), http.StatusNotFound, "bucket_not_found")
}

func TestBucketBlocksPoolDeletion(t *testing.T) {
	h := Handler()
	createPool(t, h, bucketPool)
	createBucket(t, h, `{"id":"b1","poolId":"bp"}`)
	wantError(t, do(h, http.MethodDelete, "/v1/storage-pools/bp", ""), http.StatusConflict, "pool_not_empty")
	if rec := do(h, http.MethodDelete, "/v1/buckets/b1", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete bucket status = %d", rec.Code)
	}
	if rec := do(h, http.MethodDelete, "/v1/storage-pools/bp", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete pool status = %d", rec.Code)
	}
}

func TestPutObjectRepresentationAndETag(t *testing.T) {
	h := Handler()
	createPool(t, h, bucketPool)
	createBucket(t, h, `{"id":"b1","poolId":"bp"}`)

	rec := mustPutObject(t, h, "b1", "hello/world.txt", "hello world", map[string]string{
		"X-Vaultgrid-Meta-Author": "me",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("first put status = %d", rec.Code)
	}
	o := decodeBody(t, rec)
	wantETag := sha256ETag("hello world")
	if o["key"] != "hello/world.txt" || o["etag"] != wantETag {
		t.Fatalf("object key/etag = %v", o)
	}
	if o["sizeBytes"].(float64) != 11 {
		t.Fatalf("sizeBytes = %v", o)
	}
	meta := o["metadata"].(map[string]any)
	if meta["Author"] != "me" {
		t.Fatalf("metadata = %v", meta)
	}
	if rec.Header().Get("ETag") != wantETag {
		t.Fatalf("ETag header = %q", rec.Header().Get("ETag"))
	}

	// Overwrite returns 200 and replaces metadata wholesale.
	rec = mustPutObject(t, h, "b1", "hello/world.txt", "hi", map[string]string{
		"X-Vaultgrid-Meta-Other": "x",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("overwrite status = %d", rec.Code)
	}
	o = decodeBody(t, rec)
	meta = o["metadata"].(map[string]any)
	if len(meta) != 1 || meta["Other"] != "x" {
		t.Fatalf("metadata after overwrite = %v", meta)
	}
	if o["etag"] != sha256ETag("hi") {
		t.Fatalf("etag after overwrite = %v", o["etag"])
	}
}

func TestPutObjectEmptyContent(t *testing.T) {
	h := Handler()
	createPool(t, h, bucketPool)
	createBucket(t, h, `{"id":"b1","poolId":"bp"}`)
	rec := mustPutObject(t, h, "b1", "empty", "", nil)
	o := decodeBody(t, rec)
	if o["sizeBytes"].(float64) != 0 || o["etag"] != sha256ETag("") {
		t.Fatalf("empty object = %v", o)
	}
}

func TestGetAndHeadObject(t *testing.T) {
	h := Handler()
	createPool(t, h, bucketPool)
	createBucket(t, h, `{"id":"b1","poolId":"bp"}`)
	mustPutObject(t, h, "b1", "k", "payload", map[string]string{"X-Vaultgrid-Meta-A": "1"})

	rec := do(h, http.MethodGet, "/v1/buckets/b1/objects/k", "")
	if rec.Code != http.StatusOK || rec.Body.String() != "payload" {
		t.Fatalf("get = %d %q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("ETag") != sha256ETag("payload") {
		t.Fatalf("get ETag = %q", rec.Header().Get("ETag"))
	}
	if rec.Header().Get("X-Vaultgrid-Meta-A") != "1" {
		t.Fatalf("get meta header = %q", rec.Header().Get("X-Vaultgrid-Meta-A"))
	}

	rec = do(h, http.MethodHead, "/v1/buckets/b1/objects/k", "")
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Fatalf("head = %d body=%q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("ETag") != sha256ETag("payload") || rec.Header().Get("X-Vaultgrid-Meta-A") != "1" {
		t.Fatalf("head headers = %v", rec.Header())
	}

	wantError(t, do(h, http.MethodGet, "/v1/buckets/b1/objects/nope", ""), http.StatusNotFound, "object_not_found")
	wantError(t, do(h, http.MethodGet, "/v1/buckets/nope/objects/k", ""), http.StatusNotFound, "bucket_not_found")
	wantError(t, do(h, http.MethodHead, "/v1/buckets/b1/objects/nope", ""), http.StatusNotFound, "object_not_found")
}

func TestDeleteObjectIdempotent(t *testing.T) {
	h := Handler()
	createPool(t, h, bucketPool)
	createBucket(t, h, `{"id":"b1","poolId":"bp"}`)
	mustPutObject(t, h, "b1", "k", "12345", nil)

	if rec := do(h, http.MethodDelete, "/v1/buckets/b1/objects/k", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d", rec.Code)
	}
	if p := getPoolView(t, h, "bp"); p["allocatedBytes"].(float64) != 0 {
		t.Fatalf("allocated after delete = %v", p)
	}
	// Repeat delete is still 204; never-existing keys are 204 too.
	if rec := do(h, http.MethodDelete, "/v1/buckets/b1/objects/k", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("re-delete status = %d", rec.Code)
	}
	if rec := do(h, http.MethodDelete, "/v1/buckets/b1/objects/never", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete missing status = %d", rec.Code)
	}
	wantError(t, do(h, http.MethodDelete, "/v1/buckets/nope/objects/k", ""), http.StatusNotFound, "bucket_not_found")
}

func TestObjectKeyRules(t *testing.T) {
	h := Handler()
	createPool(t, h, bucketPool)
	createBucket(t, h, `{"id":"b1","poolId":"bp"}`)

	// Slashes, including repeated and dot segments, are part of the key.
	mustPutObject(t, h, "b1", "a//b/../c", "x", nil)
	rec := do(h, http.MethodGet, "/v1/buckets/b1/objects/a//b/../c", "")
	if rec.Code != http.StatusOK || rec.Body.String() != "x" {
		t.Fatalf("get slash key = %d %q", rec.Code, rec.Body.String())
	}

	// Percent-encoding is decoded once: PUT a%2Fb stores the key "a/b".
	mustPutObject(t, h, "b1", "a%2Fb", "y", nil)
	rec = do(h, http.MethodGet, "/v1/buckets/b1/objects/a%2Fb", "")
	if rec.Code != http.StatusOK || rec.Body.String() != "y" {
		t.Fatalf("get encoded key = %d %q", rec.Code, rec.Body.String())
	}
	rec = do(h, http.MethodGet, "/v1/buckets/b1/objects/a/b", "")
	if rec.Code != http.StatusOK || rec.Body.String() != "y" {
		t.Fatalf("get decoded key = %d %q", rec.Code, rec.Body.String())
	}
	// %252F decodes once to the literal key "a%2Fb", which does not exist.
	wantError(t, do(h, http.MethodGet, "/v1/buckets/b1/objects/a%252Fb", ""), http.StatusNotFound, "object_not_found")

	// 1024 bytes is fine; 1025 is not; empty key is not.
	long := strings.Repeat("k", 1024)
	if rec := putObject(t, h, "b1", long, "x", nil); rec.Code != http.StatusCreated {
		t.Fatalf("1024-byte key status = %d body = %s", rec.Code, rec.Body.String())
	}
	wantError(t, putObject(t, h, "b1", strings.Repeat("k", 1025), "x", nil), http.StatusBadRequest, "invalid_request")
	wantError(t, putObject(t, h, "b1", "", "x", nil), http.StatusBadRequest, "invalid_request")

	// Control characters are rejected.
	wantError(t, putObject(t, h, "b1", "a%00b", "x", nil), http.StatusBadRequest, "invalid_request")
	wantError(t, putObject(t, h, "b1", "a%7Fb", "x", nil), http.StatusBadRequest, "invalid_request")
}

func TestObjectCapacityAccounting(t *testing.T) {
	h := Handler()
	createPool(t, h, bucketPool) // 1000 bytes
	createBucket(t, h, `{"id":"b1","poolId":"bp"}`)
	createVol(t, h, `{"id":"vol1","poolId":"bp","sizeBytes":400}`)

	mustPutObject(t, h, "b1", "k1", strings.Repeat("x", 500), nil)
	p := getPoolView(t, h, "bp")
	if p["allocatedBytes"].(float64) != 900 || p["availableBytes"].(float64) != 100 {
		t.Fatalf("pool after put = %v", p)
	}
	b := decodeBody(t, do(h, http.MethodGet, "/v1/buckets/b1", ""))
	if b["objectCount"].(float64) != 1 || b["bytesUsed"].(float64) != 500 {
		t.Fatalf("bucket counters = %v", b)
	}

	// Overwrite adjusts only the delta.
	mustPutObject(t, h, "b1", "k1", strings.Repeat("x", 550), nil)
	if p := getPoolView(t, h, "bp"); p["allocatedBytes"].(float64) != 950 {
		t.Fatalf("pool after grow = %v", p)
	}
	mustPutObject(t, h, "b1", "k1", strings.Repeat("x", 100), nil)
	if p := getPoolView(t, h, "bp"); p["allocatedBytes"].(float64) != 500 {
		t.Fatalf("pool after shrink = %v", p)
	}

	// A write exceeding capacity fails and leaves the old object untouched.
	rec := putObject(t, h, "b1", "k1", strings.Repeat("x", 700), map[string]string{"X-Vaultgrid-Meta-N": "1"})
	wantError(t, rec, http.StatusConflict, "insufficient_capacity")
	if p := getPoolView(t, h, "bp"); p["allocatedBytes"].(float64) != 500 {
		t.Fatalf("pool after failed write = %v", p)
	}
	rec = do(h, http.MethodGet, "/v1/buckets/b1/objects/k1", "")
	if rec.Body.String() != strings.Repeat("x", 100) || rec.Header().Get("X-Vaultgrid-Meta-N") != "" {
		t.Fatalf("old object changed after failed write: %q meta=%q", rec.Body.String(), rec.Header().Get("X-Vaultgrid-Meta-N"))
	}

	// A new object that does not fit is rejected too.
	wantError(t, putObject(t, h, "b1", "k2", strings.Repeat("x", 501), nil), http.StatusConflict, "insufficient_capacity")
}

func TestObjectConditionalWrites(t *testing.T) {
	h := Handler()
	createPool(t, h, bucketPool)
	createBucket(t, h, `{"id":"b1","poolId":"bp"}`)
	mustPutObject(t, h, "b1", "k", "v1", nil)
	etag := sha256ETag("v1")

	// If-Match with the current etag succeeds.
	rec := putObject(t, h, "b1", "k", "v2", map[string]string{"If-Match": etag})
	if rec.Code != http.StatusOK {
		t.Fatalf("if-match status = %d body = %s", rec.Code, rec.Body.String())
	}
	// If-Match with a stale etag fails with 412 and changes nothing.
	stale := sha256ETag("other")
	rec = putObject(t, h, "b1", "k", "v3", map[string]string{"If-Match": stale})
	wantError(t, rec, http.StatusPreconditionFailed, "precondition_failed")
	if rec := do(h, http.MethodGet, "/v1/buckets/b1/objects/k", ""); rec.Body.String() != "v2" {
		t.Fatalf("object changed after 412: %q", rec.Body.String())
	}
	// If-Match against a missing object fails.
	wantError(t, putObject(t, h, "b1", "missing", "x", map[string]string{"If-Match": etag}), http.StatusPreconditionFailed, "precondition_failed")

	// If-None-Match: * creates only when absent.
	rec = putObject(t, h, "b1", "new", "x", map[string]string{"If-None-Match": "*"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("if-none-match create status = %d body = %s", rec.Code, rec.Body.String())
	}
	wantError(t, putObject(t, h, "b1", "new", "y", map[string]string{"If-None-Match": "*"}), http.StatusPreconditionFailed, "precondition_failed")

	// Both headers together, or any other form, is a 400.
	wantError(t, putObject(t, h, "b1", "k", "x", map[string]string{"If-Match": etag, "If-None-Match": "*"}), http.StatusBadRequest, "invalid_request")
	wantError(t, putObject(t, h, "b1", "k", "x", map[string]string{"If-Match": "*"}), http.StatusBadRequest, "invalid_request")
	wantError(t, putObject(t, h, "b1", "k", "x", map[string]string{"If-None-Match": etag}), http.StatusBadRequest, "invalid_request")
	wantError(t, putObject(t, h, "b1", "k", "x", map[string]string{"If-Match": "unquoted"}), http.StatusBadRequest, "invalid_request")
}

func TestListObjects(t *testing.T) {
	h := Handler()
	createPool(t, h, bucketPool)
	createBucket(t, h, `{"id":"b1","poolId":"bp"}`)
	mustPutObject(t, h, "b1", "b/2", "2", nil)
	mustPutObject(t, h, "b1", "a/1", "1", nil)
	mustPutObject(t, h, "b1", "c", "3", nil)

	rec := do(h, http.MethodGet, "/v1/buckets/b1/objects", "")
	items := decodeBody(t, rec)["items"].([]any)
	if len(items) != 3 {
		t.Fatalf("list len = %d", len(items))
	}
	keys := []string{items[0].(map[string]any)["key"].(string), items[1].(map[string]any)["key"].(string), items[2].(map[string]any)["key"].(string)}
	if keys[0] != "a/1" || keys[1] != "b/2" || keys[2] != "c" {
		t.Fatalf("list not sorted: %v", keys)
	}
	first := items[0].(map[string]any)
	if first["sizeBytes"].(float64) != 1 || first["etag"] != sha256ETag("1") || first["metadata"].(map[string]any) == nil {
		t.Fatalf("summary = %v", first)
	}

	rec = do(h, http.MethodGet, "/v1/buckets/b1/objects?prefix=b/", "")
	items = decodeBody(t, rec)["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["key"] != "b/2" {
		t.Fatalf("prefix list = %v", items)
	}

	wantError(t, do(h, http.MethodGet, "/v1/buckets/b1/objects?foo=1", ""), http.StatusBadRequest, "invalid_request")
	wantError(t, do(h, http.MethodGet, "/v1/buckets/b1/objects?prefix=a&prefix=b", ""), http.StatusBadRequest, "invalid_request")
	wantError(t, do(h, http.MethodGet, "/v1/buckets/b1/objects?prefix=a&foo=1", ""), http.StatusBadRequest, "invalid_request")
	wantError(t, do(h, http.MethodGet, "/v1/buckets/nope/objects", ""), http.StatusNotFound, "bucket_not_found")
}

func TestObjectMetadataValidation(t *testing.T) {
	h := Handler()
	createPool(t, h, bucketPool)
	createBucket(t, h, `{"id":"b1","poolId":"bp"}`)

	// Empty suffix is invalid.
	r := httptest.NewRequest(http.MethodPut, "/v1/buckets/b1/objects/k", bytes.NewBufferString("x"))
	r.Header["X-Vaultgrid-Meta-"] = []string{"v"}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	wantError(t, rec, http.StatusBadRequest, "invalid_request")

	// Control characters in values are invalid.
	r = httptest.NewRequest(http.MethodPut, "/v1/buckets/b1/objects/k", bytes.NewBufferString("x"))
	r.Header["X-Vaultgrid-Meta-A"] = []string{"bad\x01value"}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	wantError(t, rec, http.StatusBadRequest, "invalid_request")

	// Unknown query parameters on object endpoints are invalid.
	wantError(t, putObject(t, h, "b1", "k2?x=1", "x", nil), http.StatusBadRequest, "invalid_request")
	wantError(t, do(h, http.MethodGet, "/v1/buckets/b1/objects/k?x=1", ""), http.StatusBadRequest, "invalid_request")
	wantError(t, do(h, http.MethodDelete, "/v1/buckets/b1/objects/k?x=1", ""), http.StatusBadRequest, "invalid_request")
}

func TestConcurrentObjectWritesNeverOvercommit(t *testing.T) {
	h := Handler()
	createPool(t, h, bucketPool) // 1000 bytes
	createBucket(t, h, `{"id":"b1","poolId":"bp"}`)

	const n = 20
	var wg sync.WaitGroup
	successes := make(chan int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := putObject(t, h, "b1", fmt.Sprintf("k%d", i), strings.Repeat("x", 100), nil)
			if rec.Code == http.StatusCreated {
				successes <- i
			}
		}(i)
	}
	wg.Wait()
	close(successes)
	count := 0
	for range successes {
		count++
	}
	if count != 10 {
		t.Fatalf("successful writes = %d, want 10", count)
	}
	if p := getPoolView(t, h, "bp"); p["allocatedBytes"].(float64) != 1000 {
		t.Fatalf("allocated = %v", p)
	}
}

func TestConcurrentPutDeleteSameKey(t *testing.T) {
	h := Handler()
	createPool(t, h, bucketPool)
	createBucket(t, h, `{"id":"b1","poolId":"bp"}`)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); putObject(t, h, "b1", "k", "data", nil) }()
		go func() { defer wg.Done(); do(h, http.MethodDelete, "/v1/buckets/b1/objects/k", "") }()
	}
	wg.Wait()
	// Whatever the interleaving, capacity accounting must be consistent with
	// the final state of the bucket.
	b := decodeBody(t, do(h, http.MethodGet, "/v1/buckets/b1", ""))
	p := getPoolView(t, h, "bp")
	if b["bytesUsed"].(float64) != p["allocatedBytes"].(float64) {
		t.Fatalf("bucket bytesUsed %v != pool allocated %v", b["bytesUsed"], p["allocatedBytes"])
	}
}

func TestBucketMethodRouting(t *testing.T) {
	h := Handler()
	createPool(t, h, bucketPool)
	createBucket(t, h, `{"id":"b1","poolId":"bp"}`)

	rec := do(h, http.MethodDelete, "/v1/buckets", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE /v1/buckets = %d", rec.Code)
	}
	rec = do(h, http.MethodPost, "/v1/buckets/b1", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /v1/buckets/b1 = %d", rec.Code)
	}
	rec = do(h, http.MethodPost, "/v1/buckets/b1/objects", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST objects = %d", rec.Code)
	}
	rec = do(h, http.MethodPatch, "/v1/buckets/b1/objects/k", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PATCH object = %d", rec.Code)
	}
	wantError(t, do(h, http.MethodGet, "/v1/buckets/b1/unknown", ""), http.StatusNotFound, "not_found")
	wantError(t, do(h, http.MethodGet, "/v1/buckets/b1/", ""), http.StatusNotFound, "not_found")
}

func TestObjectJSONShape(t *testing.T) {
	h := Handler()
	createPool(t, h, bucketPool)
	createBucket(t, h, `{"id":"b1","poolId":"bp"}`)
	rec := mustPutObject(t, h, "b1", "k", "v", nil)
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"key", "sizeBytes", "metadata", "etag"} {
		if _, ok := raw[field]; !ok {
			t.Fatalf("missing field %q in %s", field, rec.Body.String())
		}
	}
	if string(raw["metadata"]) != "{}" {
		t.Fatalf("empty metadata should be {}, got %s", raw["metadata"])
	}
}

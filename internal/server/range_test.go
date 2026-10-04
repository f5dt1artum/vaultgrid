package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// rangeRequest issues a GET/HEAD read, optionally carrying a Range header.
// An empty rangeVal sends no Range header at all.
func rangeRequest(h http.Handler, method, target, rangeVal string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, nil)
	if rangeVal != "" {
		r.Header.Set("Range", rangeVal)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func TestRangeFullReadUnchanged(t *testing.T) {
	h := Handler()
	createPool(t, h, bucketPool)
	createBucket(t, h, `{"id":"b1","poolId":"bp"}`)
	mustPutObject(t, h, "b1", "k", "payload", map[string]string{"X-Vaultgrid-Meta-A": "1"})

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		rec := rangeRequest(h, method, "/v1/buckets/b1/objects/k", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s no-range status = %d", method, rec.Code)
		}
		if rec.Header().Get("Accept-Ranges") != "bytes" {
			t.Fatalf("%s Accept-Ranges = %q", method, rec.Header().Get("Accept-Ranges"))
		}
		if rec.Header().Get("Content-Length") != "7" {
			t.Fatalf("%s Content-Length = %q", method, rec.Header().Get("Content-Length"))
		}
		if rec.Header().Get("Content-Range") != "" {
			t.Fatalf("%s unexpected Content-Range %q", method, rec.Header().Get("Content-Range"))
		}
		if rec.Header().Get("ETag") != sha256ETag("payload") || rec.Header().Get("X-Vaultgrid-Meta-A") != "1" {
			t.Fatalf("%s standard headers lost: %v", method, rec.Header())
		}
	}
	rec := rangeRequest(h, http.MethodGet, "/v1/buckets/b1/objects/k", "")
	if rec.Body.String() != "payload" {
		t.Fatalf("full body = %q", rec.Body.String())
	}
	head := rangeRequest(h, http.MethodHead, "/v1/buckets/b1/objects/k", "")
	if head.Body.Len() != 0 {
		t.Fatalf("HEAD body = %q", head.Body.String())
	}
}

func TestRangeClosedForm(t *testing.T) {
	h := Handler()
	createPool(t, h, bucketPool)
	createBucket(t, h, `{"id":"b1","poolId":"bp"}`)
	mustPutObject(t, h, "b1", "k", "0123456789", nil) // length 10

	rec := rangeRequest(h, http.MethodGet, "/v1/buckets/b1/objects/k", "bytes=2-4")
	if rec.Code != http.StatusPartialContent || rec.Body.String() != "234" {
		t.Fatalf("closed range = %d %q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Content-Range") != "bytes 2-4/10" {
		t.Fatalf("Content-Range = %q", rec.Header().Get("Content-Range"))
	}
	if rec.Header().Get("Content-Length") != "3" {
		t.Fatalf("Content-Length = %q", rec.Header().Get("Content-Length"))
	}
	if rec.Header().Get("Accept-Ranges") != "bytes" {
		t.Fatalf("Accept-Ranges = %q", rec.Header().Get("Accept-Ranges"))
	}
	if rec.Header().Get("ETag") != sha256ETag("0123456789") {
		t.Fatalf("ETag = %q", rec.Header().Get("ETag"))
	}

	// First and last single bytes, inclusive on both ends.
	for _, c := range []struct{ spec, want string }{
		{"bytes=0-0", "0"},
		{"bytes=9-9", "9"},
		{"bytes=0-9", "0123456789"},
	} {
		rec = rangeRequest(h, http.MethodGet, "/v1/buckets/b1/objects/k", c.spec)
		if rec.Code != http.StatusPartialContent || rec.Body.String() != c.want {
			t.Fatalf("%s = %d %q, want %q", c.spec, rec.Code, rec.Body.String(), c.want)
		}
	}

	// An end past the object length is clipped to the last byte.
	rec = rangeRequest(h, http.MethodGet, "/v1/buckets/b1/objects/k", "bytes=8-100")
	if rec.Code != http.StatusPartialContent || rec.Body.String() != "89" {
		t.Fatalf("clipped end = %d %q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Content-Range") != "bytes 8-9/10" || rec.Header().Get("Content-Length") != "2" {
		t.Fatalf("clipped headers = %v", rec.Header())
	}
}

func TestRangeOpenAndSuffixForms(t *testing.T) {
	h := Handler()
	createPool(t, h, bucketPool)
	createBucket(t, h, `{"id":"b1","poolId":"bp"}`)
	mustPutObject(t, h, "b1", "k", "0123456789", nil)

	// Open-ended "start-".
	rec := rangeRequest(h, http.MethodGet, "/v1/buckets/b1/objects/k", "bytes=7-")
	if rec.Code != http.StatusPartialContent || rec.Body.String() != "789" {
		t.Fatalf("open range = %d %q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Content-Range") != "bytes 7-9/10" || rec.Header().Get("Content-Length") != "3" {
		t.Fatalf("open headers = %v", rec.Header())
	}

	// Suffix "-suffixLength".
	rec = rangeRequest(h, http.MethodGet, "/v1/buckets/b1/objects/k", "bytes=-4")
	if rec.Code != http.StatusPartialContent || rec.Body.String() != "6789" {
		t.Fatalf("suffix range = %d %q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Content-Range") != "bytes 6-9/10" || rec.Header().Get("Content-Length") != "4" {
		t.Fatalf("suffix headers = %v", rec.Header())
	}

	// Suffix of exactly the object length, or longer, returns the whole
	// object with 200.
	for _, spec := range []string{"bytes=-10", "bytes=-11", "bytes=-1000000"} {
		rec = rangeRequest(h, http.MethodGet, "/v1/buckets/b1/objects/k", spec)
		if rec.Code != http.StatusOK || rec.Body.String() != "0123456789" {
			t.Fatalf("%s = %d %q, want full 200", spec, rec.Code, rec.Body.String())
		}
		if rec.Header().Get("Content-Range") != "" || rec.Header().Get("Content-Length") != "10" ||
			rec.Header().Get("Accept-Ranges") != "bytes" {
			t.Fatalf("%s full-read headers = %v", spec, rec.Header())
		}
	}
}

func TestRangeHeadMatchesGet(t *testing.T) {
	h := Handler()
	createPool(t, h, bucketPool)
	createBucket(t, h, `{"id":"b1","poolId":"bp"}`)
	mustPutObject(t, h, "b1", "k", "0123456789", map[string]string{"X-Vaultgrid-Meta-A": "1"})

	get := rangeRequest(h, http.MethodGet, "/v1/buckets/b1/objects/k", "bytes=3-7")
	head := rangeRequest(h, http.MethodHead, "/v1/buckets/b1/objects/k", "bytes=3-7")
	if head.Code != get.Code || head.Code != http.StatusPartialContent {
		t.Fatalf("HEAD status = %d, GET = %d", head.Code, get.Code)
	}
	for _, name := range []string{"Content-Range", "Content-Length", "Accept-Ranges", "ETag", "Content-Type", "X-Vaultgrid-Meta-A"} {
		if head.Header().Get(name) != get.Header().Get(name) {
			t.Fatalf("HEAD %s = %q, GET = %q", name, head.Header().Get(name), get.Header().Get(name))
		}
	}
	if head.Header().Get("Content-Range") != "bytes 3-7/10" || head.Header().Get("Content-Length") != "5" {
		t.Fatalf("HEAD range headers = %v", head.Header())
	}
	if head.Body.Len() != 0 {
		t.Fatalf("HEAD carried a body: %q", head.Body.String())
	}
	if get.Body.String() != "34567" {
		t.Fatalf("GET body = %q", get.Body.String())
	}
}

func TestRangeNotSatisfiable(t *testing.T) {
	h := Handler()
	createPool(t, h, bucketPool)
	createBucket(t, h, `{"id":"b1","poolId":"bp"}`)
	mustPutObject(t, h, "b1", "k", "0123456789", nil)

	// start at/past the length, or start greater than end -> 416.
	for _, spec := range []string{"bytes=10-", "bytes=10-11", "bytes=11-", "bytes=5-2", "bytes=100-200"} {
		rec := rangeRequest(h, http.MethodGet, "/v1/buckets/b1/objects/k", spec)
		wantError(t, rec, http.StatusRequestedRangeNotSatisfiable, "range_not_satisfiable")
		if rec.Header().Get("Content-Range") != "bytes */10" {
			t.Fatalf("%s Content-Range = %q, want bytes */10", spec, rec.Header().Get("Content-Range"))
		}
	}

	// HEAD reports the identical status, error and marker. Error bodies follow
	// the existing HEAD error handling (the JSON error is still produced); the
	// successful-HEAD no-body guarantee is covered elsewhere.
	head := rangeRequest(h, http.MethodHead, "/v1/buckets/b1/objects/k", "bytes=10-")
	if head.Code != http.StatusRequestedRangeNotSatisfiable || head.Header().Get("Content-Range") != "bytes */10" {
		t.Fatalf("HEAD 416 = %d %v", head.Code, head.Header())
	}
	if errObj := decodeBody(t, head)["error"].(map[string]any); errObj["code"] != "range_not_satisfiable" {
		t.Fatalf("HEAD 416 body = %s", head.Body.String())
	}
}

func TestRangeEmptyObject(t *testing.T) {
	h := Handler()
	createPool(t, h, bucketPool)
	createBucket(t, h, `{"id":"b1","poolId":"bp"}`)
	mustPutObject(t, h, "b1", "empty", "", nil)

	// A plain read of the empty object stays 200 with Accept-Ranges.
	rec := rangeRequest(h, http.MethodGet, "/v1/buckets/b1/objects/empty", "")
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Length") != "0" || rec.Header().Get("Accept-Ranges") != "bytes" {
		t.Fatalf("empty full read = %d %v", rec.Code, rec.Header())
	}

	// Every range form, including a suffix, is 416 with bytes */0.
	for _, spec := range []string{"bytes=0-0", "bytes=0-", "bytes=-1", "bytes=-100"} {
		rec = rangeRequest(h, http.MethodGet, "/v1/buckets/b1/objects/empty", spec)
		wantError(t, rec, http.StatusRequestedRangeNotSatisfiable, "range_not_satisfiable")
		if rec.Header().Get("Content-Range") != "bytes */0" {
			t.Fatalf("%s Content-Range = %q, want bytes */0", spec, rec.Header().Get("Content-Range"))
		}
	}
}

func TestRangeMalformed(t *testing.T) {
	h := Handler()
	createPool(t, h, bucketPool)
	createBucket(t, h, `{"id":"b1","poolId":"bp"}`)
	mustPutObject(t, h, "b1", "k", "0123456789", nil)

	bad := []string{
		"bytes=",        // both bounds empty
		"bytes=-",       // both bounds empty
		"bytes=-0",      // zero suffix
		"bytes=0-1,2-3", // multiple ranges
		"bytes=0-1,",    // trailing range
		"items=0-1",     // wrong unit
		"byte=0-1",
		"Bytes=0-1",
		" bytes=0-1", // leading whitespace
		"bytes=0-1 ", // trailing whitespace
		"bytes= 0-1",
		"bytes=0 -1",
		"bytes=a-b",   // non-decimal
		"bytes=0x1-2", // hex
		"bytes=1.0-2",
		"bytes=+0-1", // explicit sign
		"bytes=1-+2",
		"bytes=0--1", // negative-looking bound
		"bytes=--1",
		"bytes=0-1;", // trailing junk
		"bytes==0-1",
		"bytes0-1", // missing '='
	}
	for _, spec := range bad {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			rec := rangeRequest(h, method, "/v1/buckets/b1/objects/k", spec)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("%s %s = %d, want 400", method, spec, rec.Code)
			}
			if errObj := decodeBody(t, rec)["error"].(map[string]any); errObj["code"] != "invalid_request" {
				t.Fatalf("%s %s body = %s", method, spec, rec.Body.String())
			}
			if rec.Header().Get("Content-Range") != "" || rec.Header().Get("Accept-Ranges") != "" {
				t.Fatalf("%s %s leaked range headers: %v", method, spec, rec.Header())
			}
		}
	}

	// Repeated Range header lines are invalid too.
	r := httptest.NewRequest(http.MethodGet, "/v1/buckets/b1/objects/k", nil)
	r.Header["Range"] = []string{"bytes=0-1", "bytes=2-3"}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	wantError(t, rec, http.StatusBadRequest, "invalid_request")
}

func TestRangeValidationOrder(t *testing.T) {
	h := Handler()
	createPool(t, h, bucketPool)
	createBucket(t, h, `{"id":"b1","poolId":"bp"}`)
	enableVersioning(t, h, "b1")
	mustPutObject(t, h, "b1", "k", "abc", nil)
	createBucket(t, h, `{"id":"b2","poolId":"bp"}`)
	mustPutObject(t, h, "b2", "k", "z", nil)

	// Bucket/object/version resolution precedes Range handling: a malformed
	// or valid range never masks an existing 404/400.
	wantError(t, rangeRequest(h, http.MethodGet, "/v1/buckets/nope/objects/k", "bytes=garbage"), http.StatusNotFound, "bucket_not_found")
	wantError(t, rangeRequest(h, http.MethodGet, "/v1/buckets/b1/objects/missing", "bytes=0-1"), http.StatusNotFound, "object_not_found")
	wantError(t, rangeRequest(h, http.MethodGet, "/v1/buckets/b1/objects/k?versionId=nope", "bytes=0-1"), http.StatusNotFound, "object_version_not_found")
	// Delete marker hides the key even with a well-formed range.
	do(h, http.MethodDelete, "/v1/buckets/b1/objects/k", "")
	wantError(t, rangeRequest(h, http.MethodGet, "/v1/buckets/b1/objects/k", "bytes=0-0"), http.StatusNotFound, "object_not_found")
	// A non-versioned bucket still rejects versionId before Range.
	wantError(t, rangeRequest(h, http.MethodGet, "/v1/buckets/b2/objects/k?versionId=x", "bytes=0-0"), http.StatusBadRequest, "invalid_request")
}

func TestRangeOnSelectedVersion(t *testing.T) {
	h := Handler()
	createPool(t, h, bucketPool)
	createBucket(t, h, `{"id":"b1","poolId":"bp"}`)
	enableVersioning(t, h, "b1")
	r1 := mustPutObject(t, h, "b1", "k", "aaa", map[string]string{"X-Vaultgrid-Meta-A": "old"})
	r2 := mustPutObject(t, h, "b1", "k", "bbbbbb", map[string]string{"X-Vaultgrid-Meta-A": "new"})
	id1 := decodeBody(t, r1)["versionId"].(string)
	id2 := decodeBody(t, r2)["versionId"].(string)

	// Ranges are computed against the selected version's full length.
	rec := rangeRequest(h, http.MethodGet, "/v1/buckets/b1/objects/k?versionId="+id1, "bytes=1-")
	if rec.Code != http.StatusPartialContent || rec.Body.String() != "aa" {
		t.Fatalf("old version range = %d %q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Content-Range") != "bytes 1-2/3" || rec.Header().Get(versionHeader) != id1 ||
		rec.Header().Get("ETag") != sha256ETag("aaa") || rec.Header().Get("X-Vaultgrid-Meta-A") != "old" {
		t.Fatalf("old version range headers = %v", rec.Header())
	}

	rec = rangeRequest(h, http.MethodGet, "/v1/buckets/b1/objects/k?versionId="+id2, "bytes=-2")
	if rec.Code != http.StatusPartialContent || rec.Body.String() != "bb" {
		t.Fatalf("new version suffix = %d %q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Content-Range") != "bytes 4-5/6" || rec.Header().Get(versionHeader) != id2 {
		t.Fatalf("new version headers = %v", rec.Header())
	}

	// Default read ranges the visible version.
	rec = rangeRequest(h, http.MethodGet, "/v1/buckets/b1/objects/k", "bytes=0-0")
	if rec.Code != http.StatusPartialContent || rec.Body.String() != "b" || rec.Header().Get(versionHeader) != id2 {
		t.Fatalf("visible range = %d %q %v", rec.Code, rec.Body.String(), rec.Header())
	}

	// 416 on a short old version reports that version's length and id.
	rec = rangeRequest(h, http.MethodGet, "/v1/buckets/b1/objects/k?versionId="+id1, "bytes=3-")
	wantError(t, rec, http.StatusRequestedRangeNotSatisfiable, "range_not_satisfiable")
	if rec.Header().Get("Content-Range") != "bytes */3" || rec.Header().Get(versionHeader) != id1 {
		t.Fatalf("old version 416 headers = %v", rec.Header())
	}
}

func TestRangeReadsHaveNoSideEffects(t *testing.T) {
	h := Handler()
	createPool(t, h, bucketPool)
	createBucket(t, h, `{"id":"b1","poolId":"bp"}`)
	mustPutObject(t, h, "b1", "k", "0123456789", nil)

	before := len(getAuditEvents(t, h, "/v1/audit-events?limit=1000")["items"].([]any))
	for _, spec := range []string{"", "bytes=0-3", "bytes=-2", "bytes=9-9", "bytes=100-", "bytes=bad"} {
		rangeRequest(h, http.MethodGet, "/v1/buckets/b1/objects/k", spec)
		rangeRequest(h, http.MethodHead, "/v1/buckets/b1/objects/k", spec)
	}
	after := len(getAuditEvents(t, h, "/v1/audit-events?limit=1000")["items"].([]any))
	if after != before {
		t.Fatalf("audit events grew %d -> %d", before, after)
	}
	// Object, bucket and pool state is untouched.
	if rec := do(h, http.MethodGet, "/v1/buckets/b1/objects/k", ""); rec.Body.String() != "0123456789" {
		t.Fatalf("object changed: %q", rec.Body.String())
	}
	b := decodeBody(t, do(h, http.MethodGet, "/v1/buckets/b1", ""))
	if b["objectCount"].(float64) != 1 || b["bytesUsed"].(float64) != 10 {
		t.Fatalf("bucket counters changed: %v", b)
	}
}

func TestRangeConcurrentOverwrite(t *testing.T) {
	h := Handler()
	createPool(t, h, `{"id":"cp","devices":[{"id":"d","capacityBytes":1000000,"faultDomain":"fd"}]}`)
	createBucket(t, h, `{"id":"b1","poolId":"cp"}`)
	mustPutObject(t, h, "b1", "k", strings.Repeat("0", 21), nil)

	// Overwriters each install a distinct, fixed-length version
	// "<two-digit i>" + 19 'x' bytes, so a reader can verify that the body,
	// length and Content-Range denominator all come from the same version.
	versions := make([]string, 20)
	etags := make(map[string]string, 21)
	for i := 0; i < 20; i++ {
		full := fmt.Sprintf("%02d", i) + strings.Repeat("x", 19)
		versions[i] = full
		etags[full] = sha256ETag(full)
	}
	// Readers may also observe the pre-test version of the same length.
	initial := strings.Repeat("0", 21)
	etags[initial] = sha256ETag(initial)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			putObject(t, h, "b1", "k", versions[i], nil)
		}(i)
		go func() {
			defer wg.Done()
			rec := rangeRequest(h, http.MethodGet, "/v1/buckets/b1/objects/k", "bytes=0-4")
			if rec.Code != http.StatusPartialContent {
				t.Errorf("range status = %d", rec.Code)
				return
			}
			if rec.Body.Len() != 5 || rec.Header().Get("Content-Length") != "5" {
				t.Errorf("inconsistent range length: cl=%s body=%d", rec.Header().Get("Content-Length"), rec.Body.Len())
			}
			if cr := rec.Header().Get("Content-Range"); cr != "bytes 0-4/21" {
				t.Errorf("Content-Range = %q, want bytes 0-4/21", cr)
			}
			// The returned prefix must be the prefix of the exact version
			// named by the etag; a torn read would fail this.
			full, ok := etagToVersion(etags, rec.Header().Get("ETag"))
			if !ok {
				t.Errorf("ETag %q matches no installed version", rec.Header().Get("ETag"))
				return
			}
			if rec.Body.String() != full[:5] {
				t.Errorf("body %q is not the prefix of its etag version %q", rec.Body.String(), full)
			}
		}()
	}
	wg.Wait()
}

func etagToVersion(etags map[string]string, etag string) (string, bool) {
	for v, e := range etags {
		if e == etag {
			return v, true
		}
	}
	return "", false
}

func TestRangeLargeDecimalValues(t *testing.T) {
	h := Handler()
	createPool(t, h, bucketPool)
	createBucket(t, h, `{"id":"b1","poolId":"bp"}`)
	mustPutObject(t, h, "b1", "k", "0123456789", nil)

	// An overflowing but decimal end clips to the object; an overflowing
	// open start is 416; an overflowing suffix returns the whole object.
	rec := rangeRequest(h, http.MethodGet, "/v1/buckets/b1/objects/k", "bytes=0-999999999999999999999999")
	if rec.Code != http.StatusPartialContent || rec.Body.String() != "0123456789" || rec.Header().Get("Content-Range") != "bytes 0-9/10" {
		t.Fatalf("overflow end = %d %q %v", rec.Code, rec.Body.String(), rec.Header())
	}
	rec = rangeRequest(h, http.MethodGet, "/v1/buckets/b1/objects/k", "bytes=999999999999999999999999-")
	wantError(t, rec, http.StatusRequestedRangeNotSatisfiable, "range_not_satisfiable")
	if rec.Header().Get("Content-Range") != "bytes */10" {
		t.Fatalf("overflow start marker = %q", rec.Header().Get("Content-Range"))
	}
	rec = rangeRequest(h, http.MethodGet, "/v1/buckets/b1/objects/k", "bytes=-999999999999999999999999")
	if rec.Code != http.StatusOK || rec.Body.String() != "0123456789" {
		t.Fatalf("overflow suffix = %d %q", rec.Code, rec.Body.String())
	}
}

func TestRangeExactBoundaryPositions(t *testing.T) {
	h := Handler()
	createPool(t, h, bucketPool)
	createBucket(t, h, `{"id":"b1","poolId":"bp"}`)
	mustPutObject(t, h, "b1", "k", "abcdefghij", nil) // length 10

	cases := []struct {
		spec   string
		status int
		body   string
		cr     string
		cl     string
	}{
		{"bytes=9-20", http.StatusPartialContent, "j", "bytes 9-9/10", "1"},
		{"bytes=9-", http.StatusPartialContent, "j", "bytes 9-9/10", "1"},
		{"bytes=-1", http.StatusPartialContent, "j", "bytes 9-9/10", "1"},
		{"bytes=-9", http.StatusPartialContent, "bcdefghij", "bytes 1-9/10", "9"},
		{"bytes=5-5", http.StatusPartialContent, "f", "bytes 5-5/10", "1"},
	}
	for i, c := range cases {
		rec := rangeRequest(h, http.MethodGet, "/v1/buckets/b1/objects/k", c.spec)
		if rec.Code != c.status || rec.Body.String() != c.body {
			t.Fatalf("case %d %s = %d %q", i, c.spec, rec.Code, rec.Body.String())
		}
		if rec.Header().Get("Content-Range") != c.cr || rec.Header().Get("Content-Length") != c.cl {
			t.Fatalf("case %d headers = %v", i, rec.Header())
		}
		if strconv.Itoa(rec.Body.Len()) != c.cl {
			t.Fatalf("case %d body length %d != Content-Length %s", i, rec.Body.Len(), c.cl)
		}
	}
}

package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// rangedGet issues a GET or HEAD with a single Range header value.
func rangedGet(h http.Handler, method, target, rangeHeader string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, nil)
	if rangeHeader != "" {
		r.Header.Set("Range", rangeHeader)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func setupRangedObject(t *testing.T) (http.Handler, string) {
	t.Helper()
	h := Handler()
	createPool(t, h, bucketPool)
	createBucket(t, h, `{"id":"b1","poolId":"bp"}`)
	// 10 bytes, indexes 0..9.
	mustPutObject(t, h, "b1", "k", "0123456789", map[string]string{"X-Vaultgrid-Meta-A": "1"})
	return h, sha256ETag("0123456789")
}

func TestRangeGetForms(t *testing.T) {
	h, etag := setupRangedObject(t)

	cases := []struct {
		name       string
		header     string
		wantBody   string
		wantRange  string
		wantLength string
	}{
		{"closed", "bytes=2-5", "2345", "bytes 2-5/10", "4"},
		{"from start", "bytes=0-0", "0", "bytes 0-0/10", "1"},
		{"open ended", "bytes=7-", "789", "bytes 7-9/10", "3"},
		{"end clamps to size", "bytes=8-100", "89", "bytes 8-9/10", "2"},
		{"start at last byte", "bytes=9-999", "9", "bytes 9-9/10", "1"},
		{"suffix", "bytes=-4", "6789", "bytes 6-9/10", "4"},
		{"suffix one", "bytes=-1", "9", "bytes 9-9/10", "1"},
		{"suffix overlong returns whole object", "bytes=-100", "0123456789", "bytes 0-9/10", "10"},
		{"suffix equal to length", "bytes=-10", "0123456789", "bytes 0-9/10", "10"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := rangedGet(h, http.MethodGet, "/v1/buckets/b1/objects/k", tc.header)
			if rec.Code != http.StatusPartialContent {
				t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
			}
			if rec.Body.String() != tc.wantBody {
				t.Fatalf("body = %q, want %q", rec.Body.String(), tc.wantBody)
			}
			if got := rec.Header().Get("Content-Range"); got != tc.wantRange {
				t.Fatalf("Content-Range = %q, want %q", got, tc.wantRange)
			}
			if got := rec.Header().Get("Content-Length"); got != tc.wantLength {
				t.Fatalf("Content-Length = %q, want %q", got, tc.wantLength)
			}
			if got := rec.Header().Get("Accept-Ranges"); got != "bytes" {
				t.Fatalf("Accept-Ranges = %q, want bytes", got)
			}
			if got := rec.Header().Get("ETag"); got != etag {
				t.Fatalf("ETag = %q, want %q", got, etag)
			}
			if got := rec.Header().Get("Content-Type"); got != "application/octet-stream" {
				t.Fatalf("Content-Type = %q", got)
			}
			if got := rec.Header().Get("X-Vaultgrid-Meta-A"); got != "1" {
				t.Fatalf("meta header = %q", got)
			}
		})
	}
}

func TestRangeHeadMatchesGetWithoutBody(t *testing.T) {
	h, _ := setupRangedObject(t)

	for _, header := range []string{"bytes=2-5", "bytes=7-", "bytes=-4", "bytes=8-100", "bytes=-100"} {
		get := rangedGet(h, http.MethodGet, "/v1/buckets/b1/objects/k", header)
		head := rangedGet(h, http.MethodHead, "/v1/buckets/b1/objects/k", header)
		if head.Code != get.Code {
			t.Fatalf("header %q: head status %d != get %d", header, head.Code, get.Code)
		}
		if head.Body.Len() != 0 {
			t.Fatalf("header %q: head carried body %q", header, head.Body.String())
		}
		for _, name := range []string{"Content-Range", "Content-Length", "Accept-Ranges", "ETag", "Content-Type", "X-Vaultgrid-Meta-A"} {
			if head.Header().Get(name) != get.Header().Get(name) {
				t.Fatalf("header %q: head %s = %q, get = %q", header, name, head.Header().Get(name), get.Header().Get(name))
			}
		}
	}
}

func TestFullReadAdvertisesRanges(t *testing.T) {
	h, etag := setupRangedObject(t)

	rec := rangedGet(h, http.MethodGet, "/v1/buckets/b1/objects/k", "")
	if rec.Code != http.StatusOK || rec.Body.String() != "0123456789" {
		t.Fatalf("full get = %d %q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Accept-Ranges") != "bytes" {
		t.Fatalf("Accept-Ranges = %q", rec.Header().Get("Accept-Ranges"))
	}
	if rec.Header().Get("Content-Length") != "10" || rec.Header().Get("ETag") != etag {
		t.Fatalf("full read headers = %v", rec.Header())
	}
	if rec.Header().Get("Content-Range") != "" {
		t.Fatalf("full read must not carry Content-Range: %q", rec.Header().Get("Content-Range"))
	}

	head := rangedGet(h, http.MethodHead, "/v1/buckets/b1/objects/k", "")
	if head.Code != http.StatusOK || head.Body.Len() != 0 {
		t.Fatalf("full head = %d body=%q", head.Code, head.Body.String())
	}
	if head.Header().Get("Accept-Ranges") != "bytes" || head.Header().Get("Content-Length") != "10" {
		t.Fatalf("full head headers = %v", head.Header())
	}
}

func TestRangeNotSatisfiable(t *testing.T) {
	h, _ := setupRangedObject(t)

	for _, header := range []string{"bytes=10-", "bytes=10-11", "bytes=999-", "bytes=5-2"} {
		rec := rangedGet(h, http.MethodGet, "/v1/buckets/b1/objects/k", header)
		wantError(t, rec, http.StatusRequestedRangeNotSatisfiable, "range_not_satisfiable")
		if got := rec.Header().Get("Content-Range"); got != "bytes */10" {
			t.Fatalf("header %q: Content-Range = %q, want bytes */10", header, got)
		}
		head := rangedGet(h, http.MethodHead, "/v1/buckets/b1/objects/k", header)
		if head.Code != http.StatusRequestedRangeNotSatisfiable {
			t.Fatalf("header %q: head status = %d", header, head.Code)
		}
		if got := head.Header().Get("Content-Range"); got != "bytes */10" {
			t.Fatalf("header %q: head Content-Range = %q", header, got)
		}
	}
}

func TestRangeOnEmptyObject(t *testing.T) {
	h := Handler()
	createPool(t, h, bucketPool)
	createBucket(t, h, `{"id":"b1","poolId":"bp"}`)
	mustPutObject(t, h, "b1", "empty", "", nil)

	// No range: full 200 read of an empty object still works.
	rec := rangedGet(h, http.MethodGet, "/v1/buckets/b1/objects/empty", "")
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Fatalf("full read of empty = %d %q", rec.Code, rec.Body.String())
	}
	for _, header := range []string{"bytes=0-", "bytes=0-0", "bytes=-1"} {
		rec := rangedGet(h, http.MethodGet, "/v1/buckets/b1/objects/empty", header)
		wantError(t, rec, http.StatusRequestedRangeNotSatisfiable, "range_not_satisfiable")
		if got := rec.Header().Get("Content-Range"); got != "bytes */0" {
			t.Fatalf("header %q: Content-Range = %q, want bytes */0", header, got)
		}
	}
}

func TestMalformedRangeIsBadRequest(t *testing.T) {
	h, _ := setupRangedObject(t)

	bad := []string{
		"bytes=0-1,2-3", // multiple ranges
		"bytes=-",       // both ends empty
		"bytes=",        // empty spec
		"items=0-1",     // wrong unit
		"Bytes=0-1",     // unit must be exactly bytes
		"bytes =0-1",    // unit must be exactly bytes
		"bytes=0--1",    // negative end
		"bytes=-1-2",    // negative/suffix with extra end
		"bytes=0-0xff",  // non-decimal
		"bytes=abc-def", // non-decimal
		"bytes=-0",      // zero suffix length
		"bytes= 0-1",    // leading whitespace
		"bytes=0-1 ",    // trailing whitespace
		"bytes=0 -1",    // inner whitespace
		"bytes=0-1,",    // dangling second range
		"bytes=0-1-2",   // extra shape
	}
	for _, header := range bad {
		rec := rangedGet(h, http.MethodGet, "/v1/buckets/b1/objects/k", header)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("header %q: status = %d, want 400 (body=%s)", header, rec.Code, rec.Body.String())
			continue
		}
		if code := decodeBody(t, rec)["error"].(map[string]any)["code"]; code != "invalid_request" {
			t.Errorf("header %q: code = %v", header, code)
		}
		if rec.Body.String() == "0123456789" {
			t.Errorf("header %q: malformed range degraded to full read", header)
		}
		head := rangedGet(h, http.MethodHead, "/v1/buckets/b1/objects/k", header)
		if head.Code != http.StatusBadRequest {
			t.Errorf("header %q: head status = %d, want 400", header, head.Code)
		}
	}

	// Overlong start digits parse as a saturated number: malformed shape it is
	// not, but it can never be satisfiable for a 10-byte object -> 416.
	rec := rangedGet(h, http.MethodGet, "/v1/buckets/b1/objects/k", "bytes=99999999999999999999999999-")
	wantError(t, rec, http.StatusRequestedRangeNotSatisfiable, "range_not_satisfiable")
}

func TestMalformedRangeMultipleHeaders(t *testing.T) {
	h, _ := setupRangedObject(t)
	r := httptest.NewRequest(http.MethodGet, "/v1/buckets/b1/objects/k", nil)
	r.Header["Range"] = []string{"bytes=0-1", "bytes=2-3"}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	wantError(t, rec, http.StatusBadRequest, "invalid_request")
}

func TestRangeLookupErrorsStill404(t *testing.T) {
	h, _ := setupRangedObject(t)
	header := "bytes=0-1"

	// Missing bucket, object and version win over Range processing.
	wantError(t, rangedGet(h, http.MethodGet, "/v1/buckets/nope/objects/k", header), http.StatusNotFound, "bucket_not_found")
	wantError(t, rangedGet(h, http.MethodGet, "/v1/buckets/b1/objects/nope", header), http.StatusNotFound, "object_not_found")

	// A syntactically bad query is still a 400 even with a valid range.
	wantError(t, rangedGet(h, http.MethodGet, "/v1/buckets/b1/objects/k?bogus=1", header), http.StatusBadRequest, "invalid_request")

	// In a versioned bucket an unknown version is selected before Range is
	// examined, so even a malformed Range header loses to the 404.
	createBucket(t, h, `{"id":"b2","poolId":"bp"}`)
	if rec := do(h, http.MethodPut, "/v1/buckets/b2/versioning", `{"enabled":true}`); rec.Code != http.StatusOK {
		t.Fatalf("enable versioning = %d", rec.Code)
	}
	mustPutObject(t, h, "b2", "k", "data", nil)
	wantError(t, rangedGet(h, http.MethodGet, "/v1/buckets/b2/objects/k?versionId=deadbeef", "bytes=0-1"),
		http.StatusNotFound, "object_version_not_found")
	wantError(t, rangedGet(h, http.MethodGet, "/v1/buckets/b2/objects/k?versionId=deadbeef", "bytes=nope"),
		http.StatusNotFound, "object_version_not_found")
}

func TestRangeOnSelectedVersion(t *testing.T) {
	h := Handler()
	createPool(t, h, bucketPool)
	createBucket(t, h, `{"id":"b1","poolId":"bp"}`)
	if rec := do(h, http.MethodPut, "/v1/buckets/b1/versioning", `{"enabled":true}`); rec.Code != http.StatusOK {
		t.Fatalf("enable versioning = %d %s", rec.Code, rec.Body.String())
	}
	first := mustPutObject(t, h, "b1", "k", "aaa", nil)
	firstID := decodeBody(t, first)["versionId"].(string)
	second := mustPutObject(t, h, "b1", "k", "bbbbbb", nil)
	secondID := decodeBody(t, second)["versionId"].(string)

	// Latest version: range computed over 6 bytes.
	rec := rangedGet(h, http.MethodGet, "/v1/buckets/b1/objects/k", "bytes=2-")
	if rec.Code != http.StatusPartialContent || rec.Body.String() != "bbbb" {
		t.Fatalf("latest range = %d %q", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Range"); got != "bytes 2-5/6" {
		t.Fatalf("latest Content-Range = %q", got)
	}
	if got := rec.Header().Get(versionHeader); got != secondID {
		t.Fatalf("version header = %q, want %q", got, secondID)
	}

	// Older version: range computed over its own 3 bytes and carries its id.
	rec = rangedGet(h, http.MethodGet, "/v1/buckets/b1/objects/k?versionId="+firstID, "bytes=-2")
	if rec.Code != http.StatusPartialContent || rec.Body.String() != "aa" {
		t.Fatalf("old version range = %d %q", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Range"); got != "bytes 1-2/3" {
		t.Fatalf("old Content-Range = %q", got)
	}
	if got := rec.Header().Get(versionHeader); got != firstID {
		t.Fatalf("version header = %q, want %q", got, firstID)
	}
}

func TestRangeReadsDoNotChangeState(t *testing.T) {
	h, _ := setupRangedObject(t)

	before := do(h, http.MethodGet, "/v1/storage-pools/bp", "")
	auditBefore := do(h, http.MethodGet, "/v1/audit-events", "")

	for _, header := range []string{"bytes=0-1", "bytes=-3", "bytes=9-", "bytes=10-", "bytes=bad"} {
		rangedGet(h, http.MethodGet, "/v1/buckets/b1/objects/k", header)
		rangedGet(h, http.MethodHead, "/v1/buckets/b1/objects/k", header)
	}

	after := do(h, http.MethodGet, "/v1/storage-pools/bp", "")
	if after.Body.String() != before.Body.String() {
		t.Fatalf("pool view changed after range reads:\nbefore %s\nafter  %s", before.Body.String(), after.Body.String())
	}
	auditAfter := do(h, http.MethodGet, "/v1/audit-events", "")
	if auditAfter.Body.String() != auditBefore.Body.String() {
		t.Fatalf("audit log changed after range reads:\nbefore %s\nafter  %s", auditBefore.Body.String(), auditAfter.Body.String())
	}
}

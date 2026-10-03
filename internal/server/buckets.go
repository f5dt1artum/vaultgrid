package server

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// metaPrefix is the header prefix carrying user metadata on object writes.
// The constant is already in Go's canonical header form.
const metaPrefix = "X-Vaultgrid-Meta-"

type createBucketInput struct {
	ID     string `json:"id"`
	PoolID string `json:"poolId"`
}

// bucketView is the public representation of a bucket.
type bucketView struct {
	ID          string `json:"id"`
	PoolID      string `json:"poolId"`
	ObjectCount int    `json:"objectCount"`
	BytesUsed   int64  `json:"bytesUsed"`
}

// objectView is the public representation of an object after a write, and
// the summary shape used in object listings.
type objectView struct {
	Key       string            `json:"key"`
	SizeBytes int64             `json:"sizeBytes"`
	Metadata  map[string]string `json:"metadata"`
	ETag      string            `json:"etag"`
}

// bucket is a namespace of objects bound to one pool. bytesUsed is kept in
// sync with the sum of its objects' sizes; the same total is charged to the
// pool as objectBytes. tenant owns the bucket; object bytes are charged to
// the tenant's usage in the pool.
type bucket struct {
	id      string
	poolID  string
	tenant  string
	objects map[string]*object
}

func (b *bucket) bytesUsed() int64 {
	var total int64
	for _, o := range b.objects {
		total += o.sizeBytes
	}
	return total
}

func (b *bucket) view() bucketView {
	return bucketView{ID: b.id, PoolID: b.poolID, ObjectCount: len(b.objects), BytesUsed: b.bytesUsed()}
}

// object is a stored byte sequence with user metadata. etag is the quoted
// lowercase hex SHA-256 of content.
type object struct {
	key       string
	content   []byte
	metadata  map[string]string
	etag      string
	sizeBytes int64
}

func (o *object) view() objectView {
	return objectView{Key: o.key, SizeBytes: o.sizeBytes, Metadata: o.metadata, ETag: o.etag}
}

// routeBuckets dispatches /v1/buckets and its sub-paths. The object key is
// the remainder of the path after "objects/" (already URL-decoded once by
// the net/http layer) and may contain slashes, so it is not split here.
func (s *store) routeBuckets(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, bucketPath)
	if rest == "" {
		switch r.Method {
		case http.MethodGet:
			s.listBuckets(w)
		case http.MethodPost:
			s.createBucket(w, r)
		default:
			methodNotAllowed(w, http.MethodGet, http.MethodPost)
		}
		return
	}
	// rest begins with "/"; the bucket id is the single next segment.
	bucketID, tail, found := strings.Cut(rest[1:], "/")
	if bucketID == "" {
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	if !found {
		switch r.Method {
		case http.MethodGet:
			s.getBucket(w, bucketID)
		case http.MethodDelete:
			s.deleteBucket(w, bucketID)
		default:
			methodNotAllowed(w, http.MethodGet, http.MethodDelete)
		}
		return
	}
	if tail == "objects" {
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		s.listObjects(w, r, bucketID)
		return
	}
	if key, ok := strings.CutPrefix(tail, "objects/"); ok {
		switch r.Method {
		case http.MethodPut:
			s.putObject(w, r, bucketID, key)
		case http.MethodGet:
			s.getObject(w, r, bucketID, key, false)
		case http.MethodHead:
			s.getObject(w, r, bucketID, key, true)
		case http.MethodDelete:
			s.deleteObject(w, r, bucketID, key)
		default:
			methodNotAllowed(w, http.MethodPut, http.MethodGet, http.MethodHead, http.MethodDelete)
		}
		return
	}
	writeError(w, http.StatusNotFound, "not_found")
}

func (s *store) listBuckets(w http.ResponseWriter) {
	s.mu.RLock()
	ids := make([]string, 0, len(s.buckets))
	for id := range s.buckets {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	items := make([]bucketView, 0, len(ids))
	for _, id := range ids {
		items = append(items, s.buckets[id].view())
	}
	s.mu.RUnlock()
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *store) getBucket(w http.ResponseWriter, id string) {
	if !validID(id) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	s.mu.RLock()
	b, ok := s.buckets[id]
	var v bucketView
	if ok {
		v = b.view()
	}
	s.mu.RUnlock()
	if !ok {
		writeError(w, http.StatusNotFound, "bucket_not_found")
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *store) createBucket(w http.ResponseWriter, r *http.Request) {
	var in createBucketInput
	if !decodeRequest(r, &in) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	tenant, tok := extractTenant(r.Header)
	if !tok || !validID(in.ID) || !validID(in.PoolID) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, dup := s.buckets[in.ID]; dup {
		if existing.poolID == in.PoolID && existing.tenant == tenant {
			writeJSON(w, http.StatusOK, existing.view())
			return
		}
		writeError(w, http.StatusConflict, "bucket_exists")
		return
	}
	if _, ok := s.pools[in.PoolID]; !ok {
		writeError(w, http.StatusNotFound, "pool_not_found")
		return
	}
	s.buckets[in.ID] = &bucket{id: in.ID, poolID: in.PoolID, tenant: tenant, objects: make(map[string]*object)}
	s.appendAudit(auditBucketCreated, bucketPath+"/"+in.ID, in.PoolID, 0)
	writeJSON(w, http.StatusCreated, s.buckets[in.ID].view())
}

func (s *store) deleteBucket(w http.ResponseWriter, id string) {
	if !validID(id) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.buckets[id]
	if !ok {
		writeError(w, http.StatusNotFound, "bucket_not_found")
		return
	}
	if len(b.objects) > 0 {
		writeError(w, http.StatusConflict, "bucket_not_empty")
		return
	}
	delete(s.buckets, id)
	s.appendAudit(auditBucketDeleted, bucketPath+"/"+id, b.poolID, 0)
	w.WriteHeader(http.StatusNoContent)
}

// validObjectKey reports whether key is 1..1024 bytes of valid UTF-8 with no
// control characters.
func validObjectKey(key string) bool {
	if len(key) == 0 || len(key) > 1024 || !utf8.ValidString(key) {
		return false
	}
	for _, r := range key {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// extractMetadata collects the X-Vaultgrid-Meta-* request headers into a
// metadata map keyed by the suffix after the prefix. A header with an empty
// suffix or a value that is not valid UTF-8 or carries control characters
// makes the whole request invalid.
func extractMetadata(h http.Header) (map[string]string, bool) {
	meta := make(map[string]string)
	for name, values := range h {
		suffix, ok := strings.CutPrefix(name, metaPrefix)
		if !ok {
			continue
		}
		if suffix == "" {
			return nil, false
		}
		for _, v := range values {
			if !utf8.ValidString(v) {
				return nil, false
			}
			for _, r := range v {
				if unicode.IsControl(r) {
					return nil, false
				}
			}
		}
		meta[suffix] = strings.Join(values, ", ")
	}
	return meta, true
}

// parsePrecondition validates the conditional-write headers. It returns the
// If-Match etag ("" when absent) and whether If-None-Match: * was given. ok
// is false when both headers are present or either has an unsupported form.
func parsePrecondition(h http.Header) (ifMatch string, ifNoneMatchStar, ok bool) {
	matches, hasMatch := h["If-Match"]
	noneMatches, hasNoneMatch := h["If-None-Match"]
	if hasMatch && hasNoneMatch {
		return "", false, false
	}
	if hasMatch {
		if len(matches) != 1 || !isQuotedETag(matches[0]) {
			return "", false, false
		}
		return matches[0], false, true
	}
	if hasNoneMatch {
		if len(noneMatches) != 1 || noneMatches[0] != "*" {
			return "", false, false
		}
		return "", true, true
	}
	return "", false, true
}

// isQuotedETag reports whether v looks like a quoted hex etag, e.g.
// "9f86d081...". The content is not checked against any object here.
func isQuotedETag(v string) bool {
	if len(v) < 2 || v[0] != '"' || v[len(v)-1] != '"' {
		return false
	}
	inner := v[1 : len(v)-1]
	if inner == "" {
		return false
	}
	for _, c := range inner {
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}

func etagOf(content []byte) string {
	sum := sha256.Sum256(content)
	return `"` + hex.EncodeToString(sum[:]) + `"`
}

func (s *store) putObject(w http.ResponseWriter, r *http.Request, bucketID, key string) {
	if !validID(bucketID) || !validObjectKey(key) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if r.URL.RawQuery != "" {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	metadata, ok := extractMetadata(r.Header)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	ifMatch, ifNoneMatchStar, ok := parsePrecondition(r.Header)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	content, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	etag := etagOf(content)
	size := int64(len(content))

	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.buckets[bucketID]
	if !ok {
		writeError(w, http.StatusNotFound, "bucket_not_found")
		return
	}
	existing := b.objects[key]
	if ifMatch != "" && (existing == nil || existing.etag != ifMatch) {
		writeError(w, http.StatusPreconditionFailed, "precondition_failed")
		return
	}
	if ifNoneMatchStar && existing != nil {
		writeError(w, http.StatusPreconditionFailed, "precondition_failed")
		return
	}
	var oldSize int64
	if existing != nil {
		oldSize = existing.sizeBytes
	}
	p := s.pools[b.poolID]
	// The store lock makes the quota and capacity checks plus the charge
	// atomic, so concurrent writes can never exceed either limit, and a failed
	// write leaves the old object and its metadata untouched. Objects inherit
	// the bucket's tenant; only a growing write can breach the quota.
	if delta := size - oldSize; delta > 0 && p.tenantQuotaExceeded(b.tenant, delta) {
		writeError(w, http.StatusConflict, "tenant_quota_exceeded")
		return
	}
	if p.allocated() > p.rawCapacity-(size-oldSize) {
		writeError(w, http.StatusConflict, "insufficient_capacity")
		return
	}
	b.objects[key] = &object{key: key, content: content, metadata: metadata, etag: etag, sizeBytes: size}
	p.objectBytes += size - oldSize
	p.tenantUsed[b.tenant] += size - oldSize
	// An overwrite is recorded even when the size delta is zero.
	resource := bucketPath + "/" + bucketID + "/objects/" + key
	if existing != nil {
		s.appendAudit(auditObjectOverwritten, resource, b.poolID, size-oldSize)
	} else {
		s.appendAudit(auditObjectCreated, resource, b.poolID, size)
	}
	status := http.StatusCreated
	if existing != nil {
		status = http.StatusOK
	}
	w.Header().Set("ETag", etag)
	writeJSON(w, status, b.objects[key].view())
}

// getObject serves both GET and HEAD; headOnly suppresses the body.
func (s *store) getObject(w http.ResponseWriter, r *http.Request, bucketID, key string, headOnly bool) {
	if !validID(bucketID) || !validObjectKey(key) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if r.URL.RawQuery != "" {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	s.mu.RLock()
	b, ok := s.buckets[bucketID]
	var o *object
	if ok {
		o = b.objects[key]
	}
	s.mu.RUnlock()
	if !ok {
		writeError(w, http.StatusNotFound, "bucket_not_found")
		return
	}
	if o == nil {
		writeError(w, http.StatusNotFound, "object_not_found")
		return
	}
	for name, value := range o.metadata {
		w.Header().Set(metaPrefix+name, value)
	}
	w.Header().Set("ETag", o.etag)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(o.sizeBytes, 10))
	w.WriteHeader(http.StatusOK)
	if !headOnly {
		_, _ = w.Write(o.content)
	}
}

func (s *store) deleteObject(w http.ResponseWriter, r *http.Request, bucketID, key string) {
	if !validID(bucketID) || !validObjectKey(key) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if r.URL.RawQuery != "" {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.buckets[bucketID]
	if !ok {
		writeError(w, http.StatusNotFound, "bucket_not_found")
		return
	}
	// Deleting an unknown or already-deleted object is a success with no
	// state change, so only a real deletion records an event.
	if o, exists := b.objects[key]; exists {
		s.pools[b.poolID].objectBytes -= o.sizeBytes
		s.pools[b.poolID].tenantUsed[b.tenant] -= o.sizeBytes
		delete(b.objects, key)
		s.appendAudit(auditObjectDeleted, bucketPath+"/"+bucketID+"/objects/"+key, b.poolID, -o.sizeBytes)
	}
	w.WriteHeader(http.StatusNoContent)
}

// parsePrefixQuery accepts an empty query or exactly one "prefix=<value>"
// pair. Duplicated or unknown parameters yield ok=false.
func parsePrefixQuery(raw string) (string, bool) {
	if raw == "" {
		return "", true
	}
	var prefix string
	found := false
	for _, pair := range strings.Split(raw, "&") {
		k, value, ok := strings.Cut(pair, "=")
		if !ok {
			return "", false
		}
		decodedKey, err := url.QueryUnescape(k)
		if err != nil || decodedKey != "prefix" || found {
			return "", false
		}
		decodedValue, err := url.QueryUnescape(value)
		if err != nil {
			return "", false
		}
		prefix, found = decodedValue, true
	}
	return prefix, found
}

func (s *store) listObjects(w http.ResponseWriter, r *http.Request, bucketID string) {
	prefix, ok := parsePrefixQuery(r.URL.RawQuery)
	if !ok || !validID(bucketID) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	s.mu.RLock()
	b, ok := s.buckets[bucketID]
	var items []objectView
	if ok {
		keys := make([]string, 0, len(b.objects))
		for k := range b.objects {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		items = make([]objectView, 0, len(keys))
		for _, k := range keys {
			if strings.HasPrefix(k, prefix) {
				items = append(items, b.objects[k].view())
			}
		}
	}
	s.mu.RUnlock()
	if !ok {
		writeError(w, http.StatusNotFound, "bucket_not_found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

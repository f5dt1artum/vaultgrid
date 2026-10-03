package server

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

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

// objectView is the public summary of an object, returned by writes and
// listings. ETag carries the quoted lowercase SHA-256 hex of the content.
type objectView struct {
	Key       string            `json:"key"`
	SizeBytes int64             `json:"sizeBytes"`
	ETag      string            `json:"etag"`
	Metadata  map[string]string `json:"metadata"`
}

// object is a stored byte blob. Objects are never mutated in place: an
// overwrite swaps in a fresh value, so readers holding the store lock (or a
// pointer fetched under it) always see a consistent whole.
type object struct {
	key      string
	content  []byte
	metadata map[string]string
	etag     string
}

// bucket is a flat namespace of objects whose bytes charge the owning pool.
type bucket struct {
	id        string
	poolID    string
	objects   map[string]*object
	bytesUsed int64
}

func (b *bucket) view() bucketView {
	return bucketView{
		ID:          b.id,
		PoolID:      b.poolID,
		ObjectCount: len(b.objects),
		BytesUsed:   b.bytesUsed,
	}
}

func (o *object) view() objectView {
	return objectView{
		Key:       o.key,
		SizeBytes: int64(len(o.content)),
		ETag:      o.etag,
		Metadata:  o.metadata,
	}
}

// metaHeaderPrefix marks request/response headers that carry object metadata.
// Header names are canonical on arrival, so a plain prefix match suffices.
const metaHeaderPrefix = "X-Vaultgrid-Meta-"

// etagPattern is the single strong entity-tag form accepted in If-Match.
var etagPattern = regexp.MustCompile(`^"[0-9a-f]{64}"$`)

// routeBuckets dispatches /v1/buckets and its sub-paths. Object keys may
// contain slashes and percent-escapes, so everything below the collection is
// parsed from the escaped path and decoded exactly once per segment.
func (s *store) routeBuckets(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.EscapedPath(), bucketPath)
	if rest == "" {
		switch r.Method {
		case http.MethodGet:
			s.listBuckets(w, r)
		case http.MethodPost:
			s.createBucket(w, r)
		default:
			methodNotAllowed(w, http.MethodGet, http.MethodPost)
		}
		return
	}
	// rest starts with "/": split the bucket id from the remaining tail.
	if !strings.HasPrefix(rest, "/") {
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	bucketRaw, tail, _ := strings.Cut(rest[1:], "/")
	if bucketRaw == "" {
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	bucketID, err := url.PathUnescape(bucketRaw)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if tail == "" {
		switch r.Method {
		case http.MethodGet:
			s.getBucket(w, r, bucketID)
		case http.MethodDelete:
			s.deleteBucket(w, r, bucketID)
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
	keyRaw, isObject := strings.CutPrefix(tail, "objects/")
	if !isObject {
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	key, err := url.PathUnescape(keyRaw)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
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
}

// validObjectKey enforces 1..1024 UTF-8 bytes without control characters.
func validObjectKey(key string) bool {
	if len(key) < 1 || len(key) > 1024 || !utf8.ValidString(key) {
		return false
	}
	for _, r := range key {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// extractMetadata collects X-Vaultgrid-Meta-* request headers into a map
// keyed by the lowercased suffix. A bare prefix, a non-UTF-8 value or a
// control character in a value makes the whole request invalid.
func extractMetadata(h http.Header) (map[string]string, bool) {
	meta := make(map[string]string)
	for name, values := range h {
		if !strings.HasPrefix(name, metaHeaderPrefix) {
			continue
		}
		suffix := name[len(metaHeaderPrefix):]
		if suffix == "" {
			return nil, false
		}
		value := strings.Join(values, ",")
		if !utf8.ValidString(value) || strings.ContainsFunc(value, unicode.IsControl) {
			return nil, false
		}
		meta[strings.ToLower(suffix)] = value
	}
	return meta, true
}

// parseConditionals validates the conditional-write headers. At most one of
// If-Match (a single strong ETag in our emitted form) and If-None-Match: *
// may be present; any other shape is a 400.
func parseConditionals(h http.Header) (ifMatch string, ifNoneMatchStar bool, ok bool) {
	ifMatchValues, hasIfMatch := h[http.CanonicalHeaderKey("If-Match")]
	ifNoneMatchValues, hasIfNoneMatch := h[http.CanonicalHeaderKey("If-None-Match")]
	if hasIfMatch && hasIfNoneMatch {
		return "", false, false
	}
	if hasIfMatch {
		if len(ifMatchValues) != 1 || !etagPattern.MatchString(strings.TrimSpace(ifMatchValues[0])) {
			return "", false, false
		}
		return strings.TrimSpace(ifMatchValues[0]), false, true
	}
	if hasIfNoneMatch {
		if len(ifNoneMatchValues) != 1 || strings.TrimSpace(ifNoneMatchValues[0]) != "*" {
			return "", false, false
		}
		return "", true, true
	}
	return "", false, true
}

// parsePrefixQuery accepts an empty query or exactly one "prefix=<value>"
// pair. Anything else (extra parameters, duplicates, malformed escapes)
// yields ok=false.
func parsePrefixQuery(raw string) (string, bool) {
	if raw == "" {
		return "", true
	}
	var prefix string
	found := false
	for _, pair := range strings.Split(raw, "&") {
		key, value, ok := strings.Cut(pair, "=")
		if !ok {
			return "", false
		}
		decodedKey, err := url.QueryUnescape(key)
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

func (s *store) createBucket(w http.ResponseWriter, r *http.Request) {
	var in createBucketInput
	if !decodeRequest(r, &in) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if !validID(in.ID) || !validID(in.PoolID) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, dup := s.buckets[in.ID]; dup {
		if existing.poolID == in.PoolID {
			writeJSON(w, http.StatusOK, existing.view())
			return
		}
		writeError(w, http.StatusConflict, "bucket_exists")
		return
	}
	p, ok := s.pools[in.PoolID]
	if !ok {
		writeError(w, http.StatusNotFound, "pool_not_found")
		return
	}
	b := &bucket{id: in.ID, poolID: in.PoolID, objects: make(map[string]*object)}
	s.buckets[in.ID] = b
	p.bucketCount++
	writeJSON(w, http.StatusCreated, b.view())
}

func (s *store) listBuckets(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids := make([]string, 0, len(s.buckets))
	for id := range s.buckets {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	items := make([]bucketView, 0, len(ids))
	for _, id := range ids {
		items = append(items, s.buckets[id].view())
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *store) getBucket(w http.ResponseWriter, r *http.Request, id string) {
	if r.URL.RawQuery != "" || !validID(id) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	s.mu.RLock()
	b, ok := s.buckets[id]
	var view bucketView
	if ok {
		view = b.view()
	}
	s.mu.RUnlock()
	if !ok {
		writeError(w, http.StatusNotFound, "bucket_not_found")
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *store) deleteBucket(w http.ResponseWriter, r *http.Request, id string) {
	if r.URL.RawQuery != "" || !validID(id) {
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
	s.pools[b.poolID].bucketCount--
	delete(s.buckets, id)
	w.WriteHeader(http.StatusNoContent)
}

// putObject handles PUT /v1/buckets/{bucketId}/objects/{key}. The body is the
// raw object content; X-Vaultgrid-Meta-* headers become the metadata,
// replacing any previous metadata wholesale.
func (s *store) putObject(w http.ResponseWriter, r *http.Request, bucketID, key string) {
	if r.URL.RawQuery != "" || !validID(bucketID) || !validObjectKey(key) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	meta, ok := extractMetadata(r.Header)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	ifMatch, ifNoneMatchStar, ok := parseConditionals(r.Header)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	sum := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(sum[:]) + `"`

	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.buckets[bucketID]
	if !ok {
		writeError(w, http.StatusNotFound, "bucket_not_found")
		return
	}
	existing := b.objects[key]
	switch {
	case ifMatch != "":
		if existing == nil || existing.etag != ifMatch {
			writeError(w, http.StatusPreconditionFailed, "precondition_failed")
			return
		}
	case ifNoneMatchStar:
		if existing != nil {
			writeError(w, http.StatusPreconditionFailed, "precondition_failed")
			return
		}
	}
	p := s.pools[b.poolID]
	var oldSize int64
	if existing != nil {
		oldSize = int64(len(existing.content))
	}
	delta := int64(len(body)) - oldSize
	// The store lock makes the capacity check and the charge atomic, and the
	// check runs before any state changes, so a failed write leaves the old
	// object and its metadata untouched.
	if delta > 0 && p.allocated() > p.rawCapacity-delta {
		writeError(w, http.StatusConflict, "insufficient_capacity")
		return
	}
	b.objects[key] = &object{key: key, content: body, metadata: meta, etag: etag}
	b.bytesUsed += delta
	p.objectBytes += delta

	status := http.StatusCreated
	if existing != nil {
		status = http.StatusOK
	}
	w.Header().Set("ETag", etag)
	writeJSON(w, status, b.objects[key].view())
}

// getObject serves GET and HEAD /v1/buckets/{bucketId}/objects/{key}: the raw
// content plus ETag and metadata headers. HEAD sends the headers only.
func (s *store) getObject(w http.ResponseWriter, r *http.Request, bucketID, key string, head bool) {
	if r.URL.RawQuery != "" || !validID(bucketID) || !validObjectKey(key) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	s.mu.RLock()
	b, ok := s.buckets[bucketID]
	if !ok {
		s.mu.RUnlock()
		writeError(w, http.StatusNotFound, "bucket_not_found")
		return
	}
	obj, ok := b.objects[key]
	if !ok {
		s.mu.RUnlock()
		writeError(w, http.StatusNotFound, "object_not_found")
		return
	}
	// Objects are swapped, never mutated, so the fetched pointer stays valid
	// after the lock is released.
	content, metadata, etag := obj.content, obj.metadata, obj.etag
	s.mu.RUnlock()

	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("ETag", etag)
	h.Set("Content-Length", strconv.Itoa(len(content)))
	for k, v := range metadata {
		h.Set(metaHeaderPrefix+k, v)
	}
	w.WriteHeader(http.StatusOK)
	if !head {
		_, _ = w.Write(content)
	}
}

// deleteObject removes an object and releases its capacity. It is idempotent
// as long as the bucket exists.
func (s *store) deleteObject(w http.ResponseWriter, r *http.Request, bucketID, key string) {
	if r.URL.RawQuery != "" || !validID(bucketID) || !validObjectKey(key) {
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
	if obj, ok := b.objects[key]; ok {
		size := int64(len(obj.content))
		b.bytesUsed -= size
		s.pools[b.poolID].objectBytes -= size
		delete(b.objects, key)
	}
	w.WriteHeader(http.StatusNoContent)
}

// listObjects returns the object summaries of a bucket, ordered by key. The
// only accepted query parameter is a single prefix filter.
func (s *store) listObjects(w http.ResponseWriter, r *http.Request, bucketID string) {
	prefix, ok := parsePrefixQuery(r.URL.RawQuery)
	if !ok || !validID(bucketID) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, ok := s.buckets[bucketID]
	if !ok {
		writeError(w, http.StatusNotFound, "bucket_not_found")
		return
	}
	keys := make([]string, 0, len(b.objects))
	for k := range b.objects {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	items := make([]objectView, 0, len(keys))
	for _, k := range keys {
		items = append(items, b.objects[k].view())
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

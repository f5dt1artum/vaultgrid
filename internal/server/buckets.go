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

// versionHeader carries the version id of an object generation on writes
// into versioning-enabled buckets and on reads of a specific version. The
// constant is already in Go's canonical header form.
const versionHeader = "X-Vaultgrid-Version-Id"

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
// the summary shape used in object listings. VersionID is only set on writes
// into versioning-enabled buckets and is omitted everywhere else.
type objectView struct {
	Key       string            `json:"key"`
	SizeBytes int64             `json:"sizeBytes"`
	Metadata  map[string]string `json:"metadata"`
	ETag      string            `json:"etag"`
	VersionID string            `json:"versionId,omitempty"`
}

// objectVersionView is one entry of the object-versions listing: a data
// version or a delete marker of one key.
type objectVersionView struct {
	Key          string            `json:"key"`
	VersionID    string            `json:"versionId"`
	IsLatest     bool              `json:"isLatest"`
	DeleteMarker bool              `json:"deleteMarker"`
	SizeBytes    int64             `json:"sizeBytes"`
	ETag         string            `json:"etag"`
	Metadata     map[string]string `json:"metadata"`
}

// versioningView is the request and response body of the bucket versioning
// endpoint. Enabling is irreversible, so the only accepted input is
// {"enabled":true}.
type versioningView struct {
	Enabled bool `json:"enabled"`
}

// bucket is a namespace of objects bound to one pool. objects maps each key
// to its generations, newest first; a key whose generations are all gone is
// removed from the map. In a bucket without versioning each key holds exactly
// one data version with an empty versionID and deletes are permanent. Once
// versioning is enabled (irreversibly) every generation carries a unique
// non-empty versionID, writes add generations and deletes add delete markers.
// tenant owns the bucket; the bytes of every data version are charged to the
// tenant's usage in the pool.
type bucket struct {
	id         string
	poolID     string
	tenant     string
	versioning bool
	objects    map[string][]*objectVersion
}

// bytesUsed sums every data version of every key; delete markers carry no
// bytes. The same total is charged to the pool as objectBytes.
func (b *bucket) bytesUsed() int64 {
	var total int64
	for _, versions := range b.objects {
		for _, v := range versions {
			total += v.sizeBytes
		}
	}
	return total
}

// visibleKeys counts the keys whose newest generation is a data version.
func (b *bucket) visibleKeys() int {
	n := 0
	for _, versions := range b.objects {
		if len(versions) > 0 && !versions[0].deleteMarker {
			n++
		}
	}
	return n
}

func (b *bucket) view() bucketView {
	return bucketView{ID: b.id, PoolID: b.poolID, ObjectCount: b.visibleKeys(), BytesUsed: b.bytesUsed()}
}

// visible returns the newest generation of key when it is a data version,
// and nil when the key is unknown or its newest generation is a delete
// marker. Conditional writes and plain reads judge only this generation.
func (b *bucket) visible(key string) *objectVersion {
	if versions := b.objects[key]; len(versions) > 0 && !versions[0].deleteMarker {
		return versions[0]
	}
	return nil
}

// objectVersion is one generation of an object key: either a data version
// holding content, or a delete marker hiding every older generation. etag is
// the quoted lowercase hex SHA-256 of content ("" on a delete marker).
type objectVersion struct {
	versionID    string
	deleteMarker bool
	content      []byte
	metadata     map[string]string
	etag         string
	sizeBytes    int64
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
	if tail == "versioning" {
		switch r.Method {
		case http.MethodGet:
			s.getVersioning(w, bucketID)
		case http.MethodPut:
			s.putVersioning(w, r, bucketID)
		default:
			methodNotAllowed(w, http.MethodGet, http.MethodPut)
		}
		return
	}
	if tail == "object-versions" {
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		s.listObjectVersions(w, r, bucketID)
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
	s.buckets[in.ID] = &bucket{id: in.ID, poolID: in.PoolID, tenant: tenant, objects: make(map[string][]*objectVersion)}
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
	// Any remaining generation — data version or delete marker — blocks
	// bucket deletion, even when none of it is currently visible.
	if len(b.objects) > 0 {
		writeError(w, http.StatusConflict, "bucket_not_empty")
		return
	}
	delete(s.buckets, id)
	s.appendAudit(auditBucketDeleted, bucketPath+"/"+id, b.poolID, 0)
	w.WriteHeader(http.StatusNoContent)
}

// getVersioning handles GET /v1/buckets/{bucketId}/versioning.
func (s *store) getVersioning(w http.ResponseWriter, bucketID string) {
	if !validID(bucketID) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	s.mu.RLock()
	b, ok := s.buckets[bucketID]
	enabled := false
	if ok {
		enabled = b.versioning
	}
	s.mu.RUnlock()
	if !ok {
		writeError(w, http.StatusNotFound, "bucket_not_found")
		return
	}
	writeJSON(w, http.StatusOK, versioningView{Enabled: enabled})
}

// putVersioning handles PUT /v1/buckets/{bucketId}/versioning. Enabling is
// idempotent and irreversible: every accepted request answers 200, but only
// the transition itself records an audit event.
func (s *store) putVersioning(w http.ResponseWriter, r *http.Request, bucketID string) {
	var in versioningView
	if !decodeRequest(r, &in) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if !validID(bucketID) || !in.Enabled {
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
	if !b.versioning {
		b.versioning = true
		// Objects written before versioning was enabled become addressable
		// versions, so every generation in a versioned bucket has a unique
		// non-empty id.
		for _, versions := range b.objects {
			for _, v := range versions {
				if v.versionID == "" {
					v.versionID = s.nextVersionID()
				}
			}
		}
		s.appendAudit(auditVersioningEnabled, bucketPath+"/"+bucketID+"/versioning", b.poolID, 0)
	}
	writeJSON(w, http.StatusOK, versioningView{Enabled: true})
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

// parseVersionQuery accepts an empty query or exactly one
// "versionId=<non-empty>" pair. An empty or duplicated versionId, or any
// other parameter alongside it, yields ok=false.
func parseVersionQuery(raw string) (versionID string, present, ok bool) {
	if raw == "" {
		return "", false, true
	}
	for _, pair := range strings.Split(raw, "&") {
		k, value, found := strings.Cut(pair, "=")
		if !found {
			return "", false, false
		}
		decodedKey, err := url.QueryUnescape(k)
		if err != nil || decodedKey != "versionId" || present {
			return "", false, false
		}
		decodedValue, err := url.QueryUnescape(value)
		if err != nil || decodedValue == "" {
			return "", false, false
		}
		versionID, present = decodedValue, true
	}
	return versionID, present, true
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
	// Conditional writes judge only the currently visible generation: a key
	// whose newest generation is a delete marker counts as absent.
	existing := b.visible(key)
	if ifMatch != "" && (existing == nil || existing.etag != ifMatch) {
		writeError(w, http.StatusPreconditionFailed, "precondition_failed")
		return
	}
	if ifNoneMatchStar && existing != nil {
		writeError(w, http.StatusPreconditionFailed, "precondition_failed")
		return
	}
	// A versioned write keeps every older generation, so it always charges
	// the full new size; an unversioned overwrite releases the old
	// generation and only charges the delta.
	var oldSize int64
	if existing != nil && !b.versioning {
		oldSize = existing.sizeBytes
	}
	p := s.pools[b.poolID]
	// The store lock makes the quota and capacity checks plus the charge
	// atomic, so concurrent writes can never exceed either limit, and a failed
	// write leaves the old generations and their metadata untouched. Objects
	// inherit the bucket's tenant; only a growing write can breach the quota.
	if delta := size - oldSize; delta > 0 && p.tenantQuotaExceeded(b.tenant, delta) {
		writeError(w, http.StatusConflict, "tenant_quota_exceeded")
		return
	}
	if p.allocated() > p.rawCapacity-(size-oldSize) {
		writeError(w, http.StatusConflict, "insufficient_capacity")
		return
	}
	resource := bucketPath + "/" + bucketID + "/objects/" + key
	status := http.StatusCreated
	if existing != nil {
		status = http.StatusOK
	}
	if b.versioning {
		v := &objectVersion{versionID: s.nextVersionID(), content: content, metadata: metadata, etag: etag, sizeBytes: size}
		b.objects[key] = append([]*objectVersion{v}, b.objects[key]...)
		p.objectBytes += size
		p.tenantUsed[b.tenant] += size
		s.appendAudit(auditVersionCreated, resource, b.poolID, size)
		w.Header().Set("ETag", etag)
		w.Header().Set(versionHeader, v.versionID)
		writeJSON(w, status, objectView{Key: key, SizeBytes: size, Metadata: metadata, ETag: etag, VersionID: v.versionID})
		return
	}
	b.objects[key] = []*objectVersion{{content: content, metadata: metadata, etag: etag, sizeBytes: size}}
	p.objectBytes += size - oldSize
	p.tenantUsed[b.tenant] += size - oldSize
	// An overwrite is recorded even when the size delta is zero.
	if existing != nil {
		s.appendAudit(auditObjectOverwritten, resource, b.poolID, size-oldSize)
	} else {
		s.appendAudit(auditObjectCreated, resource, b.poolID, size)
	}
	w.Header().Set("ETag", etag)
	writeJSON(w, status, objectView{Key: key, SizeBytes: size, Metadata: metadata, ETag: etag})
}

// getObject serves both GET and HEAD; headOnly suppresses the body.
func (s *store) getObject(w http.ResponseWriter, r *http.Request, bucketID, key string, headOnly bool) {
	if !validID(bucketID) || !validObjectKey(key) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	versionID, hasVersion, ok := parseVersionQuery(r.URL.RawQuery)
	if !ok {
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
	if !b.versioning && r.URL.RawQuery != "" {
		// Buckets without versioning keep the original no-query behavior.
		s.mu.RUnlock()
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	var v *objectVersion
	errCode := ""
	if hasVersion {
		for _, candidate := range b.objects[key] {
			if candidate.versionID == versionID {
				v = candidate
				break
			}
		}
		if v == nil || v.deleteMarker {
			v = nil
			errCode = "object_version_not_found"
		}
	} else {
		if v = b.visible(key); v == nil {
			errCode = "object_not_found"
		}
	}
	versioned := b.versioning
	s.mu.RUnlock()
	if errCode != "" {
		writeError(w, http.StatusNotFound, errCode)
		return
	}
	for name, value := range v.metadata {
		w.Header().Set(metaPrefix+name, value)
	}
	w.Header().Set("ETag", v.etag)
	if versioned {
		w.Header().Set(versionHeader, v.versionID)
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(v.sizeBytes, 10))
	w.WriteHeader(http.StatusOK)
	if !headOnly {
		_, _ = w.Write(v.content)
	}
}

func (s *store) deleteObject(w http.ResponseWriter, r *http.Request, bucketID, key string) {
	if !validID(bucketID) || !validObjectKey(key) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	versionID, hasVersion, ok := parseVersionQuery(r.URL.RawQuery)
	if !ok {
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
	if !b.versioning {
		if r.URL.RawQuery != "" {
			// Buckets without versioning keep the original no-query behavior.
			writeError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		// Deleting an unknown or already-deleted object is a success with no
		// state change, so only a real deletion records an event.
		if v := b.visible(key); v != nil {
			s.pools[b.poolID].objectBytes -= v.sizeBytes
			s.pools[b.poolID].tenantUsed[b.tenant] -= v.sizeBytes
			delete(b.objects, key)
			s.appendAudit(auditObjectDeleted, bucketPath+"/"+bucketID+"/objects/"+key, b.poolID, -v.sizeBytes)
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	resource := bucketPath + "/" + bucketID + "/objects/" + key
	if hasVersion {
		// Permanent deletion of one addressed generation; only a data
		// version releases capacity.
		versions := b.objects[key]
		idx := -1
		for i, v := range versions {
			if v.versionID == versionID {
				idx = i
				break
			}
		}
		if idx < 0 {
			writeError(w, http.StatusNotFound, "object_version_not_found")
			return
		}
		v := versions[idx]
		b.objects[key] = append(versions[:idx], versions[idx+1:]...)
		if len(b.objects[key]) == 0 {
			delete(b.objects, key)
		}
		if !v.deleteMarker {
			s.pools[b.poolID].objectBytes -= v.sizeBytes
			s.pools[b.poolID].tenantUsed[b.tenant] -= v.sizeBytes
		}
		s.appendAudit(auditVersionDeleted, resource, b.poolID, -v.sizeBytes)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// Without a versionId the delete adds a delete marker on top of the
	// key. When the newest generation already is a marker the request is a
	// success with no state change, so nothing is recorded.
	if versions := b.objects[key]; len(versions) > 0 && versions[0].deleteMarker {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	marker := &objectVersion{versionID: s.nextVersionID(), deleteMarker: true, metadata: map[string]string{}}
	b.objects[key] = append([]*objectVersion{marker}, b.objects[key]...)
	s.appendAudit(auditDeleteMarkerMade, resource, b.poolID, 0)
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
			// Only the currently visible object of each key is listed.
			v := b.visible(k)
			if v != nil && strings.HasPrefix(k, prefix) {
				items = append(items, objectView{Key: k, SizeBytes: v.sizeBytes, Metadata: v.metadata, ETag: v.etag})
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

// listObjectVersions handles GET /v1/buckets/{bucketId}/object-versions.
// Every generation of every matching key is listed — data versions and
// delete markers — ordered by key ascending, newest generation first within
// one key.
func (s *store) listObjectVersions(w http.ResponseWriter, r *http.Request, bucketID string) {
	prefix, ok := parsePrefixQuery(r.URL.RawQuery)
	if !ok || !validID(bucketID) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	s.mu.RLock()
	b, ok := s.buckets[bucketID]
	var items []objectVersionView
	if ok {
		keys := make([]string, 0, len(b.objects))
		for k := range b.objects {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		items = make([]objectVersionView, 0, len(keys))
		for _, k := range keys {
			if !strings.HasPrefix(k, prefix) {
				continue
			}
			for i, v := range b.objects[k] {
				items = append(items, objectVersionView{
					Key:          k,
					VersionID:    v.versionID,
					IsLatest:     i == 0,
					DeleteMarker: v.deleteMarker,
					SizeBytes:    v.sizeBytes,
					ETag:         v.etag,
					Metadata:     v.metadata,
				})
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

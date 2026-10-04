package server

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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

// versionHeader carries the versionId of a stored object version on write
// responses and on versioned reads. It is already in Go's canonical form.
const versionHeader = "X-Vaultgrid-Version-Id"

type createBucketInput struct {
	ID     string `json:"id"`
	PoolID string `json:"poolId"`
}

// versioningInput is the sole accepted body of PUT .../versioning: the only
// valid payload is {"enabled":true}.
type versioningInput struct {
	Enabled bool `json:"enabled"`
}

// bucketView is the public representation of a bucket.
type bucketView struct {
	ID          string `json:"id"`
	PoolID      string `json:"poolId"`
	ObjectCount int    `json:"objectCount"`
	BytesUsed   int64  `json:"bytesUsed"`
}

// versioningView is the public representation of a bucket's versioning state.
type versioningView struct {
	Enabled bool `json:"enabled"`
}

// objectView is the public representation of an object after a write, and
// the summary shape used in object listings.
type objectView struct {
	Key       string            `json:"key"`
	SizeBytes int64             `json:"sizeBytes"`
	Metadata  map[string]string `json:"metadata"`
	Etag      string            `json:"etag"`
}

// versionedObjectView is the write response shape in a versioned bucket.
type versionedObjectView struct {
	Key       string            `json:"key"`
	VersionID string            `json:"versionId"`
	SizeBytes int64             `json:"sizeBytes"`
	Metadata  map[string]string `json:"metadata"`
	Etag      string            `json:"etag"`
}

// objectVersionView is one row of GET .../object-versions.
type objectVersionView struct {
	Key          string            `json:"key"`
	VersionID    string            `json:"versionId"`
	IsLatest     bool              `json:"isLatest"`
	DeleteMarker bool              `json:"deleteMarker"`
	SizeBytes    int64             `json:"sizeBytes"`
	Etag         string            `json:"etag"`
	Metadata     map[string]string `json:"metadata"`
}

// bucket is a namespace of objects bound to one pool. tenant owns the bucket;
// object bytes are charged to the tenant's usage in the pool.
//
// Each key maps to one object whose versions are kept in creation order
// (oldest first, newest last). In a non-versioned bucket the slice holds at
// most one data version and every write replaces it; in a versioned bucket
// writes append data versions and unversioned deletes append delete markers.
// bytesUsed counts every data version, including hidden ones; objectCount
// counts only keys whose latest version is a data version.
type bucket struct {
	id                string
	poolID            string
	tenant            string
	versioningEnabled bool
	objects           map[string]*object
}

func (b *bucket) bytesUsed() int64 {
	var total int64
	for _, o := range b.objects {
		for _, v := range o.versions {
			if !v.isMarker {
				total += v.sizeBytes
			}
		}
	}
	return total
}

func (b *bucket) visibleCount() int {
	count := 0
	for _, o := range b.objects {
		if o.visible() != nil {
			count++
		}
	}
	return count
}

func (b *bucket) view() bucketView {
	return bucketView{ID: b.id, PoolID: b.poolID, ObjectCount: b.visibleCount(), BytesUsed: b.bytesUsed()}
}

// notEmpty reports whether any data version or delete marker remains; such a
// bucket cannot be deleted.
func (b *bucket) notEmpty() bool { return len(b.objects) > 0 }

// findVersion returns the version with the given id and its position, or nil
// when the key or version does not exist.
func (b *bucket) findVersion(key, versionID string) (*objectVersion, int) {
	o := b.objects[key]
	if o == nil {
		return nil, -1
	}
	for i, v := range o.versions {
		if v.versionID == versionID {
			return v, i
		}
	}
	return nil, -1
}

// newVersionID mints a unique, non-empty version id. The caller must hold the
// store write lock.
func (b *bucket) newVersionID() string {
	for {
		var raw [16]byte
		if _, err := rand.Read(raw[:]); err != nil {
			// crypto/rand failure is not recoverable; a panic keeps a failed
			// write from ever committing a placeholder id.
			panic(err)
		}
		id := hex.EncodeToString(raw[:])
		collision := false
		for _, o := range b.objects {
			for _, v := range o.versions {
				if v.versionID == id {
					collision = true
				}
			}
		}
		if !collision {
			return id
		}
	}
}

// object groups every version of one key.
type object struct {
	key      string
	versions []*objectVersion
}

// latest returns the most recently created version, or nil when the key has
// no versions at all.
func (o *object) latest() *objectVersion {
	if o == nil || len(o.versions) == 0 {
		return nil
	}
	return o.versions[len(o.versions)-1]
}

// visible returns the current data version clients see without a versionId:
// the latest version unless that is a delete marker.
func (o *object) visible() *objectVersion {
	v := o.latest()
	if v == nil || v.isMarker {
		return nil
	}
	return v
}

// objectVersion is one stored data version or a delete marker. A marker
// carries no content, etag or metadata and charges no bytes. etag on a data
// version is the quoted lowercase hex SHA-256 of content.
type objectVersion struct {
	versionID string
	content   []byte
	metadata  map[string]string
	etag      string
	sizeBytes int64
	isMarker  bool
}

func (v *objectVersion) objectView(key string) objectView {
	return objectView{Key: key, SizeBytes: v.sizeBytes, Metadata: v.metadata, Etag: v.etag}
}

func (v *objectVersion) versionedView(key string) versionedObjectView {
	return versionedObjectView{Key: key, VersionID: v.versionID, SizeBytes: v.sizeBytes, Metadata: v.metadata, Etag: v.etag}
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
	switch tail {
	case "objects":
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		s.listObjects(w, r, bucketID)
		return
	case "versioning":
		switch r.Method {
		case http.MethodGet:
			s.getVersioning(w, r, bucketID)
		case http.MethodPut:
			s.putVersioning(w, r, bucketID)
		default:
			methodNotAllowed(w, http.MethodGet, http.MethodPut)
		}
		return
	case "object-versions":
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		s.listObjectVersions(w, r, bucketID)
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
	// A bucket with any surviving data version or delete marker is not empty.
	if b.notEmpty() {
		writeError(w, http.StatusConflict, "bucket_not_empty")
		return
	}
	delete(s.buckets, id)
	s.appendAudit(auditBucketDeleted, bucketPath+"/"+id, b.poolID, 0)
	w.WriteHeader(http.StatusNoContent)
}

// getVersioning handles GET /v1/buckets/{bucketId}/versioning.
func (s *store) getVersioning(w http.ResponseWriter, r *http.Request, bucketID string) {
	if !validID(bucketID) || r.URL.RawQuery != "" {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	s.mu.RLock()
	b, ok := s.buckets[bucketID]
	enabled := ok && b.versioningEnabled
	s.mu.RUnlock()
	if !ok {
		writeError(w, http.StatusNotFound, "bucket_not_found")
		return
	}
	writeJSON(w, http.StatusOK, versioningView{Enabled: enabled})
}

// putVersioning handles PUT /v1/buckets/{bucketId}/versioning. Enabling is
// idempotent: first enablement migrates existing objects to versions and is
// audited; replays answer 200 with no state change and no event. The only
// accepted body is {"enabled":true}.
func (s *store) putVersioning(w http.ResponseWriter, r *http.Request, bucketID string) {
	var in versioningInput
	if !decodeRequest(r, &in) || !in.Enabled {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if !validID(bucketID) || r.URL.RawQuery != "" {
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
	if b.versioningEnabled {
		// Idempotent replay: no state change, so no audit event.
		writeJSON(w, http.StatusOK, versioningView{Enabled: true})
		return
	}
	b.versioningEnabled = true
	// Every existing object becomes the first (latest) data version of its
	// key. Capacity does not change: the bytes were already charged once.
	keys := make([]string, 0, len(b.objects))
	for key := range b.objects {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		for _, v := range b.objects[key].versions {
			v.versionID = b.newVersionID()
		}
	}
	s.appendAudit(auditBucketVersioning, bucketPath+"/"+bucketID+"/versioning", b.poolID, 0)
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
	p := s.pools[b.poolID]
	resource := bucketPath + "/" + bucketID + "/objects/" + key
	obj := b.objects[key]

	if b.versioningEnabled {
		s.putObjectVersioned(w, b, p, obj, key, resource, content, metadata, etag, size, ifMatch, ifNoneMatchStar)
		return
	}

	var existing *objectVersion
	if obj != nil {
		existing = obj.latest()
	}
	// Conditions judge the single current version.
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
	created := &objectVersion{content: content, metadata: metadata, etag: etag, sizeBytes: size}
	b.objects[key] = &object{key: key, versions: []*objectVersion{created}}
	p.objectBytes += size - oldSize
	p.tenantUsed[b.tenant] += size - oldSize
	// An overwrite is recorded even when the size delta is zero.
	status := http.StatusCreated
	if existing != nil {
		s.appendAudit(auditObjectOverwritten, resource, b.poolID, size-oldSize)
		status = http.StatusOK
	} else {
		s.appendAudit(auditObjectCreated, resource, b.poolID, size)
	}
	w.Header().Set("ETag", etag)
	writeJSON(w, status, created.objectView(key))
}

// putObjectVersioned commits a write against a versioned bucket: every
// successful put appends a fresh data version charged at its full size,
// leaving every older version (and its bytes) in place. Conditional writes
// judge only the currently visible version. The caller holds the store lock.
func (s *store) putObjectVersioned(w http.ResponseWriter, b *bucket, p *pool, obj *object, key, resource string,
	content []byte, metadata map[string]string, etag string, size int64, ifMatch string, ifNoneMatchStar bool) {
	visible := obj.visible()
	if ifMatch != "" && (visible == nil || visible.etag != ifMatch) {
		writeError(w, http.StatusPreconditionFailed, "precondition_failed")
		return
	}
	if ifNoneMatchStar && visible != nil {
		writeError(w, http.StatusPreconditionFailed, "precondition_failed")
		return
	}
	// A new version is charged in full; older versions stay charged.
	if p.tenantQuotaExceeded(b.tenant, size) {
		writeError(w, http.StatusConflict, "tenant_quota_exceeded")
		return
	}
	if p.allocated() > p.rawCapacity-size {
		writeError(w, http.StatusConflict, "insufficient_capacity")
		return
	}
	created := &objectVersion{
		versionID: b.newVersionID(),
		content:   content,
		metadata:  metadata,
		etag:      etag,
		sizeBytes: size,
	}
	if obj == nil {
		obj = &object{key: key}
		b.objects[key] = obj
	}
	obj.versions = append(obj.versions, created)
	p.objectBytes += size
	p.tenantUsed[b.tenant] += size
	s.appendAudit(auditObjectVersioned, resource, b.poolID, size)
	// 201 when the put creates the currently visible object (key was absent or
	// hidden behind a delete marker), 200 when it overwrites visible data.
	status := http.StatusCreated
	if visible != nil {
		status = http.StatusOK
	}
	w.Header().Set("ETag", etag)
	w.Header().Set(versionHeader, created.versionID)
	writeJSON(w, status, created.versionedView(key))
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
	b, bucketExists := s.buckets[bucketID]
	var (
		v             *objectVersion
		notFoundCode  string
		rejectInvalid bool
	)
	if bucketExists {
		obj := b.objects[key]
		switch {
		case hasVersion && !b.versioningEnabled:
			// Buckets without versioning keep their original behavior: any
			// query parameter is an invalid request.
			rejectInvalid = true
		case hasVersion:
			found, _ := b.findVersion(key, versionID)
			if found == nil || found.isMarker {
				notFoundCode = "object_version_not_found"
			} else {
				v = found
			}
		default:
			// No versionId: serve the currently visible version (the latest
			// data version unless it is hidden by a delete marker).
			if cur := obj.visible(); cur == nil {
				notFoundCode = "object_not_found"
			} else {
				v = cur
			}
		}
	}
	versioned := bucketExists && b.versioningEnabled
	// Snapshot the selected version before releasing the read lock so a
	// concurrent overwrite, delete or new version cannot mix one version's
	// bytes with another version's headers. Version content and metadata are
	// immutable once stored; overwrites install a fresh version value.
	var content []byte
	var size int64
	var etag, version string
	var meta map[string]string
	if v != nil {
		content, size, etag, version, meta = v.content, v.sizeBytes, v.etag, v.versionID, v.metadata
	}
	s.mu.RUnlock()
	if !bucketExists {
		writeError(w, http.StatusNotFound, "bucket_not_found")
		return
	}
	if rejectInvalid {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if notFoundCode != "" {
		writeError(w, http.StatusNotFound, notFoundCode)
		return
	}
	s.writeObjectResponse(w, r, headOnly, versioned, version, etag, meta, content, size)
}

// writeObjectResponse answers a GET/HEAD read of one resolved object version,
// applying an optional single byte range. Bucket, key, query and version
// validation has already happened by the time this runs, so an unsatisfiable
// or malformed Range can never mask a 404. Reads are side-effect free: they
// touch no object state, counters or audit log.
func (s *store) writeObjectResponse(w http.ResponseWriter, r *http.Request, headOnly, versioned bool,
	version, etag string, meta map[string]string, content []byte, size int64) {
	br, ok := parseRangeHeader(r.Header)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}

	for name, value := range meta {
		w.Header().Set(metaPrefix+name, value)
	}
	w.Header().Set("ETag", etag)
	if versioned {
		w.Header().Set(versionHeader, version)
	}
	w.Header().Set("Content-Type", "application/octet-stream")

	// No Range header answers 200 with the complete representation.
	if br == nil {
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		w.WriteHeader(http.StatusOK)
		if !headOnly {
			_, _ = w.Write(content)
		}
		return
	}

	// Any range against an empty object is unsatisfiable, even a suffix.
	if size == 0 {
		writeRangeNotSatisfiable(w, size)
		return
	}

	// A suffix that covers at least the whole object degrades to a complete
	// 200 read; the object is shorter than the requested tail.
	if br.suffix && br.suffixLen >= size {
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		w.WriteHeader(http.StatusOK)
		if !headOnly {
			_, _ = w.Write(content)
		}
		return
	}

	var start, end int64
	switch {
	case br.suffix:
		start = size - br.suffixLen
		end = size - 1
	case br.end == -1:
		// Open-ended "start-".
		if br.start >= size {
			writeRangeNotSatisfiable(w, size)
			return
		}
		start = br.start
		end = size - 1
	default:
		if br.start > br.end || br.start >= size {
			writeRangeNotSatisfiable(w, size)
			return
		}
		start = br.start
		end = br.end
		if end >= size {
			// An end past the object is clipped to the last byte.
			end = size - 1
		}
	}

	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
	w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	w.WriteHeader(http.StatusPartialContent)
	if !headOnly {
		_, _ = w.Write(content[start : end+1])
	}
}

// writeRangeNotSatisfiable answers 416 with the mandated Content-Range marker
// carrying the complete length of the selected version.
func writeRangeNotSatisfiable(w http.ResponseWriter, size int64) {
	w.Header().Set("Content-Range", "bytes */"+strconv.FormatInt(size, 10))
	writeError(w, http.StatusRequestedRangeNotSatisfiable, "range_not_satisfiable")
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
	b, exists := s.buckets[bucketID]
	if !exists {
		writeError(w, http.StatusNotFound, "bucket_not_found")
		return
	}
	if !b.versioningEnabled {
		s.deleteObjectUnversioned(w, b, key, r.URL.RawQuery)
		return
	}
	resource := bucketPath + "/" + bucketID + "/objects/" + key
	if !hasVersion {
		obj := b.objects[key]
		var latest *objectVersion
		if obj != nil {
			latest = obj.latest()
		}
		// Deleting with no versionId adds a delete marker; only when the
		// latest version is already a marker is the request a successful
		// no-op.
		if latest != nil && latest.isMarker {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		marker := &objectVersion{versionID: b.newVersionID(), isMarker: true}
		if obj == nil {
			obj = &object{key: key}
			b.objects[key] = obj
		}
		obj.versions = append(obj.versions, marker)
		// The marker charges no bytes; hidden data versions stay charged.
		s.appendAudit(auditObjectMarker, resource, b.poolID, 0)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// A specific version is permanently removed; only a data version frees
	// capacity. An unknown version (including a version of another key) is an
	// error.
	v, idx := b.findVersion(key, versionID)
	if v == nil {
		writeError(w, http.StatusNotFound, "object_version_not_found")
		return
	}
	obj := b.objects[key]
	obj.versions = append(obj.versions[:idx], obj.versions[idx+1:]...)
	delta := int64(0)
	if !v.isMarker {
		delta = -v.sizeBytes
		p := s.pools[b.poolID]
		p.objectBytes -= v.sizeBytes
		p.tenantUsed[b.tenant] -= v.sizeBytes
	}
	if len(obj.versions) == 0 {
		delete(b.objects, key)
	}
	s.appendAudit(auditObjectVersionDel, resource, b.poolID, delta)
	w.WriteHeader(http.StatusNoContent)
}

// deleteObjectUnversioned preserves the original delete semantics for buckets
// without versioning: the single current version is removed, query strings
// are rejected, and deleting a missing object is a successful no-op. The
// caller holds the store lock.
func (s *store) deleteObjectUnversioned(w http.ResponseWriter, b *bucket, key, rawQuery string) {
	if rawQuery != "" {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if o, exists := b.objects[key]; exists && len(o.versions) > 0 {
		v := o.versions[0]
		s.pools[b.poolID].objectBytes -= v.sizeBytes
		s.pools[b.poolID].tenantUsed[b.tenant] -= v.sizeBytes
		delete(b.objects, key)
		s.appendAudit(auditObjectDeleted, bucketPath+"/"+b.id+"/objects/"+key, b.poolID, -v.sizeBytes)
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

// parseVersionQuery accepts an empty query or exactly one non-empty
// "versionId=<value>" pair. Empty, duplicated, value-less or otherwise
// accompanied parameters yield ok=false.
func parseVersionQuery(raw string) (versionID string, present, ok bool) {
	if raw == "" {
		return "", false, true
	}
	for _, pair := range strings.Split(raw, "&") {
		k, value, cutOK := strings.Cut(pair, "=")
		if !cutOK {
			return "", false, false
		}
		key, err := url.QueryUnescape(k)
		if err != nil || key != "versionId" || present {
			return "", false, false
		}
		decoded, err := url.QueryUnescape(value)
		if err != nil || decoded == "" {
			return "", false, false
		}
		versionID, present = decoded, true
	}
	return versionID, present, true
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
		for k, obj := range b.objects {
			// The ordinary listing shows only currently visible objects; a key
			// whose latest version is a delete marker is omitted.
			if obj.visible() != nil {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		items = make([]objectView, 0, len(keys))
		for _, k := range keys {
			if strings.HasPrefix(k, prefix) {
				items = append(items, b.objects[k].visible().objectView(k))
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

// listObjectVersions handles GET /v1/buckets/{bucketId}/object-versions. It
// returns every matching data version and delete marker sorted by key
// ascending and, within one key, newest first.
func (s *store) listObjectVersions(w http.ResponseWriter, r *http.Request, bucketID string) {
	prefix, ok := parsePrefixQuery(r.URL.RawQuery)
	if !ok || !validID(bucketID) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	s.mu.RLock()
	b, exists := s.buckets[bucketID]
	var items []objectVersionView
	if exists {
		keys := make([]string, 0, len(b.objects))
		for k := range b.objects {
			if strings.HasPrefix(k, prefix) {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		items = make([]objectVersionView, 0)
		for _, k := range keys {
			obj := b.objects[k]
			latest := len(obj.versions) - 1
			for i := latest; i >= 0; i-- {
				v := obj.versions[i]
				row := objectVersionView{
					Key:          k,
					VersionID:    v.versionID,
					IsLatest:     i == latest,
					DeleteMarker: v.isMarker,
					Metadata:     v.metadata,
				}
				if !v.isMarker {
					row.SizeBytes = v.sizeBytes
					row.Etag = v.etag
					if row.Metadata == nil {
						row.Metadata = map[string]string{}
					}
				} else {
					row.Metadata = map[string]string{}
				}
				items = append(items, row)
			}
		}
	}
	s.mu.RUnlock()
	if !exists {
		writeError(w, http.StatusNotFound, "bucket_not_found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

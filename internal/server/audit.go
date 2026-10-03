package server

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Audit actions emitted by state-changing endpoints.
const (
	actionPoolCreated        = "pool.created"
	actionPoolDeleted        = "pool.deleted"
	actionReservationCreated = "reservation.created"
	actionReservationDeleted = "reservation.deleted"
	actionVolumeCreated      = "volume.created"
	actionVolumeDeleted      = "volume.deleted"
	actionVolumeBound        = "volume.bound"
	actionVolumeUnbound      = "volume.unbound"
	actionSnapshotCreated    = "snapshot.created"
	actionSnapshotDeleted    = "snapshot.deleted"
	actionCloneCreated       = "clone.created"
	actionBucketCreated      = "bucket.created"
	actionBucketDeleted      = "bucket.deleted"
	actionObjectCreated      = "object.created"
	actionObjectOverwritten  = "object.overwritten"
	actionObjectDeleted      = "object.deleted"
)

// auditEvent is one recorded state change. sequence is 1-based and assigned in
// store commit order; resource is the public request path of the changed
// resource; poolID is the owning pool; bytesDelta is the signed change to that
// pool's allocatedBytes (0 when the change does not move capacity).
type auditEvent struct {
	sequence   int64
	action     string
	resource   string
	poolID     string
	bytesDelta int64
}

// auditEventView is the public JSON representation of an audit event.
type auditEventView struct {
	Sequence   int64  `json:"sequence"`
	Action     string `json:"action"`
	Resource   string `json:"resource"`
	PoolID     string `json:"poolId"`
	BytesDelta int64  `json:"bytesDelta"`
}

func (e auditEvent) view() auditEventView {
	return auditEventView{
		Sequence:   e.sequence,
		Action:     e.action,
		Resource:   e.resource,
		PoolID:     e.poolID,
		BytesDelta: e.bytesDelta,
	}
}

// auditEventsPage is the fixed-shape audit response. A struct (rather than a
// map) keeps the field order exactly items, nextAfter, hasMore.
type auditEventsPage struct {
	Items     []auditEventView `json:"items"`
	NextAfter int64            `json:"nextAfter"`
	HasMore   bool             `json:"hasMore"`
}

// recordEvent appends an audit event, assigning the next sequence. Callers
// must hold s.mu (write lock) and must only call it once the business state
// and capacity have been updated for a request that genuinely changed state,
// so the event commits atomically with that state and is never left behind by
// a failure.
func (s *store) recordEvent(action, resource, poolID string, bytesDelta int64) {
	s.auditSeq++
	s.events = append(s.events, auditEvent{
		sequence:   s.auditSeq,
		action:     action,
		resource:   resource,
		poolID:     poolID,
		bytesDelta: bytesDelta,
	})
}

// auditEventsHandler dispatches /v1/audit-events. The collection only supports
// GET; sub-paths are unknown.
func (s *store) routeAuditEvents(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == auditEventsPath {
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		s.listAuditEvents(w, r)
		return
	}
	writeError(w, http.StatusNotFound, "not_found")
}

// parseAuditEventsQuery accepts an empty query or any combination of the
// "after", "limit" and "poolId" parameters, each given at most once and always
// with a value. after must be a non-negative decimal integer and limit a
// decimal integer in 1..1000; poolId follows the usual id rules but is only
// shape-checked here (existence is checked against the store). Duplicated,
// unknown, value-less or malformed parameters yield ok=false.
func parseAuditEventsQuery(raw string) (after, limit int64, poolID string, hasPool, ok bool) {
	limit = 100
	if raw == "" {
		return 0, limit, "", false, true
	}
	seenAfter, seenLimit, seenPool := false, false, false
	for _, pair := range strings.Split(raw, "&") {
		k, value, found := strings.Cut(pair, "=")
		if !found {
			return 0, 0, "", false, false
		}
		key, err := url.QueryUnescape(k)
		if err != nil {
			return 0, 0, "", false, false
		}
		decoded, err := url.QueryUnescape(value)
		if err != nil {
			return 0, 0, "", false, false
		}
		switch key {
		case "after":
			if seenAfter {
				return 0, 0, "", false, false
			}
			seenAfter = true
			parsed, err := strconv.ParseInt(decoded, 10, 64)
			if err != nil || parsed < 0 {
				return 0, 0, "", false, false
			}
			after = parsed
		case "limit":
			if seenLimit {
				return 0, 0, "", false, false
			}
			seenLimit = true
			parsed, err := strconv.ParseInt(decoded, 10, 64)
			if err != nil || parsed < 1 || parsed > 1000 {
				return 0, 0, "", false, false
			}
			limit = parsed
		case "poolId":
			if seenPool {
				return 0, 0, "", false, false
			}
			seenPool = true
			poolID = decoded
		default:
			return 0, 0, "", false, false
		}
	}
	return after, limit, poolID, seenPool, true
}

// listAuditEvents handles GET /v1/audit-events. Events are scanned in sequence
// order under a single read lock, so a whole page (and the hasMore answer)
// reflects one consistent read state even while other requests append events.
func (s *store) listAuditEvents(w http.ResponseWriter, r *http.Request) {
	after, limit, poolID, hasPool, ok := parseAuditEventsQuery(r.URL.RawQuery)
	if !ok || (hasPool && !validID(poolID)) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	if hasPool {
		if _, exists := s.pools[poolID]; !exists {
			writeError(w, http.StatusNotFound, "pool_not_found")
			return
		}
	}

	items := make([]auditEventView, 0, min(int(limit), len(s.events)))
	var hasMore bool
	for _, e := range s.events {
		if e.sequence <= after {
			continue
		}
		if hasPool && e.poolID != poolID {
			continue
		}
		if int64(len(items)) < limit {
			items = append(items, e.view())
			continue
		}
		// The first match past the page proves more matching events remain in
		// this same read state.
		hasMore = true
		break
	}

	nextAfter := after
	if len(items) > 0 {
		nextAfter = items[len(items)-1].Sequence
	}
	writeJSON(w, http.StatusOK, auditEventsPage{
		Items:     items,
		NextAfter: nextAfter,
		HasMore:   hasMore,
	})
}

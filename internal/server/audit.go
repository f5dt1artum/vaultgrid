package server

import (
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// auditAction enumerates the recorded state-changing operations. Failures,
// read-only requests and successful requests that change no state are never
// recorded.
type auditAction string

const (
	auditPoolCreated        auditAction = "pool.created"
	auditPoolDeleted        auditAction = "pool.deleted"
	auditReservationCreated auditAction = "reservation.created"
	auditReservationDeleted auditAction = "reservation.deleted"
	auditVolumeCreated      auditAction = "volume.created"
	auditVolumeDeleted      auditAction = "volume.deleted"
	auditVolumeBound        auditAction = "volume.bound"
	auditVolumeUnbound      auditAction = "volume.unbound"
	auditVolumeResized      auditAction = "volume.resized"
	auditVolumeRestored     auditAction = "volume.restored"
	auditSnapshotCreated    auditAction = "snapshot.created"
	auditSnapshotDeleted    auditAction = "snapshot.deleted"
	auditCloneCreated       auditAction = "clone.created"
	auditBucketCreated      auditAction = "bucket.created"
	auditBucketDeleted      auditAction = "bucket.deleted"
	auditObjectCreated      auditAction = "object.created"
	auditObjectOverwritten  auditAction = "object.overwritten"
	auditObjectDeleted      auditAction = "object.deleted"
	auditBucketVersioning   auditAction = "bucket.versioning-enabled"
	auditObjectVersioned    auditAction = "object.version-created"
	auditObjectMarker       auditAction = "object.delete-marker-created"
	auditObjectVersionDel   auditAction = "object.version-deleted"
	auditTenantQuotaCreated auditAction = "tenant-quota.created"
	auditTenantQuotaUpdated auditAction = "tenant-quota.updated"
	auditTenantQuotaDeleted auditAction = "tenant-quota.deleted"
)

// auditEventView is the public representation of an audit event.
type auditEventView struct {
	Sequence   int64  `json:"sequence"`
	Action     string `json:"action"`
	Resource   string `json:"resource"`
	PoolID     string `json:"poolId"`
	BytesDelta int64  `json:"bytesDelta"`
}

// auditEvent is one entry of the in-memory audit log. Events are appended
// while holding the store write lock in the same critical section as the
// business state and capacity counters, so an event and the change it
// describes commit atomically: a committed response is always followed by a
// visible event, and a failure leaves no event. The log lives in memory and
// is wiped on restart.
type auditEvent struct {
	sequence   int64
	action     auditAction
	resource   string
	poolID     string
	bytesDelta int64
}

func (e *auditEvent) view() auditEventView {
	return auditEventView{
		Sequence:   e.sequence,
		Action:     string(e.action),
		Resource:   e.resource,
		PoolID:     e.poolID,
		BytesDelta: e.bytesDelta,
	}
}

// appendAudit assigns the next sequence and appends an event. The caller must
// hold s.mu for writing; numbering inside the commit critical section orders
// concurrent commits by the order in which they actually committed.
func (s *store) appendAudit(action auditAction, resource, poolID string, bytesDelta int64) {
	s.auditSeq++
	s.auditEvents = append(s.auditEvents, auditEvent{
		sequence:   s.auditSeq,
		action:     action,
		resource:   resource,
		poolID:     poolID,
		bytesDelta: bytesDelta,
	})
}

// auditEventsPath is the route constant for the audit events endpoint.
const auditEventsPath = "/v1/audit-events"

// auditEventsQuery is the parsed GET /v1/audit-events query.
type auditEventsQuery struct {
	after   int64
	limit   int
	poolID  string
	hasPool bool
}

// parseAuditEventsQuery accepts an empty query or any combination of the
// "after", "limit" and "poolId" parameters, each given at most once and always
// with a value. Defaults are after=0, limit=100, no pool filter. Duplicated,
// unknown, value-less or malformed parameters yield ok=false.
func parseAuditEventsQuery(raw string) (auditEventsQuery, bool) {
	q := auditEventsQuery{limit: 100}
	if raw == "" {
		return q, true
	}
	seenAfter, seenLimit, seenPool := false, false, false
	for _, pair := range strings.Split(raw, "&") {
		k, value, found := strings.Cut(pair, "=")
		if !found {
			return q, false
		}
		key, err := url.QueryUnescape(k)
		if err != nil {
			return q, false
		}
		decoded, err := url.QueryUnescape(value)
		if err != nil {
			return q, false
		}
		switch key {
		case "after":
			if seenAfter || !isNonNegativeDecimal(decoded) {
				return q, false
			}
			seenAfter = true
			n, err := strconv.ParseInt(decoded, 10, 64)
			if err != nil {
				return q, false
			}
			q.after = n
		case "limit":
			if seenLimit || !isNonNegativeDecimal(decoded) {
				return q, false
			}
			seenLimit = true
			n, err := strconv.ParseInt(decoded, 10, 64)
			if err != nil || n < 1 || n > 1000 {
				return q, false
			}
			q.limit = int(n)
		case "poolId":
			if seenPool || decoded == "" || !validID(decoded) {
				return q, false
			}
			seenPool = true
			q.poolID = decoded
			q.hasPool = true
		default:
			return q, false
		}
	}
	return q, true
}

// isNonNegativeDecimal reports whether s is one or more ASCII decimal digits,
// rejecting signs, spaces and exponent notation.
func isNonNegativeDecimal(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// auditEventsList is the response envelope.
type auditEventsList struct {
	Items     []auditEventView `json:"items"`
	NextAfter int64            `json:"nextAfter"`
	HasMore   bool             `json:"hasMore"`
}

// listAuditEvents handles GET /v1/audit-events. The whole page is selected
// under a single read lock, so items and hasMore describe one consistent read
// state even while other requests commit new events.
func (s *store) listAuditEvents(w http.ResponseWriter, r *http.Request) {
	q, ok := parseAuditEventsQuery(r.URL.RawQuery)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	if q.hasPool {
		if _, exists := s.pools[q.poolID]; !exists {
			writeError(w, http.StatusNotFound, "pool_not_found")
			return
		}
	}

	// Sequences are dense and the slice is append-only, so index order is
	// sequence order. Jump to the first event with sequence > after.
	first := sort.Search(len(s.auditEvents), func(i int) bool {
		return s.auditEvents[i].sequence > q.after
	})
	items := make([]auditEventView, 0, q.limit)
	hasMore := false
	for i := first; i < len(s.auditEvents); i++ {
		e := s.auditEvents[i]
		if q.hasPool && e.poolID != q.poolID {
			continue
		}
		if len(items) >= q.limit {
			// A matching event beyond the page (in this same read state)
			// remains.
			hasMore = true
			break
		}
		items = append(items, e.view())
	}
	nextAfter := q.after
	if len(items) > 0 {
		nextAfter = items[len(items)-1].Sequence
	}
	writeJSON(w, http.StatusOK, auditEventsList{Items: items, NextAfter: nextAfter, HasMore: hasMore})
}

package server

import (
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// capacityReportTotals carries the per-pool metered fields. It is embedded
// in both the item view (which adds poolId) and the summary view, so the
// summary always uses the same field names and order as the items.
type capacityReportTotals struct {
	RawCapacityBytes int64 `json:"rawCapacityBytes"`
	ReservationBytes int64 `json:"reservationBytes"`
	VolumeBytes      int64 `json:"volumeBytes"`
	SnapshotBytes    int64 `json:"snapshotBytes"`
	ObjectBytes      int64 `json:"objectBytes"`
	AllocatedBytes   int64 `json:"allocatedBytes"`
	AvailableBytes   int64 `json:"availableBytes"`
}

// capacityReportItem is one pool's row of the capacity report.
type capacityReportItem struct {
	PoolID string `json:"poolId"`
	capacityReportTotals
}

// capacityReport is the JSON document returned by GET /v1/capacity-report.
type capacityReport struct {
	Summary capacityReportTotals `json:"summary"`
	Items   []capacityReportItem `json:"items"`
}

// capacityReportCSVHeader is the fixed first line of the CSV representation.
const capacityReportCSVHeader = "poolId,rawCapacityBytes,reservationBytes,volumeBytes,snapshotBytes,objectBytes,allocatedBytes,availableBytes"

// routeCapacityReport dispatches /v1/capacity-report. Only GET is allowed.
func (s *store) routeCapacityReport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	s.getCapacityReport(w, r)
}

// reportItem snapshots the pool's metered breakdown. reservationBytes is the
// sum of outstanding reservations; allocatedBytes is the sum of the four
// usage classes and availableBytes is raw capacity minus allocated.
func (p *pool) reportItem() capacityReportItem {
	var reservationBytes int64
	for _, r := range p.reservations {
		reservationBytes += r.bytes
	}
	allocated := reservationBytes + p.volumeBytes + p.snapshotBytes + p.objectBytes
	return capacityReportItem{
		PoolID: p.id,
		capacityReportTotals: capacityReportTotals{
			RawCapacityBytes: p.rawCapacity,
			ReservationBytes: reservationBytes,
			VolumeBytes:      p.volumeBytes,
			SnapshotBytes:    p.snapshotBytes,
			ObjectBytes:      p.objectBytes,
			AllocatedBytes:   allocated,
			AvailableBytes:   p.rawCapacity - allocated,
		},
	}
}

// parseCapacityReportQuery accepts an empty query or one pair each of
// "format=<json|csv>" and "poolId=<id>". Unknown, duplicated or valueless
// parameters yield ok=false. hasPoolID reports whether poolId was given at
// all, so an empty value is rejected rather than treated as absent.
func parseCapacityReportQuery(raw string) (format, poolID string, hasPoolID, ok bool) {
	format = "json"
	if raw == "" {
		return format, "", false, true
	}
	seenFormat := false
	for _, pair := range strings.Split(raw, "&") {
		k, value, found := strings.Cut(pair, "=")
		if !found {
			return "", "", false, false
		}
		decodedKey, err := url.QueryUnescape(k)
		if err != nil {
			return "", "", false, false
		}
		decodedValue, err := url.QueryUnescape(value)
		if err != nil {
			return "", "", false, false
		}
		switch decodedKey {
		case "format":
			if seenFormat {
				return "", "", false, false
			}
			seenFormat = true
			format = decodedValue
		case "poolId":
			if hasPoolID {
				return "", "", false, false
			}
			hasPoolID = true
			poolID = decodedValue
		default:
			return "", "", false, false
		}
	}
	return format, poolID, hasPoolID, true
}

func (s *store) getCapacityReport(w http.ResponseWriter, r *http.Request) {
	format, poolID, hasPoolID, ok := parseCapacityReportQuery(r.URL.RawQuery)
	if !ok || (format != "json" && format != "csv") || (hasPoolID && !validID(poolID)) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}

	// The whole report is assembled under one read lock, so every item and
	// the summary describe the same store state even while writers run.
	s.mu.RLock()
	ids := make([]string, 0, len(s.pools))
	if hasPoolID {
		if _, exists := s.pools[poolID]; !exists {
			s.mu.RUnlock()
			writeError(w, http.StatusNotFound, "pool_not_found")
			return
		}
		ids = append(ids, poolID)
	} else {
		for id := range s.pools {
			ids = append(ids, id)
		}
		sort.Strings(ids)
	}
	items := make([]capacityReportItem, 0, len(ids))
	var summary capacityReportTotals
	for _, id := range ids {
		item := s.pools[id].reportItem()
		items = append(items, item)
		summary.RawCapacityBytes += item.RawCapacityBytes
		summary.ReservationBytes += item.ReservationBytes
		summary.VolumeBytes += item.VolumeBytes
		summary.SnapshotBytes += item.SnapshotBytes
		summary.ObjectBytes += item.ObjectBytes
		summary.AllocatedBytes += item.AllocatedBytes
		summary.AvailableBytes += item.AvailableBytes
	}
	s.mu.RUnlock()

	if format == "csv" {
		writeCapacityReportCSV(w, items)
		return
	}
	writeJSON(w, http.StatusOK, capacityReport{Summary: summary, Items: items})
}

// writeCapacityReportCSV renders the same rows as the JSON items, in the
// same pool-id order, as UTF-8 CSV with decimal integers. Pool ids are
// restricted to [A-Za-z0-9._-], so no field ever needs quoting.
func writeCapacityReportCSV(w http.ResponseWriter, items []capacityReportItem) {
	var b strings.Builder
	b.WriteString(capacityReportCSVHeader)
	b.WriteByte('\n')
	for _, item := range items {
		b.WriteString(item.PoolID)
		b.WriteByte(',')
		b.WriteString(strconv.FormatInt(item.RawCapacityBytes, 10))
		b.WriteByte(',')
		b.WriteString(strconv.FormatInt(item.ReservationBytes, 10))
		b.WriteByte(',')
		b.WriteString(strconv.FormatInt(item.VolumeBytes, 10))
		b.WriteByte(',')
		b.WriteString(strconv.FormatInt(item.SnapshotBytes, 10))
		b.WriteByte(',')
		b.WriteString(strconv.FormatInt(item.ObjectBytes, 10))
		b.WriteByte(',')
		b.WriteString(strconv.FormatInt(item.AllocatedBytes, 10))
		b.WriteByte(',')
		b.WriteString(strconv.FormatInt(item.AvailableBytes, 10))
		b.WriteByte('\n')
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, b.String())
}

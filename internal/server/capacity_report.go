package server

import (
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// capacityReportItem is the per-pool row of the capacity report.
type capacityReportItem struct {
	PoolID           string `json:"poolId"`
	RawCapacityBytes int64  `json:"rawCapacityBytes"`
	ReservationBytes int64  `json:"reservationBytes"`
	VolumeBytes      int64  `json:"volumeBytes"`
	SnapshotBytes    int64  `json:"snapshotBytes"`
	ObjectBytes      int64  `json:"objectBytes"`
	AllocatedBytes   int64  `json:"allocatedBytes"`
	AvailableBytes   int64  `json:"availableBytes"`
}

// capacityReportSummary is the aggregate over every reported pool; it carries
// the same fields as a pool row except poolId.
type capacityReportSummary struct {
	RawCapacityBytes int64 `json:"rawCapacityBytes"`
	ReservationBytes int64 `json:"reservationBytes"`
	VolumeBytes      int64 `json:"volumeBytes"`
	SnapshotBytes    int64 `json:"snapshotBytes"`
	ObjectBytes      int64 `json:"objectBytes"`
	AllocatedBytes   int64 `json:"allocatedBytes"`
	AvailableBytes   int64 `json:"availableBytes"`
}

// csvHeader is the fixed first line of the CSV report. Pool ids are
// restricted to A-Za-z0-9._- so no field ever needs quoting.
const csvHeader = "poolId,rawCapacityBytes,reservationBytes,volumeBytes,snapshotBytes,objectBytes,allocatedBytes,availableBytes"

// parseCapacityReportQuery accepts an empty query or any combination of the
// "format" and "poolId" parameters, each given at most once and always with
// a value. Duplicated, unknown or value-less parameters yield ok=false.
func parseCapacityReportQuery(raw string) (format, poolID string, hasPool, ok bool) {
	format = "json"
	if raw == "" {
		return format, "", false, true
	}
	seenFormat, seenPool := false, false
	for _, pair := range strings.Split(raw, "&") {
		k, value, found := strings.Cut(pair, "=")
		if !found {
			return "", "", false, false
		}
		key, err := url.QueryUnescape(k)
		if err != nil {
			return "", "", false, false
		}
		decoded, err := url.QueryUnescape(value)
		if err != nil {
			return "", "", false, false
		}
		switch key {
		case "format":
			if seenFormat {
				return "", "", false, false
			}
			seenFormat = true
			format = decoded
		case "poolId":
			if seenPool {
				return "", "", false, false
			}
			seenPool = true
			poolID = decoded
		default:
			return "", "", false, false
		}
	}
	return format, poolID, seenPool, true
}

// capacityReport handles GET /v1/capacity-report. The whole report is built
// under a single read lock, so every row and the summary describe one
// consistent state even while other requests mutate the store.
func (s *store) capacityReport(w http.ResponseWriter, r *http.Request) {
	format, poolID, hasPool, ok := parseCapacityReportQuery(r.URL.RawQuery)
	if !ok || (format != "json" && format != "csv") || (hasPool && !validID(poolID)) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}

	s.mu.RLock()
	var ids []string
	if hasPool {
		if _, exists := s.pools[poolID]; !exists {
			s.mu.RUnlock()
			writeError(w, http.StatusNotFound, "pool_not_found")
			return
		}
		ids = []string{poolID}
	} else {
		ids = make([]string, 0, len(s.pools))
		for id := range s.pools {
			ids = append(ids, id)
		}
		sort.Strings(ids)
	}
	items := make([]capacityReportItem, 0, len(ids))
	var summary capacityReportSummary
	for _, id := range ids {
		p := s.pools[id]
		var reservationBytes int64
		for _, res := range p.reservations {
			reservationBytes += res.bytes
		}
		allocated := p.allocated()
		item := capacityReportItem{
			PoolID:           p.id,
			RawCapacityBytes: p.rawCapacity,
			ReservationBytes: reservationBytes,
			VolumeBytes:      p.volumeBytes,
			SnapshotBytes:    p.snapshotBytes,
			ObjectBytes:      p.objectBytes,
			AllocatedBytes:   allocated,
			AvailableBytes:   p.rawCapacity - allocated,
		}
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
		var b strings.Builder
		b.WriteString(csvHeader)
		b.WriteByte('\n')
		for _, it := range items {
			fmt.Fprintf(&b, "%s,%d,%d,%d,%d,%d,%d,%d\n",
				it.PoolID, it.RawCapacityBytes, it.ReservationBytes, it.VolumeBytes,
				it.SnapshotBytes, it.ObjectBytes, it.AllocatedBytes, it.AvailableBytes)
		}
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(b.String()))
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Summary capacityReportSummary `json:"summary"`
		Items   []capacityReportItem  `json:"items"`
	}{Summary: summary, Items: items})
}

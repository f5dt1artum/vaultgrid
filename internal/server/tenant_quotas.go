package server

import (
	"net/http"
	"sort"
)

// tenantHeader is the request header selecting the tenant a new reservation,
// volume or bucket belongs to. At most one value is accepted; when the header
// is absent the resource belongs to the default tenant.
const tenantHeader = "X-Vaultgrid-Tenant"

// defaultTenant is the tenant resources belong to when the request carries
// no tenant header.
const defaultTenant = "default"

// tenantFromHeader extracts the tenant id from the request headers. ok is
// false when the header carries more than one value or a value that is not a
// valid id.
func tenantFromHeader(h http.Header) (tenant string, ok bool) {
	values, present := h[tenantHeader]
	if !present {
		return defaultTenant, true
	}
	if len(values) != 1 || !validID(values[0]) {
		return "", false
	}
	return values[0], true
}

// tenantQuotaInput is the body of a tenant-quota PUT: only a positive
// limitBytes is accepted.
type tenantQuotaInput struct {
	LimitBytes int64 `json:"limitBytes"`
}

// tenantQuotaView is the public representation of a tenant quota.
type tenantQuotaView struct {
	PoolID         string `json:"poolId"`
	TenantID       string `json:"tenantId"`
	LimitBytes     int64  `json:"limitBytes"`
	UsedBytes      int64  `json:"usedBytes"`
	AvailableBytes int64  `json:"availableBytes"`
}

// quotaView renders the quota of tenantID from the pool's current state.
func (p *pool) quotaView(tenantID string, limit int64) tenantQuotaView {
	used := p.tenantUsed[tenantID]
	return tenantQuotaView{
		PoolID:         p.id,
		TenantID:       tenantID,
		LimitBytes:     limit,
		UsedBytes:      used,
		AvailableBytes: limit - used,
	}
}

// putTenantQuota handles PUT /v1/storage-pools/{poolId}/tenant-quotas/{tenantId}.
// A first creation answers 201; a modification or a same-value replay answers
// 200. A limit below the tenant's current usage is rejected.
func (s *store) putTenantQuota(w http.ResponseWriter, r *http.Request, poolID, tenantID string) {
	var in tenantQuotaInput
	if !decodeRequest(r, &in) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if !validID(poolID) || !validID(tenantID) || in.LimitBytes <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.pools[poolID]
	if !ok {
		writeError(w, http.StatusNotFound, "pool_not_found")
		return
	}
	if used := p.tenantUsed[tenantID]; in.LimitBytes < used {
		writeError(w, http.StatusConflict, "quota_below_usage")
		return
	}
	resource := storagePoolPath + "/" + poolID + "/tenant-quotas/" + tenantID
	existing, exists := p.tenantQuotas[tenantID]
	if !exists {
		p.tenantQuotas[tenantID] = in.LimitBytes
		s.appendAudit(auditTenantQuotaCreated, resource, poolID, 0)
		writeJSON(w, http.StatusCreated, p.quotaView(tenantID, in.LimitBytes))
		return
	}
	// A same-value replay changes nothing and records no event.
	if existing != in.LimitBytes {
		p.tenantQuotas[tenantID] = in.LimitBytes
		s.appendAudit(auditTenantQuotaUpdated, resource, poolID, 0)
	}
	writeJSON(w, http.StatusOK, p.quotaView(tenantID, in.LimitBytes))
}

func (s *store) getTenantQuota(w http.ResponseWriter, poolID, tenantID string) {
	if !validID(poolID) || !validID(tenantID) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	s.mu.RLock()
	p, poolOK := s.pools[poolID]
	var view tenantQuotaView
	exists := false
	if poolOK {
		var limit int64
		limit, exists = p.tenantQuotas[tenantID]
		if exists {
			view = p.quotaView(tenantID, limit)
		}
	}
	s.mu.RUnlock()
	if !poolOK {
		writeError(w, http.StatusNotFound, "pool_not_found")
		return
	}
	if !exists {
		writeError(w, http.StatusNotFound, "quota_not_found")
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *store) listTenantQuotas(w http.ResponseWriter, poolID string) {
	if !validID(poolID) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	s.mu.RLock()
	p, ok := s.pools[poolID]
	if !ok {
		s.mu.RUnlock()
		writeError(w, http.StatusNotFound, "pool_not_found")
		return
	}
	ids := make([]string, 0, len(p.tenantQuotas))
	for id := range p.tenantQuotas {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	items := make([]tenantQuotaView, 0, len(ids))
	for _, id := range ids {
		items = append(items, p.quotaView(id, p.tenantQuotas[id]))
	}
	s.mu.RUnlock()
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// deleteTenantQuota handles DELETE on a tenant quota. It always answers 204:
// removing a quota only lifts the limit and never blocks deleting an
// otherwise empty pool. Only a real removal records an audit event.
func (s *store) deleteTenantQuota(w http.ResponseWriter, poolID, tenantID string) {
	if !validID(poolID) || !validID(tenantID) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.pools[poolID]; ok {
		if _, exists := p.tenantQuotas[tenantID]; exists {
			delete(p.tenantQuotas, tenantID)
			s.appendAudit(auditTenantQuotaDeleted,
				storagePoolPath+"/"+poolID+"/tenant-quotas/"+tenantID, poolID, 0)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

package server

import (
	"net/http"
	"sort"
)

// tenantHeader is the request header naming the tenant a new reservation,
// volume or bucket belongs to. It is already in Go's canonical header form.
const tenantHeader = "X-Vaultgrid-Tenant"

// defaultTenant owns every resource created without a tenant header.
const defaultTenant = "default"

// tenantQuotaPath builds the public resource path of one tenant quota.
func tenantQuotaPath(poolID, tenantID string) string {
	return storagePoolPath + "/" + poolID + "/tenant-quotas/" + tenantID
}

// extractTenant reads the optional X-Vaultgrid-Tenant header. Absent means the
// default tenant; more than one value or a value that is not a valid id makes
// the whole request invalid.
func extractTenant(h http.Header) (string, bool) {
	values, present := h[tenantHeader]
	if !present || len(values) == 0 {
		return defaultTenant, true
	}
	if len(values) != 1 || !validID(values[0]) {
		return "", false
	}
	return values[0], true
}

// tenantQuotaExceeded reports whether charging delta more bytes to tenant in
// pool p would exceed the tenant's configured quota. Tenants without a quota
// are unlimited. delta must be positive.
func (p *pool) tenantQuotaExceeded(tenant string, delta int64) bool {
	limit, ok := p.quotas[tenant]
	if !ok {
		return false
	}
	return p.tenantUsed[tenant] > limit-delta
}

// tenantQuotaInput is the body of PUT .../tenant-quotas/{tenantId}.
type tenantQuotaInput struct {
	LimitBytes int64 `json:"limitBytes"`
}

// tenantQuotaView is the public representation of one tenant quota.
type tenantQuotaView struct {
	PoolID         string `json:"poolId"`
	TenantID       string `json:"tenantId"`
	LimitBytes     int64  `json:"limitBytes"`
	UsedBytes      int64  `json:"usedBytes"`
	AvailableBytes int64  `json:"availableBytes"`
}

// quotaView renders the quota of tenant in pool p. The caller must hold s.mu.
func (p *pool) quotaView(tenant string, limit int64) tenantQuotaView {
	used := p.tenantUsed[tenant]
	return tenantQuotaView{
		PoolID:         p.id,
		TenantID:       tenant,
		LimitBytes:     limit,
		UsedBytes:      used,
		AvailableBytes: limit - used,
	}
}

// listTenantQuotas handles GET /v1/storage-pools/{poolId}/tenant-quotas.
func (s *store) listTenantQuotas(w http.ResponseWriter, poolID string) {
	if !validID(poolID) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	s.mu.RLock()
	p, ok := s.pools[poolID]
	var items []tenantQuotaView
	if ok {
		tenants := make([]string, 0, len(p.quotas))
		for tenant := range p.quotas {
			tenants = append(tenants, tenant)
		}
		sort.Strings(tenants)
		items = make([]tenantQuotaView, 0, len(tenants))
		for _, tenant := range tenants {
			items = append(items, p.quotaView(tenant, p.quotas[tenant]))
		}
	}
	s.mu.RUnlock()
	if !ok {
		writeError(w, http.StatusNotFound, "pool_not_found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// getTenantQuota handles GET /v1/storage-pools/{poolId}/tenant-quotas/{tenantId}.
func (s *store) getTenantQuota(w http.ResponseWriter, poolID, tenantID string) {
	if !validID(poolID) || !validID(tenantID) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	s.mu.RLock()
	p, ok := s.pools[poolID]
	var view tenantQuotaView
	exists := false
	if ok {
		var limit int64
		limit, exists = p.quotas[tenantID]
		if exists {
			view = p.quotaView(tenantID, limit)
		}
	}
	s.mu.RUnlock()
	if !ok {
		writeError(w, http.StatusNotFound, "pool_not_found")
		return
	}
	if !exists {
		writeError(w, http.StatusNotFound, "quota_not_found")
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// putTenantQuota handles PUT /v1/storage-pools/{poolId}/tenant-quotas/{tenantId}.
// A first create answers 201; an actual change and a same-value replay both
// answer 200, but only the real change is audited.
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
	if existing, exists := p.quotas[tenantID]; exists && existing == in.LimitBytes {
		// Same-value replay: a successful no-op, so no audit event.
		writeJSON(w, http.StatusOK, p.quotaView(tenantID, in.LimitBytes))
		return
	}
	if used := p.tenantUsed[tenantID]; in.LimitBytes < used {
		writeError(w, http.StatusConflict, "quota_below_usage")
		return
	}
	resource := tenantQuotaPath(poolID, tenantID)
	if _, exists := p.quotas[tenantID]; exists {
		p.quotas[tenantID] = in.LimitBytes
		s.appendAudit(auditTenantQuotaUpdated, resource, poolID, 0)
		writeJSON(w, http.StatusOK, p.quotaView(tenantID, in.LimitBytes))
		return
	}
	p.quotas[tenantID] = in.LimitBytes
	s.appendAudit(auditTenantQuotaCreated, resource, poolID, 0)
	writeJSON(w, http.StatusCreated, p.quotaView(tenantID, in.LimitBytes))
}

// deleteTenantQuota handles DELETE .../tenant-quotas/{tenantId}. Deleting an
// unknown or already-deleted quota is a success with no state change, so only
// a real deletion records an event. The quota only lifts the tenant's limit;
// it never blocks deleting the (otherwise empty) pool.
func (s *store) deleteTenantQuota(w http.ResponseWriter, poolID, tenantID string) {
	if !validID(poolID) || !validID(tenantID) {
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
	if _, existed := p.quotas[tenantID]; existed {
		delete(p.quotas, tenantID)
		s.appendAudit(auditTenantQuotaDeleted, tenantQuotaPath(poolID, tenantID), poolID, 0)
	}
	w.WriteHeader(http.StatusNoContent)
}

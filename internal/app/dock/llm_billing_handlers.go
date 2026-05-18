package dock

// LLM billing (B1) HTTP layer.
//
// Tenant-side:
//   - GET  /api/llm-configs/marketplace     — browse shared configs with pricing + own team's budget/spend
//   - GET  /api/llm-config-applications     — list this workspace's applications
//   - POST /api/llm-config-applications     — apply for access to a shared config (auto-approve at default budget)
//
// Admin-side:
//   - PUT  /api/admin/llm-configs/:id/pricing            — set per-1M-token prices
//   - GET  /api/admin/llm-config-team-quotas             — list all (workspace, config) budgets
//   - PUT  /api/admin/llm-config-team-quotas             — upsert a (workspace, config) budget
//   - GET  /api/admin/llm-config-applications            — list all applications across workspaces

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

// GET /api/llm-configs/marketplace — every workspace member can browse the
// list of shared LLM configs available across the platform, plus
// pricing and (if applicable) their own team's budget + month-to-date
// spend. The UI uses this for the "marketplace" view.
type sharedLLMConfigDTO struct {
	ID                       int64    `json:"id"`
	Name                     string   `json:"name"`
	Model                    string   `json:"model"`
	IsPlatform               bool     `json:"is_platform"`
	OwnedByMyWorkspace       bool     `json:"owned_by_my_workspace"`
	PricePerMillionInputUSD  *float64 `json:"price_per_million_input_usd,omitempty"`
	PricePerMillionOutputUSD *float64 `json:"price_per_million_output_usd,omitempty"`
	MyMonthlyBudgetUSD       *float64 `json:"my_monthly_budget_usd,omitempty"`
	MyMonthSpendUSD          *float64 `json:"my_month_spend_usd,omitempty"`
	MyApplicationStatus      string   `json:"my_application_status,omitempty"`
}

func (s *Server) handleSharedLLMConfigList(c *gin.Context) {
	workspaceID := strings.TrimSpace(c.GetString("workspace_id"))
	if workspaceID == "" {
		jsonError(c, http.StatusBadRequest, "workspace.required")
		return
	}

	rows, err := s.db.Query(
		`SELECT id, COALESCE(workspace_id, ''), name, model, is_platform, price_per_million_input_usd, price_per_million_output_usd
		   FROM llm_configs
		  WHERE (shared = TRUE OR is_platform = TRUE OR workspace_id = $1)
		    AND (provider_kind = 'text' OR provider_kind = '' OR provider_kind IS NULL)
		  ORDER BY (workspace_id = $1) DESC, is_platform DESC, name ASC`,
		workspaceID,
	)
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "llm_configs.list_failed")
		return
	}
	defer rows.Close()

	out := make([]sharedLLMConfigDTO, 0)
	for rows.Next() {
		var dto sharedLLMConfigDTO
		var ownerWorkspaceID string
		var inUSD, outUSD sql.NullFloat64
		if scanErr := rows.Scan(&dto.ID, &ownerWorkspaceID, &dto.Name, &dto.Model, &dto.IsPlatform, &inUSD, &outUSD); scanErr != nil {
			jsonError(c, http.StatusInternalServerError, "llm_configs.scan_failed")
			return
		}
		dto.OwnedByMyWorkspace = ownerWorkspaceID != "" && ownerWorkspaceID == workspaceID
		if inUSD.Valid {
			v := inUSD.Float64
			dto.PricePerMillionInputUSD = &v
		}
		if outUSD.Valid {
			v := outUSD.Float64
			dto.PricePerMillionOutputUSD = &v
		}
		// Spend is queried unconditionally so a workspace can see its
		// month-to-date consumption even before applying for a budget.
		// llm_proxy_calls.cost_usd is written on every call (B1 P6
		// cost ledger), so this is real data — Root + non-applied
		// teams alike.
		if sp, spErr := s.teamLLMConfigMonthSpend(workspaceID, dto.ID); spErr == nil {
			dto.MyMonthSpendUSD = &sp
		}
		if q, qErr := s.getLLMConfigTeamQuota(dto.ID, workspaceID); qErr == nil && q != nil {
			b := q.MonthlyBudgetUSD
			dto.MyMonthlyBudgetUSD = &b
		}
		out = append(out, dto)
	}

	if apps, aErr := s.listLLMConfigApplications(workspaceID); aErr == nil {
		latest := map[int64]string{}
		for _, a := range apps {
			if _, ok := latest[a.LLMConfigID]; !ok {
				latest[a.LLMConfigID] = a.Status
			}
		}
		for i := range out {
			if st, ok := latest[out[i].ID]; ok {
				out[i].MyApplicationStatus = st
			}
		}
	}

	c.JSON(http.StatusOK, gin.H{"items": out})
}

// GET /api/llm-config-applications — workspace's own application
// history (member-visible).
func (s *Server) handleLLMConfigApplicationsListMine(c *gin.Context) {
	workspaceID := strings.TrimSpace(c.GetString("workspace_id"))
	if workspaceID == "" {
		jsonError(c, http.StatusBadRequest, "workspace.required")
		return
	}
	items, err := s.listLLMConfigApplications(workspaceID)
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "llm_applications.list_failed")
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

// POST /api/llm-config-applications — apply for access to a shared
// LLM config. Body: { "llm_config_id": 12, "note": "..." }
//
// Auto-approve policy (B1): every application is approved immediately
// at the platform default budget (LLM_TEAM_DEFAULT_MONTHLY_BUDGET_USD,
// default $10). The application row is still recorded for audit; admins
// can later raise/lower the budget via PUT /admin/llm-config-team-quotas.
type llmConfigApplyRequest struct {
	LLMConfigID int64  `json:"llm_config_id"`
	Note        string `json:"note"`
}

func (s *Server) handleLLMConfigApply(c *gin.Context) {
	workspaceID := strings.TrimSpace(c.GetString("workspace_id"))
	userID := strings.TrimSpace(c.GetString("user_id"))
	if workspaceID == "" || userID == "" {
		jsonError(c, http.StatusBadRequest, "workspace_or_user.required")
		return
	}

	var req llmConfigApplyRequest
	if err := c.BindJSON(&req); err != nil {
		jsonError(c, http.StatusBadRequest, "invalid_json")
		return
	}
	if req.LLMConfigID <= 0 {
		jsonError(c, http.StatusBadRequest, "llm_config_id.required")
		return
	}

	var shared, isPlatform bool
	if err := s.db.QueryRow(
		`SELECT shared, is_platform FROM llm_configs WHERE id = $1`,
		req.LLMConfigID,
	).Scan(&shared, &isPlatform); err != nil {
		jsonError(c, http.StatusNotFound, "llm_config.not_found")
		return
	}
	if !(shared || isPlatform) {
		jsonError(c, http.StatusForbidden, "llm_config.not_shared")
		return
	}

	app, err := s.createLLMConfigApplication(req.LLMConfigID, workspaceID, userID, req.Note)
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "llm_application.create_failed")
		return
	}

	budget := llmTeamDefaultMonthlyBudget()
	if approveErr := s.approveLLMConfigApplication(app.ID, userID, budget); approveErr != nil {
		jsonError(c, http.StatusInternalServerError, "llm_application.approve_failed")
		return
	}
	if _, qErr := s.upsertLLMConfigTeamQuota(req.LLMConfigID, workspaceID, budget); qErr != nil {
		jsonError(c, http.StatusInternalServerError, "llm_team_quota.upsert_failed")
		return
	}

	app.Status = "approved"
	app.GrantedBudgetUSD = budget
	c.JSON(http.StatusOK, gin.H{"application": app, "granted_budget_usd": budget})
}

// PUT /api/admin/llm-configs/:id/pricing — body:
//
//	{ "price_per_million_input_usd": 0.5, "price_per_million_output_usd": 1.5 }
//
// Either field may be null to clear the override (falls back to the
// hardcoded prefix table).
type llmConfigPricingRequest struct {
	PricePerMillionInputUSD  *float64 `json:"price_per_million_input_usd"`
	PricePerMillionOutputUSD *float64 `json:"price_per_million_output_usd"`
}

func (s *Server) handleAdminLLMConfigPricingUpdate(c *gin.Context) {
	id, parseErr := strconv.ParseInt(c.Param("id"), 10, 64)
	if parseErr != nil || id <= 0 {
		jsonError(c, http.StatusBadRequest, "id.invalid")
		return
	}
	var req llmConfigPricingRequest
	if err := c.BindJSON(&req); err != nil {
		jsonError(c, http.StatusBadRequest, "invalid_json")
		return
	}
	if err := s.setLLMConfigPricing(id, req.PricePerMillionInputUSD, req.PricePerMillionOutputUSD); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			jsonError(c, http.StatusNotFound, "llm_config.not_found")
			return
		}
		jsonError(c, http.StatusInternalServerError, "llm_config.pricing_update_failed")
		return
	}
	p, _ := s.getLLMConfigPricing(id)
	c.JSON(http.StatusOK, gin.H{"pricing": p})
}

// GET /api/admin/llm-config-team-quotas — all (workspace, config)
// budgets. Optional ?workspace_id=... filter.
func (s *Server) handleAdminLLMConfigTeamQuotaList(c *gin.Context) {
	workspaceID := strings.TrimSpace(c.Query("workspace_id"))
	items, err := s.listLLMConfigTeamQuotas(workspaceID)
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "llm_team_quotas.list_failed")
		return
	}
	for i := range items {
		if sp, spErr := s.teamLLMConfigMonthSpend(items[i].WorkspaceID, items[i].LLMConfigID); spErr == nil {
			items[i].MonthSpendUSD = sp
		}
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

// PUT /api/admin/llm-config-team-quotas — body:
//
//	{ "llm_config_id": 12, "workspace_id": "team-abc", "monthly_budget_usd": 50 }
type llmConfigTeamQuotaUpsertRequest struct {
	LLMConfigID      int64   `json:"llm_config_id"`
	WorkspaceID      string  `json:"workspace_id"`
	MonthlyBudgetUSD float64 `json:"monthly_budget_usd"`
}

func (s *Server) handleAdminLLMConfigTeamQuotaUpsert(c *gin.Context) {
	var req llmConfigTeamQuotaUpsertRequest
	if err := c.BindJSON(&req); err != nil {
		jsonError(c, http.StatusBadRequest, "invalid_json")
		return
	}
	if req.LLMConfigID <= 0 || strings.TrimSpace(req.WorkspaceID) == "" {
		jsonError(c, http.StatusBadRequest, "llm_config_id_or_workspace_id.required")
		return
	}
	q, err := s.upsertLLMConfigTeamQuota(req.LLMConfigID, req.WorkspaceID, req.MonthlyBudgetUSD)
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "llm_team_quota.upsert_failed")
		return
	}
	c.JSON(http.StatusOK, gin.H{"quota": q})
}

// GET /api/admin/llm-config-applications — all applications across
// workspaces, newest first.
func (s *Server) handleAdminLLMConfigApplicationsList(c *gin.Context) {
	items, err := s.listLLMConfigApplications("")
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "llm_applications.list_failed")
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

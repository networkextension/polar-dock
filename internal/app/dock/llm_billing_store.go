package dock

// LLM billing (B1) — typed CRUD for pricing, per-team budgets, and
// application records. See schema block at the end of store.go.
//
// Three concerns layered on top of the Phase 1 LLM proxy:
//   - Pricing: admin sets price_per_million_{input,output}_usd on a
//     shared llm_config; the chat handler uses it to compute cost_usd
//     for each call (falling back to the hardcoded prefix-match table
//     in llm_proxy_pricing.go when unset).
//   - Per-team quota: an llm_config_team_quotas row caps how much a
//     given workspace can spend against a given shared config per
//     UTC month. Chat handler 402s when month spend ≥ budget.
//   - Applications: an llm_config_applications row records that
//     workspace W asked for access to config C. Auto-approve creates
//     the matching team_quota row at admin default budget.

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ---- pricing ----

// LLMConfigPricing is the admin-set per-1M-token prices for a shared
// llm_config. NULL columns mean "no override" — fall back to the
// hardcoded llmProxyPriceTable lookup.
type LLMConfigPricing struct {
	LLMConfigID            int64
	PricePerMillionInput   *float64
	PricePerMillionOutput  *float64
}

func (s *Server) getLLMConfigPricing(llmConfigID int64) (*LLMConfigPricing, error) {
	var inUSD, outUSD sql.NullFloat64
	err := s.db.QueryRow(
		`SELECT price_per_million_input_usd, price_per_million_output_usd
		   FROM llm_configs WHERE id = $1`,
		llmConfigID,
	).Scan(&inUSD, &outUSD)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	p := &LLMConfigPricing{LLMConfigID: llmConfigID}
	if inUSD.Valid {
		v := inUSD.Float64
		p.PricePerMillionInput = &v
	}
	if outUSD.Valid {
		v := outUSD.Float64
		p.PricePerMillionOutput = &v
	}
	return p, nil
}

func (s *Server) setLLMConfigPricing(llmConfigID int64, inputUSD, outputUSD *float64) error {
	var inSQL, outSQL any
	if inputUSD != nil {
		inSQL = *inputUSD
	}
	if outputUSD != nil {
		outSQL = *outputUSD
	}
	res, err := s.db.Exec(
		`UPDATE llm_configs
		    SET price_per_million_input_usd = $2,
		        price_per_million_output_usd = $3,
		        updated_at = now()
		  WHERE id = $1`,
		llmConfigID, inSQL, outSQL,
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// ---- per-team quotas ----

type LLMConfigTeamQuota struct {
	ID                int64     `json:"id"`
	LLMConfigID       int64     `json:"llm_config_id"`
	WorkspaceID       string    `json:"workspace_id"`
	MonthlyBudgetUSD  float64   `json:"monthly_budget_usd"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
	// MonthSpendUSD is populated by getLLMConfigTeamQuotaWithSpend;
	// not stored in the table (computed from llm_proxy_calls).
	MonthSpendUSD float64 `json:"month_spend_usd"`
}

// upsertLLMConfigTeamQuota sets the monthly budget for (config, team).
// Used by the apply auto-approve path AND admin manual edits.
func (s *Server) upsertLLMConfigTeamQuota(llmConfigID int64, workspaceID string, monthlyBudgetUSD float64) (*LLMConfigTeamQuota, error) {
	workspaceID = strings.TrimSpace(workspaceID)
	if workspaceID == "" {
		return nil, errors.New("workspace_id required")
	}
	if monthlyBudgetUSD < 0 {
		monthlyBudgetUSD = 0
	}
	var id int64
	var createdAt, updatedAt time.Time
	err := s.db.QueryRow(
		`INSERT INTO llm_config_team_quotas (llm_config_id, workspace_id, monthly_budget_usd)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (llm_config_id, workspace_id)
		 DO UPDATE SET monthly_budget_usd = EXCLUDED.monthly_budget_usd, updated_at = now()
		 RETURNING id, created_at, updated_at`,
		llmConfigID, workspaceID, monthlyBudgetUSD,
	).Scan(&id, &createdAt, &updatedAt)
	if err != nil {
		return nil, err
	}
	return &LLMConfigTeamQuota{
		ID:               id,
		LLMConfigID:      llmConfigID,
		WorkspaceID:      workspaceID,
		MonthlyBudgetUSD: monthlyBudgetUSD,
		CreatedAt:        createdAt,
		UpdatedAt:        updatedAt,
	}, nil
}

// getLLMConfigTeamQuota returns (quota, nil) when set or (nil, nil)
// when no row exists (i.e. team hasn't applied for this config).
func (s *Server) getLLMConfigTeamQuota(llmConfigID int64, workspaceID string) (*LLMConfigTeamQuota, error) {
	var q LLMConfigTeamQuota
	err := s.db.QueryRow(
		`SELECT id, llm_config_id, workspace_id, monthly_budget_usd, created_at, updated_at
		   FROM llm_config_team_quotas
		  WHERE llm_config_id = $1 AND workspace_id = $2`,
		llmConfigID, workspaceID,
	).Scan(&q.ID, &q.LLMConfigID, &q.WorkspaceID, &q.MonthlyBudgetUSD, &q.CreatedAt, &q.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &q, nil
}

// listLLMConfigTeamQuotas — admin view, all (config, team) budgets.
// Workspace filter optional (pass "" to see all).
func (s *Server) listLLMConfigTeamQuotas(workspaceID string) ([]LLMConfigTeamQuota, error) {
	var rows *sql.Rows
	var err error
	if workspaceID == "" {
		rows, err = s.db.Query(
			`SELECT id, llm_config_id, workspace_id, monthly_budget_usd, created_at, updated_at
			   FROM llm_config_team_quotas
			  ORDER BY workspace_id, llm_config_id`,
		)
	} else {
		rows, err = s.db.Query(
			`SELECT id, llm_config_id, workspace_id, monthly_budget_usd, created_at, updated_at
			   FROM llm_config_team_quotas
			  WHERE workspace_id = $1
			  ORDER BY llm_config_id`,
			workspaceID,
		)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]LLMConfigTeamQuota, 0)
	for rows.Next() {
		var q LLMConfigTeamQuota
		if err := rows.Scan(&q.ID, &q.LLMConfigID, &q.WorkspaceID, &q.MonthlyBudgetUSD, &q.CreatedAt, &q.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

// teamLLMConfigMonthSpend returns SUM(cost_usd) for (workspace,
// llm_config_id) in the current UTC month. Used by the chat handler's
// team-quota check.
func (s *Server) teamLLMConfigMonthSpend(workspaceID string, llmConfigID int64) (float64, error) {
	var v float64
	err := s.db.QueryRow(
		`SELECT COALESCE(SUM(cost_usd), 0)::FLOAT8
		   FROM llm_proxy_calls
		  WHERE workspace_id = $1
		    AND llm_config_id = $2
		    AND created_at >= date_trunc('month', NOW())`,
		workspaceID, llmConfigID,
	).Scan(&v)
	return v, err
}

// ---- applications ----

type LLMConfigApplication struct {
	ID                  int64      `json:"id"`
	LLMConfigID         int64      `json:"llm_config_id"`
	WorkspaceID         string     `json:"workspace_id"`
	RequestedByUserID   string     `json:"requested_by_user_id"`
	Status              string     `json:"status"`
	Note                string     `json:"note"`
	AppliedAt           time.Time  `json:"applied_at"`
	DecidedAt           *time.Time `json:"decided_at,omitempty"`
	DecidedByUserID     *string    `json:"decided_by_user_id,omitempty"`
	GrantedBudgetUSD    float64    `json:"granted_budget_usd"`
}

func (s *Server) createLLMConfigApplication(llmConfigID int64, workspaceID, requestedByUserID, note string) (*LLMConfigApplication, error) {
	workspaceID = strings.TrimSpace(workspaceID)
	requestedByUserID = strings.TrimSpace(requestedByUserID)
	if workspaceID == "" || requestedByUserID == "" {
		return nil, errors.New("workspace_id + requested_by_user_id required")
	}
	var id int64
	var appliedAt time.Time
	err := s.db.QueryRow(
		`INSERT INTO llm_config_applications (llm_config_id, workspace_id, requested_by_user_id, note)
		 VALUES ($1, $2, $3, $4)
		 RETURNING id, applied_at`,
		llmConfigID, workspaceID, requestedByUserID, strings.TrimSpace(note),
	).Scan(&id, &appliedAt)
	if err != nil {
		return nil, err
	}
	return &LLMConfigApplication{
		ID:                id,
		LLMConfigID:       llmConfigID,
		WorkspaceID:       workspaceID,
		RequestedByUserID: requestedByUserID,
		Status:            "pending",
		Note:              strings.TrimSpace(note),
		AppliedAt:         appliedAt,
	}, nil
}

// approveLLMConfigApplication is the auto-approve path: marks the
// row approved + records the granted budget. Caller MUST also call
// upsertLLMConfigTeamQuota to make the budget take effect.
func (s *Server) approveLLMConfigApplication(id int64, decidedByUserID string, grantedBudgetUSD float64) error {
	res, err := s.db.Exec(
		`UPDATE llm_config_applications
		    SET status = 'approved',
		        decided_at = now(),
		        decided_by_user_id = $2,
		        granted_budget_usd = $3
		  WHERE id = $1 AND status = 'pending'`,
		id, decidedByUserID, grantedBudgetUSD,
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("application %d not in pending state", id)
	}
	return nil
}

func (s *Server) listLLMConfigApplications(workspaceID string) ([]LLMConfigApplication, error) {
	var rows *sql.Rows
	var err error
	if workspaceID == "" {
		rows, err = s.db.Query(
			`SELECT id, llm_config_id, workspace_id, requested_by_user_id, status, note,
			        applied_at, decided_at, decided_by_user_id, granted_budget_usd
			   FROM llm_config_applications
			  ORDER BY applied_at DESC`,
		)
	} else {
		rows, err = s.db.Query(
			`SELECT id, llm_config_id, workspace_id, requested_by_user_id, status, note,
			        applied_at, decided_at, decided_by_user_id, granted_budget_usd
			   FROM llm_config_applications
			  WHERE workspace_id = $1
			  ORDER BY applied_at DESC`,
			workspaceID,
		)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]LLMConfigApplication, 0)
	for rows.Next() {
		var a LLMConfigApplication
		var decidedAt sql.NullTime
		var decidedBy sql.NullString
		if err := rows.Scan(&a.ID, &a.LLMConfigID, &a.WorkspaceID, &a.RequestedByUserID,
			&a.Status, &a.Note, &a.AppliedAt, &decidedAt, &decidedBy, &a.GrantedBudgetUSD); err != nil {
			return nil, err
		}
		if decidedAt.Valid {
			v := decidedAt.Time
			a.DecidedAt = &v
		}
		if decidedBy.Valid {
			v := decidedBy.String
			a.DecidedByUserID = &v
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ---- recharges / credit balance (B4) ----

type LLMConfigTeamRecharge struct {
	ID              int64     `json:"id"`
	LLMConfigID     int64     `json:"llm_config_id"`
	WorkspaceID     string    `json:"workspace_id"`
	AmountUSD       float64   `json:"amount_usd"`
	Note            string    `json:"note"`
	CreatedByUserID string    `json:"created_by_user_id"`
	CreatedAt       time.Time `json:"created_at"`
}

func (s *Server) createLLMConfigTeamRecharge(llmConfigID int64, workspaceID, note, createdByUserID string, amountUSD float64) (*LLMConfigTeamRecharge, error) {
	workspaceID = strings.TrimSpace(workspaceID)
	createdByUserID = strings.TrimSpace(createdByUserID)
	if workspaceID == "" || createdByUserID == "" {
		return nil, errors.New("workspace_id + created_by_user_id required")
	}
	var id int64
	var createdAt time.Time
	err := s.db.QueryRow(
		`INSERT INTO llm_config_team_recharges (llm_config_id, workspace_id, amount_usd, note, created_by_user_id)
		 VALUES ($1, $2, $3, $4, $5)
		 RETURNING id, created_at`,
		llmConfigID, workspaceID, amountUSD, strings.TrimSpace(note), createdByUserID,
	).Scan(&id, &createdAt)
	if err != nil {
		return nil, err
	}
	return &LLMConfigTeamRecharge{
		ID:              id,
		LLMConfigID:     llmConfigID,
		WorkspaceID:     workspaceID,
		AmountUSD:       amountUSD,
		Note:            strings.TrimSpace(note),
		CreatedByUserID: createdByUserID,
		CreatedAt:       createdAt,
	}, nil
}

// teamLLMConfigBalance — current prepaid balance = SUM(recharges) -
// SUM(all-time cost_usd) for (workspace, config). 0 if no rows of
// either kind. Negative = team owes (chat handler rejects).
func (s *Server) teamLLMConfigBalance(workspaceID string, llmConfigID int64) (float64, error) {
	var v float64
	err := s.db.QueryRow(
		`SELECT
		   COALESCE((SELECT SUM(amount_usd) FROM llm_config_team_recharges
		              WHERE workspace_id = $1 AND llm_config_id = $2), 0)::FLOAT8
		   -
		   COALESCE((SELECT SUM(cost_usd) FROM llm_proxy_calls
		              WHERE workspace_id = $1 AND llm_config_id = $2), 0)::FLOAT8`,
		workspaceID, llmConfigID,
	).Scan(&v)
	return v, err
}

// teamHasLLMConfigRecharge reports whether at least one recharge
// row exists. Used as the gate trigger — no recharge history = no
// credit-balance enforcement (back-compat with B1 no-quota free-use).
func (s *Server) teamHasLLMConfigRecharge(workspaceID string, llmConfigID int64) (bool, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM llm_config_team_recharges
		  WHERE workspace_id = $1 AND llm_config_id = $2`,
		workspaceID, llmConfigID,
	).Scan(&n)
	return n > 0, err
}

func (s *Server) listLLMConfigTeamRecharges(workspaceID string, llmConfigID int64) ([]LLMConfigTeamRecharge, error) {
	rows, err := s.db.Query(
		`SELECT id, llm_config_id, workspace_id, amount_usd, note, COALESCE(created_by_user_id, ''), created_at
		   FROM llm_config_team_recharges
		  WHERE workspace_id = $1 AND llm_config_id = $2
		  ORDER BY created_at DESC`,
		workspaceID, llmConfigID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]LLMConfigTeamRecharge, 0)
	for rows.Next() {
		var r LLMConfigTeamRecharge
		if scanErr := rows.Scan(&r.ID, &r.LLMConfigID, &r.WorkspaceID, &r.AmountUSD, &r.Note, &r.CreatedByUserID, &r.CreatedAt); scanErr != nil {
			return nil, scanErr
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// llmTeamDefaultMonthlyBudget returns the auto-approve grant amount
// (env-overridable). Defaults to $10/month so first-time teams get
// a usable allowance without admin intervention.
func llmTeamDefaultMonthlyBudget() float64 {
	if n := envIntDefault("LLM_TEAM_DEFAULT_MONTHLY_BUDGET_USD", 10); n > 0 {
		return float64(n)
	}
	return 10
}

// checkChatBillingGate mirrors the dual gate in llm_proxy_chat_handlers.go
// (4b monthly_budget + 4c credit_balance) for the in-app chat path.
// Returns (blocked, userMessage). Absent quota row + absent recharge =
// back-compat free use, matching the proxy. Caller is the chat task
// handler in ai_agent.go; user message goes straight into the failed
// bot message bubble (no openAIError equivalent here — it's a chat).
func (s *Server) checkChatBillingGate(workspaceID string, llmConfigID int64) (bool, string) {
	if workspaceID == "" || llmConfigID <= 0 {
		return false, ""
	}
	if quota, qErr := s.getLLMConfigTeamQuota(llmConfigID, workspaceID); qErr == nil && quota != nil && quota.MonthlyBudgetUSD > 0 {
		spend, _ := s.teamLLMConfigMonthSpend(workspaceID, llmConfigID)
		if spend >= quota.MonthlyBudgetUSD {
			return true, fmt.Sprintf("本月该 LLM 的团队额度已用完（$%.2f / $%.2f），请联系管理员上调或等待下个月重置。", spend, quota.MonthlyBudgetUSD)
		}
	}
	if hasRecharge, _ := s.teamHasLLMConfigRecharge(workspaceID, llmConfigID); hasRecharge {
		balance, _ := s.teamLLMConfigBalance(workspaceID, llmConfigID)
		if balance <= 0 {
			return true, fmt.Sprintf("该 LLM 的团队充值余额已耗尽（当前 $%.2f），请联系管理员充值后再试。", balance)
		}
	}
	return false, ""
}

// ensureLLMConfigTeamQuota: if (workspace, config) has no quota row,
// create one at the platform default budget. Idempotent — no-op when
// a row already exists (preserves the admin's manual budget edits).
// Used by the expense extract flow so a workspace's first AI call
// auto-applies; mirrors handleLLMConfigApply's auto-approve behavior
// without the application audit trail (silent first-touch).
func (s *Server) ensureLLMConfigTeamQuota(workspaceID string, llmConfigID int64) error {
	workspaceID = strings.TrimSpace(workspaceID)
	if workspaceID == "" || llmConfigID <= 0 {
		return nil
	}
	if q, err := s.getLLMConfigTeamQuota(llmConfigID, workspaceID); err == nil && q != nil {
		return nil // already present; respect existing budget
	}
	_, err := s.db.Exec(
		`INSERT INTO llm_config_team_quotas (llm_config_id, workspace_id, monthly_budget_usd)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (llm_config_id, workspace_id) DO NOTHING`,
		llmConfigID, workspaceID, llmTeamDefaultMonthlyBudget(),
	)
	return err
}

package dock

// L1 HTTP handlers for the expense module. Workspace-scoped via
// requireWorkspaceID; auth via AuthMiddleware (every workspace member
// has read+write — no admin gate, this is a household ledger).
//
// OCR / LLM extraction lives in expense_extract.go (L2 — separate PR).

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// ---- categories ----

func (s *Server) handleExpenseCategoryList(c *gin.Context) {
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	// First read for a workspace seeds the 7 presets so the user never
	// sees an empty dropdown.
	items, err := s.listExpenseCategories(workspaceID, true)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	if len(items) == 0 {
		if err := s.bootstrapExpenseCategories(workspaceID); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "无法初始化默认分类"})
			return
		}
		items, err = s.listExpenseCategories(workspaceID, true)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
			return
		}
	}
	c.JSON(http.StatusOK, gin.H{"categories": items})
}

func (s *Server) handleExpenseCategoryCreate(c *gin.Context) {
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	var req struct {
		Name      string `json:"name" binding:"required"`
		Icon      string `json:"icon"`
		Color     string `json:"color"`
		SortOrder int    `json:"sort_order"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的输入"})
		return
	}
	cat, err := s.createExpenseCategory(workspaceID, req.Name, req.Icon, req.Color, req.SortOrder)
	if err != nil {
		// Most likely a UNIQUE-violation on (workspace_id, name).
		if strings.Contains(strings.ToLower(err.Error()), "duplicate") || strings.Contains(err.Error(), "unique") {
			c.JSON(http.StatusConflict, gin.H{"error": "分类名已存在"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"category": cat})
}

func (s *Server) handleExpenseCategoryUpdate(c *gin.Context) {
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	id, ok := parseInt64Param(c, "id")
	if !ok {
		return
	}
	var req struct {
		Name      *string `json:"name"`
		Icon      *string `json:"icon"`
		Color     *string `json:"color"`
		SortOrder *int    `json:"sort_order"`
		IsHidden  *bool   `json:"is_hidden"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的输入"})
		return
	}
	if err := s.updateExpenseCategory(id, workspaceID, ExpenseCategoryUpdate{
		Name: req.Name, Icon: req.Icon, Color: req.Color,
		SortOrder: req.SortOrder, IsHidden: req.IsHidden,
	}); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (s *Server) handleExpenseCategoryDelete(c *gin.Context) {
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	id, ok := parseInt64Param(c, "id")
	if !ok {
		return
	}
	// ON DELETE SET NULL on expenses.category_id keeps the flow rows alive.
	if err := s.deleteExpenseCategory(id, workspaceID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ---- expenses ----

func (s *Server) handleExpenseList(c *gin.Context) {
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	filter := ExpenseListFilter{}
	if v := c.Query("start"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			filter.Start = &t
		} else if t, err := time.Parse("2006-01-02", v); err == nil {
			filter.Start = &t
		}
	}
	if v := c.Query("end"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			filter.End = &t
		} else if t, err := time.Parse("2006-01-02", v); err == nil {
			// End is half-open; bump to next day so 2026-05-31 includes
			// transactions on that day.
			t = t.Add(24 * time.Hour)
			filter.End = &t
		}
	}
	if v := c.Query("category_id"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			filter.CategoryID = &n
		}
	}
	if v := c.Query("status"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			filter.Status = &n
		}
	}
	if v := c.Query("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			filter.Limit = n
		}
	}
	if v := c.Query("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			filter.Offset = n
		}
	}
	items, total, err := s.listExpenses(workspaceID, filter)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"expenses": items,
		"total":    total,
	})
}

func (s *Server) handleExpenseCreate(c *gin.Context) {
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	userIDAny, _ := c.Get("user_id")
	userID, _ := userIDAny.(string)
	if strings.TrimSpace(userID) == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "未登录"})
		return
	}
	var req struct {
		Amount        float64 `json:"amount" binding:"required"`
		Currency      string  `json:"currency"`
		Merchant      string  `json:"merchant"`
		CategoryID    *int64  `json:"category_id"`
		ConsumeTime   string  `json:"consume_time"` // RFC3339 or YYYY-MM-DD
		HasDetailTime bool    `json:"has_detail_time"`
		Region        string  `json:"region"`
		Remark        string  `json:"remark"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的输入"})
		return
	}
	if req.Amount <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "金额必须大于 0"})
		return
	}
	e := &Expense{
		WorkspaceID:     workspaceID,
		CreatedByUserID: userID,
		Amount:          req.Amount,
		Currency:        req.Currency,
		Merchant:        strings.TrimSpace(req.Merchant),
		CategoryID:      req.CategoryID,
		HasDetailTime:   req.HasDetailTime,
		Region:          strings.TrimSpace(req.Region),
		Status:          ExpenseStatusConfirmed, // manual entry = trusted
		Remark:          strings.TrimSpace(req.Remark),
	}
	if req.ConsumeTime != "" {
		if t, err := time.Parse(time.RFC3339, req.ConsumeTime); err == nil {
			e.ConsumeTime = t
		} else if t, err := time.Parse("2006-01-02", req.ConsumeTime); err == nil {
			e.ConsumeTime = t
			e.HasDetailTime = false // date-only input
		}
	}
	if err := s.createExpense(e); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"expense": e})
}

func (s *Server) handleExpenseUpdate(c *gin.Context) {
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	id, ok := parseInt64Param(c, "id")
	if !ok {
		return
	}
	var req struct {
		Amount        *float64 `json:"amount"`
		Currency      *string  `json:"currency"`
		Merchant      *string  `json:"merchant"`
		CategoryID    *int64   `json:"category_id"`
		ClearCategory bool     `json:"clear_category"` // explicit clear vs leave-alone
		ConsumeTime   *string  `json:"consume_time"`
		HasDetailTime *bool    `json:"has_detail_time"`
		Region        *string  `json:"region"`
		Status        *int     `json:"status"`
		Remark        *string  `json:"remark"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的输入"})
		return
	}
	u := ExpenseUpdate{
		Amount:        req.Amount,
		Currency:      req.Currency,
		Merchant:      req.Merchant,
		CategoryID:    req.CategoryID,
		ClearCategory: req.ClearCategory,
		HasDetailTime: req.HasDetailTime,
		Region:        req.Region,
		Status:        req.Status,
		Remark:        req.Remark,
	}
	if req.ConsumeTime != nil {
		if t, err := time.Parse(time.RFC3339, *req.ConsumeTime); err == nil {
			u.ConsumeTime = &t
		} else if t, err := time.Parse("2006-01-02", *req.ConsumeTime); err == nil {
			u.ConsumeTime = &t
		}
	}
	if err := s.updateExpense(id, workspaceID, u); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (s *Server) handleExpenseDelete(c *gin.Context) {
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	id, ok := parseInt64Param(c, "id")
	if !ok {
		return
	}
	if err := s.deleteExpense(id, workspaceID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

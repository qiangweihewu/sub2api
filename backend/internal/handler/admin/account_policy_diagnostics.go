package admin

import (
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// SetUpstreamPolicySettingService attaches the optional settings reader used by
// the read-only upstream policy diagnostics endpoint.
func (h *AccountHandler) SetUpstreamPolicySettingService(settingService *service.SettingService) {
	h.upstreamPolicySettings = settingService
}

// GetUpstreamPolicyDiagnostics reports the runtime gates and resolver result
// for an account without making an upstream request or mutating data.
// GET /admin/accounts/:id/upstream-policy[?group_id=<positive id>]
func (h *AccountHandler) GetUpstreamPolicyDiagnostics(c *gin.Context) {
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || accountID <= 0 {
		response.BadRequest(c, "Invalid account ID")
		return
	}
	account, err := h.adminService.GetAccount(c.Request.Context(), accountID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	if account == nil {
		response.NotFound(c, "Account not found")
		return
	}
	var group *service.Group
	if rawGroupID := c.Query("group_id"); rawGroupID != "" {
		groupID, parseErr := strconv.ParseInt(rawGroupID, 10, 64)
		if parseErr != nil || groupID <= 0 {
			response.BadRequest(c, "Invalid group ID")
			return
		}
		group, err = h.adminService.GetGroup(c.Request.Context(), groupID)
		if err != nil {
			response.ErrorFrom(c, err)
			return
		}
		if group == nil {
			response.NotFound(c, "Group not found")
			return
		}
		if !accountBelongsToGroup(account, groupID) {
			response.BadRequest(c, "Group is not bound to account")
			return
		}
	}
	response.Success(c, service.ExplainUpstreamPassthroughPolicy(c.Request.Context(), account, group, h.upstreamPolicySettings))
}

func accountBelongsToGroup(account *service.Account, groupID int64) bool {
	if account == nil || groupID <= 0 {
		return false
	}
	for _, id := range account.GroupIDs {
		if id == groupID {
			return true
		}
	}
	for _, binding := range account.AccountGroups {
		if binding.GroupID == groupID {
			return true
		}
	}
	for _, group := range account.Groups {
		if group != nil && group.ID == groupID {
			return true
		}
	}
	return false
}

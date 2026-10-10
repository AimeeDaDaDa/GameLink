package admin

import (
	"strings"

	"github.com/gin-gonic/gin"

	"gamelink/internal/handler/middleware"
	"gamelink/internal/model"
	"gamelink/internal/service/orderentry"
	"gamelink/pkg/apierr"
)

// OrderEntryHandler 录单接口（参考 delta-settlement）。
type OrderEntryHandler struct {
	svc *orderentry.Service
}

// NewOrderEntryHandler 创建录单 Handler。
func NewOrderEntryHandler(svc *orderentry.Service) *OrderEntryHandler {
	return &OrderEntryHandler{svc: svc}
}

// RegisterOrderEntryRoutes 注册录单路由。
func RegisterOrderEntryRoutes(router gin.IRouter, h *OrderEntryHandler, pm *middleware.PermissionMiddleware) {
	router.GET("/order-entry/options", pm.RequirePermission(model.HTTPMethodGET, "/api/v1/admin/order-entry/options"), h.Options)
	router.POST("/order-entry/quote", pm.RequirePermission(model.HTTPMethodPOST, "/api/v1/admin/order-entry/quote"), h.Quote)
	router.GET("/order-entries/:id", pm.RequirePermission(model.HTTPMethodGET, "/api/v1/admin/order-entries/:id"), h.Get)
	router.POST("/order-entries", pm.RequirePermission(model.HTTPMethodPOST, "/api/v1/admin/order-entries"), h.Save)
	router.POST("/order-entries/:id/submit", pm.RequirePermission(model.HTTPMethodPOST, "/api/v1/admin/order-entries/:id/submit"), h.Submit)
	router.POST("/order-entries/:id/void", pm.RequirePermission(model.HTTPMethodPOST, "/api/v1/admin/order-entries/:id/void"), h.Void)
}

// Options 返回录单可用功能、类型与默认规则。
func (h *OrderEntryHandler) Options(c *gin.Context) {
	respondSuccess(c, gin.H{
		"platforms":  []string{string(model.OrderPlatformPC), string(model.OrderPlatformMobile)},
		"order_types": []string{string(model.OrderEntryTypeNormal)}, // P1 仅开放普通单
		"collection_modes": []string{
			string(model.CollectionModeUncollected),
			string(model.CollectionModeManual),
		},
		"default_rates": gin.H{
			"pc":     1500, // 端游 15%
			"mobile": 1000, // 手游 10%
		},
		"rule_version": orderentry.RuleVersion,
	})
}

// Quote 计算录单报价（不动资金）。
func (h *OrderEntryHandler) Quote(c *gin.Context) {
	var in orderentry.QuoteInput
	if err := c.ShouldBindJSON(&in); err != nil {
		respondAPIError(c, apierr.BadRequest("invalid request payload").WithDetails(err.Error()))
		return
	}
	out, err := orderentry.Calculate(in)
	if err != nil {
		respondAPIError(c, err)
		return
	}
	respondSuccess(c, out)
}

// Save 保存录单（草稿或提交）。
func (h *OrderEntryHandler) Save(c *gin.Context) {
	var in orderentry.SaveInput
	if err := c.ShouldBindJSON(&in); err != nil {
		respondAPIError(c, apierr.BadRequest("invalid request payload").WithDetails(err.Error()))
		return
	}
	in.OperatorID = c.GetUint64("user_id")
	if in.OperatorID == 0 {
		respondAPIError(c, apierr.Unauthorized("缺少操作者身份"))
		return
	}
	in.Action = strings.ToLower(strings.TrimSpace(in.Action))
	in.Platform = strings.ToLower(strings.TrimSpace(in.Platform))
	in.OrderType = strings.ToLower(strings.TrimSpace(in.OrderType))
	in.CollectionMode = strings.ToLower(strings.TrimSpace(in.CollectionMode))

	res, err := h.svc.Save(c.Request.Context(), in, c.GetHeader("Idempotency-Key"))
	if err != nil {
		respondAPIError(c, err)
		return
	}
	if in.Action == "submit" {
		respondCreated(c, gin.H{"entry": res.Entry, "quote": res.Quote, "order": res.Order})
		return
	}
	respondCreated(c, gin.H{"entry": res.Entry, "quote": res.Quote})
}

// Get 获取录单详情。
func (h *OrderEntryHandler) Get(c *gin.Context) {
	id, ok := ParseIDAndRespond(c, "id")
	if !ok {
		return
	}
	entry, err := h.svc.Get(c.Request.Context(), id)
	if err != nil {
		respondAPIError(c, err)
		return
	}
	respondSuccess(c, entry)
}

// Submit 提交草稿（建立真实订单）。
func (h *OrderEntryHandler) Submit(c *gin.Context) {
	_, ok := ParseIDAndRespond(c, "id")
	if !ok {
		return
	}
	respondAPIError(c, apierr.BadRequest("请在保存时使用 action=submit 提交，草稿二次提交接口待实现"))
}

// Void 作废未入账录单。
func (h *OrderEntryHandler) Void(c *gin.Context) {
	id, ok := ParseIDAndRespond(c, "id")
	if !ok {
		return
	}
	var p struct {
		Reason string `json:"reason"`
	}
	_ = c.ShouldBindJSON(&p)
	operatorID := c.GetUint64("user_id")
	if err := h.svc.Void(c.Request.Context(), id, operatorID, p.Reason); err != nil {
		respondAPIError(c, err)
		return
	}
	respondSuccess(c, gin.H{"status": "voided"})
}

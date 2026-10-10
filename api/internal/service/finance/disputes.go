package finance

import (
	"context"
	"gamelink/internal/model"
	"gamelink/pkg/apierr"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"strings"
	"time"
)

func (s *Service) OpenDispute(ctx context.Context, d *model.OrderDispute) error {
	return s.transaction(ctx, func(tx *gorm.DB) error {
		var o model.Order
		if err := lock(tx).First(&o, d.OrderID).Error; err != nil {
			return err
		}
		switch d.InitiatorType {
		case model.DisputeInitiatorUser:
			if o.UserID != d.InitiatorID {
				return apierr.Forbidden("无权发起纠纷")
			}
		case model.DisputeInitiatorPlayer:
			var p model.Player
			if err := tx.Where("user_id = ?", d.InitiatorID).First(&p).Error; err != nil {
				return err
			}
			if o.GetPlayerID() != p.ID {
				return apierr.Forbidden("无权发起纠纷")
			}
		default:
			return apierr.BadRequest("发起人类型无效")
		}
		if strings.TrimSpace(d.Reason) == "" || !model.CanInitiateDispute(&o) || (o.Status != model.OrderStatusCompleted && o.Status != model.OrderStatusInProgress) {
			return apierr.BadRequest("当前订单不可发起售后纠纷")
		}
		var count int64
		if err := tx.Model(&model.OrderDispute{}).Where("order_id = ? AND status IN ?", o.ID, []model.DisputeStatus{model.DisputeStatusPending, model.DisputeStatusAssigned, model.DisputeStatusMediating}).Count(&count).Error; err != nil {
			return err
		}
		if count > 0 {
			return apierr.Conflict("仍有未解决的纠纷")
		}
		d.ID = 0
		d.Status = model.DisputeStatusPending
		if err := tx.Omit(clause.Associations).Create(d).Error; err != nil {
			return err
		}
		if err := tx.Model(&o).Updates(map[string]any{"has_dispute": true, "status": model.OrderStatusDisputed}).Error; err != nil {
			return err
		}
		return tx.Model(&model.CommissionRecord{}).Where("order_id = ? AND settlement_status = ?", o.ID, model.SettlementStatusPending).Update("settlement_status", model.SettlementStatusDisputed).Error
	})
}

func (s *Service) ResolveDispute(ctx context.Context, id, adminID uint64, resolution model.DisputeResolution, remark string) error {
	if adminID == 0 || strings.TrimSpace(remark) == "" {
		return apierr.BadRequest("处理人和处理说明必填")
	}
	return s.transaction(ctx, func(tx *gorm.DB) error {
		var d model.OrderDispute
		if err := lock(tx).First(&d, id).Error; err != nil {
			return err
		}
		if d.Status == model.DisputeStatusResolved || d.Status == model.DisputeStatusRejected || d.Status == model.DisputeStatusCanceled {
			return apierr.Conflict("纠纷已经处理")
		}
		var o model.Order
		if err := lock(tx).First(&o, d.OrderID).Error; err != nil {
			return err
		}
		switch resolution {
		case model.ResolutionReject:
			d.Status = model.DisputeStatusRejected
		case model.ResolutionRefund, model.ResolutionPartial:
			var count int64
			if err := tx.Model(&model.RefundRecord{}).Where("order_id = ? AND status = ?", o.ID, model.RefundStatusProcessed).Count(&count).Error; err != nil {
				return err
			}
			if count == 0 || o.RefundAmountCents <= 0 || (resolution == model.ResolutionRefund && o.RefundAmountCents != o.TotalPriceCents) {
				return apierr.BadRequest("请先完成并登记实际退款")
			}
			d.Status = model.DisputeStatusResolved
		default:
			return apierr.BadRequest("请通过单独的转单流程处理重新指派")
		}
		now := time.Now()
		d.Resolution = resolution
		d.ResolveRemark = remark
		d.ResolvedAt = &now
		d.ResolvedBy = &adminID
		if err := tx.Omit(clause.Associations).Save(&d).Error; err != nil {
			return err
		}
		o.HasDispute = false
		if o.Status != model.OrderStatusRefunded {
			if o.CompletedAt != nil {
				o.Status = model.OrderStatusCompleted
			} else {
				o.Status = model.OrderStatusInProgress
			}
		}
		if err := tx.Omit(clause.Associations).Save(&o).Error; err != nil {
			return err
		}
		return tx.Model(&model.CommissionRecord{}).Where("order_id = ? AND settlement_status = ?", o.ID, model.SettlementStatusDisputed).Update("settlement_status", model.SettlementStatusPending).Error
	})
}

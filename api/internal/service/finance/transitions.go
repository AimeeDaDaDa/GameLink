package finance

import (
	"context"
	"gamelink/internal/model"
	"gamelink/pkg/apierr"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"time"
)

// TransitionOrder serializes service start/cancel against payment and refund.
func (s *Service) TransitionOrder(ctx context.Context, id, actor uint64, role string, target model.OrderStatus, reason string) (*model.Order, error) {
	var result model.Order
	err := s.transaction(ctx, func(tx *gorm.DB) error {
		var o model.Order
		if err := lock(tx).First(&o, id).Error; err != nil {
			return err
		}
		if role == "user" && o.UserID != actor {
			return apierr.Forbidden("无权操作订单")
		}
		if role == "player" {
			var p model.Player
			if err := tx.Where("user_id = ?", actor).First(&p).Error; err != nil {
				return err
			}
			if o.GetPlayerID() != 0 && o.GetPlayerID() != p.ID {
				return apierr.Forbidden("无权操作订单")
			}
			if o.GetPlayerID() == 0 && target == model.OrderStatusInProgress {
				o.PlayerID = &p.ID
			}
		}
		if target == o.Status {
			result = o
			return nil
		}
		switch target {
		case model.OrderStatusInProgress:
			if o.Status != model.OrderStatusConfirmed {
				return apierr.BadRequest("只能开始已支付并确认的订单")
			}
			var count int64
			if err := tx.Model(&model.Payment{}).Where("order_id = ? AND status = ? AND refunded_amount_cents = 0 AND amount_cents = ?", o.ID, model.PaymentStatusPaid, o.TotalPriceCents).Count(&count).Error; err != nil {
				return err
			}
			if count != 1 {
				return apierr.BadRequest("缺少匹配的收款记录")
			}
			now := time.Now()
			o.StartedAt = &now
		case model.OrderStatusCanceled:
			if o.Status != model.OrderStatusPending {
				return apierr.BadRequest("已支付订单需通过退款流程取消")
			}
			var count int64
			if err := tx.Model(&model.Payment{}).Where("order_id = ? AND status = ?", o.ID, model.PaymentStatusPaid).Count(&count).Error; err != nil {
				return err
			}
			if count > 0 {
				return apierr.Conflict("订单已收款，请退款")
			}
			if err := tx.Model(&model.Payment{}).Where("order_id = ? AND status = ?", o.ID, model.PaymentStatusPending).Update("status", model.PaymentStatusFailed).Error; err != nil {
				return err
			}
			o.CancelReason = reason
		default:
			return apierr.BadRequest("此状态须通过核账、结算或退款接口修改")
		}
		o.Status = target
		if err := tx.Omit(clause.Associations).Save(&o).Error; err != nil {
			return err
		}
		result = o
		return nil
	})
	return &result, err
}

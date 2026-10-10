package finance

import (
	"context"
	"errors"
	"fmt"
	"gamelink/internal/model"
	"gamelink/pkg/apierr"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"strings"
	"time"
)

func (s *Service) PayWallet(ctx context.Context, userID, orderID uint64, requestID string) (*model.Payment, error) {
	if len(requestID) > 64 {
		return nil, apierr.BadRequest("请求编号过长")
	}
	var result model.Payment
	err := s.transaction(ctx, func(tx *gorm.DB) error {
		var order model.Order
		if err := lock(tx).First(&order, orderID).Error; err != nil {
			return err
		}
		if order.UserID != userID {
			return apierr.Forbidden("无权支付此订单")
		}
		if requestID != "" {
			var existing model.Payment
			err := tx.Where("request_id = ?", requestID).First(&existing).Error
			if err == nil {
				if existing.UserID != userID || existing.OrderID != orderID || existing.Method != model.PaymentMethodWallet {
					return apierr.Conflict("请求编号已用于其他支付")
				}
				if existing.Status == model.PaymentStatusPaid {
					result = existing
					return nil
				}
				return apierr.Conflict("此请求编号已经处理，请查询原支付记录")
			}
			if !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
		}
		if order.Status != model.OrderStatusPending || order.TotalPriceCents <= 0 || order.Currency != model.CurrencyCNY {
			return apierr.BadRequest("订单不可支付")
		}
		var count int64
		if err := tx.Model(&model.Payment{}).Where("order_id = ? AND status IN ?", orderID, []model.PaymentStatus{model.PaymentStatusPending, model.PaymentStatusPaid}).Count(&count).Error; err != nil {
			return err
		}
		if count > 0 {
			return apierr.Conflict("该订单已有支付记录")
		}
		now := time.Now()
		payment := model.Payment{RequestID: requestID, OrderID: orderID, UserID: userID, Method: model.PaymentMethodWallet, AmountCents: order.TotalPriceCents, WalletAmountCents: order.TotalPriceCents, Currency: order.Currency, Status: model.PaymentStatusPaid, PaidAt: &now, VerifiedAt: &now}
		if err := tx.Create(&payment).Error; err != nil {
			return err
		}
		payment.ProviderTradeNo = fmt.Sprintf("wallet_%d", payment.ID)
		if err := move(tx, userID, fmt.Sprintf("payment:%d", payment.ID), -payment.AmountCents, 0, 0); err != nil {
			return err
		}
		order.Status = model.OrderStatusConfirmed
		if err := tx.Omit(clause.Associations).Save(&order).Error; err != nil {
			return err
		}
		if err := tx.Omit(clause.Associations).Save(&payment).Error; err != nil {
			return err
		}
		result = payment
		return nil
	})
	if err == nil {
		var order model.Order
		if e := s.db.WithContext(ctx).First(&order, orderID).Error; e != nil {
			return nil, e
		}
		if order.IsGiftOrder() {
			if _, e := s.CompleteOrder(ctx, orderID, userID, "user"); e != nil {
				return &result, e
			}
		}
	}
	return &result, err
}

// CaptureManual records an administrator's reconciliation of an actual receipt.
// It never invokes a gateway or manufactures a receipt number.
func (s *Service) CaptureManual(ctx context.Context, paymentID uint64, tradeNo string) (*model.Payment, error) {
	tradeNo = strings.TrimSpace(tradeNo)
	if tradeNo == "" || len(tradeNo) > 128 || strings.HasPrefix(tradeNo, "mock") {
		return nil, apierr.BadRequest("请提供真实收款流水号")
	}
	var result model.Payment
	err := s.transaction(ctx, func(tx *gorm.DB) error {
		var p model.Payment
		if err := lock(tx).First(&p, paymentID).Error; err != nil {
			return err
		}
		if p.Status == model.PaymentStatusPaid && p.ProviderTradeNo == tradeNo && p.VerifiedAt != nil {
			result = p
			return nil
		}
		if p.Status != model.PaymentStatusPending || p.Method == model.PaymentMethodWallet || p.Method == model.PaymentMethodCombined {
			return apierr.BadRequest("此支付不能人工确认")
		}
		var order model.Order
		if err := lock(tx).First(&order, p.OrderID).Error; err != nil {
			return err
		}
		if order.Status != model.OrderStatusPending || order.UserID != p.UserID || order.TotalPriceCents != p.AmountCents || order.Currency != p.Currency || p.AmountCents <= 0 {
			return apierr.BadRequest("收款与订单不匹配")
		}
		var count int64
		if err := tx.Model(&model.Payment{}).Where("id <> ? AND (provider_trade_no = ? OR (order_id = ? AND status = ?))", p.ID, tradeNo, p.OrderID, model.PaymentStatusPaid).Count(&count).Error; err != nil {
			return err
		}
		if count > 0 {
			return apierr.Conflict("流水号或订单已入账")
		}
		now := time.Now()
		p.Status = model.PaymentStatusPaid
		p.ProviderTradeNo = tradeNo
		p.PaidAt = &now
		p.VerifiedAt = &now
		p.ThirdPartyAmountCents = p.AmountCents
		if err := tx.Omit(clause.Associations).Save(&p).Error; err != nil {
			return err
		}
		if err := tx.Model(&order).Update("status", model.OrderStatusConfirmed).Error; err != nil {
			return err
		}
		result = p
		return nil
	})
	return &result, err
}

// RefundWallet reverses the customer debit and the corresponding net income.
// External refunds require a verified gateway adapter and are refused here.
func (s *Service) refund(ctx context.Context, paymentID uint64, amount int64, reason string, operatorID *uint64, externalReceipt string, requestIDs ...string) (*model.RefundResponse, error) {
	if amount <= 0 || strings.TrimSpace(reason) == "" {
		return nil, apierr.BadRequest("退款金额和原因必填")
	}
	var response model.RefundResponse
	requestID := ""
	if len(requestIDs) > 0 {
		requestID = requestIDs[0]
	}
	if len(requestID) > 128 {
		return nil, apierr.BadRequest("退款请求编号过长")
	}
	err := s.transaction(ctx, func(tx *gorm.DB) error {
		var p model.Payment
		if err := lock(tx).First(&p, paymentID).Error; err != nil {
			return err
		}
		key := requestID
		if key == "" {
			if amount != p.RemainingRefundableAmount() {
				return apierr.BadRequest("部分退款必须提供唯一请求编号")
			}
			key = fmt.Sprintf("full-refund:%d", p.ID)
		}
		var previous model.RefundRecord
		e := tx.Where("request_id = ?", key).First(&previous).Error
		if e == nil {
			if previous.PaymentID != p.ID || previous.AmountCents != amount || (externalReceipt != "" && previous.ProviderTradeNo != externalReceipt) {
				return apierr.Conflict("退款编号已用于其他请求")
			}
			response = model.RefundResponse{RefundRecord: &previous, Payment: &p, RemainingAmount: p.RemainingRefundableAmount()}
			return nil
		}
		if !errors.Is(e, gorm.ErrRecordNotFound) {
			return e
		}
		if p.Method != model.PaymentMethodWallet && externalReceipt == "" {
			return ErrGatewayUnavailable
		}
		if p.VerifiedAt == nil {
			return apierr.Conflict("历史支付未核实，请先对账")
		}
		if externalReceipt != "" && (p.Method != model.PaymentMethodWeChat && p.Method != model.PaymentMethodAlipay) {
			return apierr.BadRequest("仅第三方收款可登记外部退款")
		}
		if externalReceipt != "" && (len(externalReceipt) > 128 || strings.HasPrefix(externalReceipt, "mock") || p.ProviderTradeNo == "" || strings.HasPrefix(p.ProviderTradeNo, "mock")) {
			return apierr.BadRequest("请提供真实收款和退款流水，历史模拟记录不能核销")
		}
		if p.Method == model.PaymentMethodWallet && p.WalletAmountCents != p.AmountCents {
			return apierr.Conflict("历史支付缺少钱包扣款凭证，请先对账")
		}
		if err := p.ValidateRefundAmount(amount); err != nil {
			return apierr.BadRequest(err.Error())
		}
		if p.Method == model.PaymentMethodWallet {
			ok, err := hasMovement(tx, fmt.Sprintf("payment:%d", p.ID))
			if err != nil {
				return err
			}
			if !ok {
				return apierr.Conflict("缺少原钱包扣款流水，请先对账")
			}
		}
		var order model.Order
		if err := lock(tx).First(&order, p.OrderID).Error; err != nil {
			return err
		}
		refund := model.RefundRecord{RequestID: key, ProviderTradeNo: externalReceipt, PaymentID: p.ID, OrderID: p.OrderID, UserID: p.UserID, AmountCents: amount, Reason: reason, Status: model.RefundStatusProcessed, OperatorID: operatorID}
		if err := tx.Create(&refund).Error; err != nil {
			return err
		}
		var record model.CommissionRecord
		err := lock(tx).Where("order_id = ?", order.ID).First(&record).Error
		if err == nil {
			ok, e := hasMovement(tx, fmt.Sprintf("commission:%d", record.ID))
			if e != nil {
				return e
			}
			if !ok {
				return apierr.Conflict("历史收益缺少流水，退款需人工对账")
			}
			if record.TotalAmountCents != p.AmountCents-p.RefundedAmountCents {
				return apierr.Conflict("收益与剩余订单金额不符")
			}
			remaining := record.TotalAmountCents - amount
			newCommission := percentage(remaining, record.CommissionRate)
			newIncome := remaining - newCommission
			reverse := record.PlayerIncomeCents - newIncome
			var player model.Player
			if err := tx.First(&player, record.PlayerID).Error; err != nil {
				return err
			}
			b, f := int64(0), -reverse
			if record.SettlementStatus == model.SettlementStatusSettled {
				b, f = -reverse, 0
			}
			if err := move(tx, player.UserID, fmt.Sprintf("refund-income:%d", refund.ID), b, f, 0); err != nil {
				return err
			}
			record.TotalAmountCents = remaining
			record.CommissionCents = newCommission
			record.PlayerIncomeCents = newIncome
			if remaining == 0 {
				record.SettlementStatus = "canceled"
			}
			if err := tx.Save(&record).Error; err != nil {
				return err
			}
			order.CommissionCents = newCommission
			order.PlayerIncomeCents = newIncome
			// Pending statements can be rebuilt. Preserve confirmed/paid statements:
			// BuildMonth will report the difference for manual reconciliation.
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if p.Method == model.PaymentMethodWallet {
			if err := move(tx, p.UserID, fmt.Sprintf("refund:%d", refund.ID), amount, 0, 0); err != nil {
				return err
			}
		}
		now := time.Now()
		refund.RefundedAt = &now
		if p.Method == model.PaymentMethodWallet {
			refund.ProviderTradeNo = fmt.Sprintf("wallet_refund_%d", refund.ID)
		}
		p.RefundedAmountCents += amount
		if p.RefundedAmountCents == p.AmountCents {
			p.Status = model.PaymentStatusRefunded
			p.RefundedAt = &now
			order.Status = model.OrderStatusRefunded
		}
		order.RefundAmountCents = p.RefundedAmountCents
		order.RefundReason = reason
		order.RefundedAt = &now
		if err := tx.Omit(clause.Associations).Save(&p).Error; err != nil {
			return err
		}
		if err := tx.Omit(clause.Associations).Save(&order).Error; err != nil {
			return err
		}
		if err := tx.Save(&refund).Error; err != nil {
			return err
		}
		response = model.RefundResponse{RefundRecord: &refund, Payment: &p, RemainingAmount: p.RemainingRefundableAmount()}
		return nil
	})
	return &response, err
}

func (s *Service) RefundWallet(ctx context.Context, id uint64, amount int64, reason string, operator *uint64, requestIDs ...string) (*model.RefundResponse, error) {
	return s.refund(ctx, id, amount, reason, operator, "", requestIDs...)
}

// RefundManual reconciles a refund already made outside GameLink. No wallet
// credit is created for money returned by the external payment channel.
func (s *Service) RefundManual(ctx context.Context, id uint64, amount int64, reason string, operator *uint64, requestID, receipt string) (*model.RefundResponse, error) {
	receipt = strings.TrimSpace(receipt)
	if requestID == "" || receipt == "" {
		return nil, apierr.BadRequest("请填写退款请求编号及实际退款流水")
	}
	return s.refund(ctx, id, amount, reason, operator, receipt, requestID)
}

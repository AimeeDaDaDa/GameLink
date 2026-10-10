// Package finance owns transactional accounting for order income and payroll.
package finance

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"gamelink/internal/model"
	"gamelink/internal/repository"
	commissionrepo "gamelink/internal/repository/commission"
	"gamelink/pkg/apierr"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var ErrGatewayUnavailable = apierr.New(503, "支付渠道尚未完成真实接入与验签，不能自动收款或退款")

type Service struct{ db *gorm.DB }

func New(db *gorm.DB) *Service { return &Service{db: db} }

// Retry whole transactions, never individual accounting writes.
func (s *Service) transaction(ctx context.Context, fn func(*gorm.DB) error) error {
	for attempt := 0; attempt < 8; attempt++ {
		err := s.db.WithContext(ctx).Transaction(fn)
		if err == nil {
			return nil
		}
		message := strings.ToLower(err.Error())
		if !(strings.Contains(message, "database is locked") || strings.Contains(message, "sqlite_busy") || strings.Contains(message, "deadlock detected") || strings.Contains(message, "could not serialize access")) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 10 * time.Millisecond):
		}
	}
	return apierr.Conflict("账务并发冲突，请使用相同请求编号重试")
}

func lock(tx *gorm.DB) *gorm.DB { return tx.Clauses(clause.Locking{Strength: "UPDATE"}) }

func move(tx *gorm.DB, userID uint64, key string, balance, frozen, withdraw int64) error {
	var wallet model.Wallet
	err := lock(tx).Where("user_id = ?", userID).First(&wallet).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		wallet.UserID = userID
		if err = tx.Create(&wallet).Error; err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	add := func(current, delta int64) (int64, error) {
		if (delta > 0 && current > math.MaxInt64-delta) || (delta < 0 && current < -delta) {
			return 0, apierr.BadRequest("余额不足或金额溢出")
		}
		return current + delta, nil
	}
	b, err := add(wallet.BalanceCents, balance)
	if err != nil {
		return err
	}
	f, err := add(wallet.FrozenCents, frozen)
	if err != nil {
		return err
	}
	w, err := add(wallet.WithdrawFrozenCents, withdraw)
	if err != nil {
		return err
	}
	result := tx.Model(&model.Wallet{}).Where("id = ? AND version = ?", wallet.ID, wallet.Version).Updates(map[string]any{
		"balance_cents": b, "frozen_cents": f, "withdraw_frozen_cents": w, "version": wallet.Version + 1,
	})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return apierr.Conflict("钱包已更新，请重试")
	}
	return tx.Create(&model.WalletLedger{Key: key, UserID: userID, BalanceDelta: balance, FrozenDelta: frozen, WithdrawDelta: withdraw, BalanceAfter: b, FrozenAfter: f, WithdrawAfter: w}).Error
}

func hasMovement(tx *gorm.DB, key string) (bool, error) {
	var count int64
	err := tx.Model(&model.WalletLedger{}).Where("key = ?", key).Count(&count).Error
	return count > 0, err
}

// commissionRate uses the real service item and configured rule, preserving the
// lowest-rate policy. Missing data is an error; a database outage is not a discount.
func commissionRate(ctx context.Context, tx *gorm.DB, order *model.Order) (int, error) {
	rates := []int{}
	if order.ItemID != 0 {
		var item model.ServiceItem
		if err := tx.First(&item, order.ItemID).Error; err != nil {
			return 0, err
		}
		if math.IsNaN(item.CommissionRate) || item.CommissionRate < 0 || item.CommissionRate > 1 {
			return 0, apierr.BadRequest("服务项目抽成比例无效")
		}
		rates = append(rates, int(math.Round(item.CommissionRate*100)))
	}
	rule, err := commissionrepo.NewCommissionRepository(tx).GetRuleForOrder(ctx, order.GameID, order.PlayerID, nil)
	if err == nil {
		rates = append(rates, rule.Rate)
	} else if !errors.Is(err, repository.ErrNotFound) {
		return 0, err
	}
	if len(rates) == 0 {
		return 0, apierr.BadRequest("请先配置佣金规则")
	}
	rate := rates[0]
	for _, r := range rates {
		if r < 0 || r > 100 {
			return 0, apierr.BadRequest("佣金比例无效")
		}
		if r < rate {
			rate = r
		}
	}
	return rate, nil
}

func percentage(amount int64, rate int) int64 {
	return amount/100*int64(rate) + amount%100*int64(rate)/100
}

func (s *Service) CompleteOrder(ctx context.Context, orderID uint64, actorID uint64, role string) (*model.Order, error) {
	var result model.Order
	err := s.transaction(ctx, func(tx *gorm.DB) error {
		var order model.Order
		if err := lock(tx).First(&order, orderID).Error; err != nil {
			return err
		}
		if role == "user" && order.UserID != actorID {
			return apierr.Forbidden("无权操作订单")
		}
		if role == "player" {
			var player model.Player
			if err := tx.Where("user_id = ?", actorID).First(&player).Error; err != nil {
				return err
			}
			if order.GetPlayerID() != player.ID {
				return apierr.Forbidden("无权操作订单")
			}
		}
		if order.RequiredPlayers > 1 {
			return apierr.BadRequest("多人订单尚未定义分账比例，不能结算")
		}
		if order.Status != model.OrderStatusInProgress && !(role == "admin" && order.Status == model.OrderStatusConfirmed) && order.Status != model.OrderStatusCompleted && !order.IsGiftOrder() {
			return apierr.BadRequest("订单状态不能完成")
		}
		if order.IsGiftOrder() && order.Status != model.OrderStatusConfirmed && order.Status != model.OrderStatusCompleted {
			return apierr.BadRequest("礼物尚未支付")
		}
		if order.TotalPriceCents <= 0 || order.PlayerID == nil {
			return apierr.BadRequest("订单金额或陪玩师无效")
		}
		var payment model.Payment
		if err := tx.Where("order_id = ? AND status = ?", orderID, model.PaymentStatusPaid).First(&payment).Error; err != nil {
			return apierr.BadRequest("订单没有已核实的收款记录")
		}
		if payment.VerifiedAt == nil || payment.UserID != order.UserID || payment.AmountCents != order.TotalPriceCents || payment.RefundedAmountCents != 0 || payment.Currency != order.Currency {
			return apierr.BadRequest("收款金额与订单不一致")
		}
		var record model.CommissionRecord
		err := tx.Where("order_id = ?", orderID).First(&record).Error
		if err == nil {
			ok, e := hasMovement(tx, fmt.Sprintf("commission:%d", record.ID))
			if e != nil {
				return e
			}
			if !ok {
				return apierr.Conflict("历史佣金缺少钱包流水，需要人工对账")
			}
			result = order
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		rate, err := commissionRate(ctx, tx, &order)
		if err != nil {
			return err
		}
		now := time.Now()
		if order.CompletedAt == nil {
			order.CompletedAt = &now
		}
		order.Status = model.OrderStatusCompleted
		order.CommissionCents = percentage(order.TotalPriceCents, rate)
		order.PlayerIncomeCents = order.TotalPriceCents - order.CommissionCents
		if order.IsGiftOrder() {
			order.DeliveredAt = &now
		}
		record = model.CommissionRecord{OrderID: order.ID, PlayerID: *order.PlayerID, TotalAmountCents: order.TotalPriceCents, CommissionRate: rate, CommissionCents: order.CommissionCents, PlayerIncomeCents: order.PlayerIncomeCents, SettlementStatus: model.SettlementStatusPending, SettlementMonth: order.CompletedAt.In(time.Local).Format("2006-01")}
		if err := tx.Create(&record).Error; err != nil {
			return err
		}
		var player model.Player
		if err := tx.First(&player, record.PlayerID).Error; err != nil {
			return err
		}
		if err := move(tx, player.UserID, fmt.Sprintf("commission:%d", record.ID), 0, record.PlayerIncomeCents, 0); err != nil {
			return err
		}
		if err := tx.Omit(clause.Associations).Save(&order).Error; err != nil {
			return err
		}
		result = order
		return nil
	})
	return &result, err
}

func (s *Service) Unfreeze(ctx context.Context, recordID uint64) (int64, error) {
	var amount int64
	err := s.transaction(ctx, func(tx *gorm.DB) error {
		var record model.CommissionRecord
		if err := lock(tx).First(&record, recordID).Error; err != nil {
			return err
		}
		if record.SettlementStatus == model.SettlementStatusSettled {
			return nil
		}
		if record.SettlementStatus != model.SettlementStatusPending && record.SettlementStatus != model.SettlementStatusDisputed {
			return apierr.BadRequest("佣金不可解冻")
		}
		var order model.Order
		if err := lock(tx).First(&order, record.OrderID).Error; err != nil {
			return err
		}
		if order.Status != model.OrderStatusCompleted || order.CompletedAt == nil || order.CompletedAt.After(time.Now().AddDate(0, 0, -7)) {
			return apierr.BadRequest("未满T+7或订单未完成")
		}
		var open int64
		if err := tx.Model(&model.OrderDispute{}).Where("order_id = ? AND status IN ?", order.ID, []model.DisputeStatus{model.DisputeStatusPending, model.DisputeStatusAssigned, model.DisputeStatusMediating}).Count(&open).Error; err != nil {
			return err
		}
		if open > 0 || order.HasDispute {
			return apierr.Conflict("订单仍有争议")
		}
		ok, err := hasMovement(tx, fmt.Sprintf("commission:%d", record.ID))
		if err != nil {
			return err
		}
		if !ok {
			return apierr.Conflict("历史佣金缺少钱包流水，需要人工对账")
		}
		var player model.Player
		if err := tx.First(&player, record.PlayerID).Error; err != nil {
			return err
		}
		if err := move(tx, player.UserID, fmt.Sprintf("unfreeze:%d", record.ID), record.PlayerIncomeCents, -record.PlayerIncomeCents, 0); err != nil {
			return err
		}
		now := time.Now()
		if err := tx.Model(&record).Updates(map[string]any{"settlement_status": model.SettlementStatusSettled, "settled_at": now}).Error; err != nil {
			return err
		}
		amount = record.PlayerIncomeCents
		return nil
	})
	return amount, err
}

// BuildMonth produces a report, not a second credit of already-settled income.
func (s *Service) BuildMonth(ctx context.Context, month string) error {
	date, err := time.Parse("2006-01", month)
	if err != nil || date.Format("2006-01") != month {
		return apierr.BadRequest("月份格式应为YYYY-MM")
	}
	if month >= time.Now().Format("2006-01") {
		return apierr.BadRequest("只能生成已结束月份的结算单")
	}
	return s.transaction(ctx, func(tx *gorm.DB) error {
		var records []model.CommissionRecord
		if err := tx.Joins("JOIN wallet_ledgers l ON l.key = 'commission:' || CAST(commission_records.id AS TEXT)").Where("settlement_month = ?", month).Find(&records).Error; err != nil {
			return err
		}
		if len(records) == 0 {
			return apierr.BadRequest("该月无收益记录")
		}
		stats := map[uint64]*model.MonthlySettlement{}
		for _, r := range records {
			v := stats[r.PlayerID]
			if v == nil {
				v = &model.MonthlySettlement{PlayerID: r.PlayerID, SettlementMonth: month, Status: model.MonthlySettlementStatusPending}
				stats[r.PlayerID] = v
			}
			if r.SettlementStatus != "canceled" {
				v.TotalOrderCount++
			}
			v.TotalAmountCents += r.TotalAmountCents
			v.TotalCommissionCents += r.CommissionCents
			v.TotalIncomeCents += r.PlayerIncomeCents
			v.FinalIncomeCents = v.TotalIncomeCents
		}
		for _, v := range stats {
			var old model.MonthlySettlement
			err := lock(tx).Where("player_id = ? AND settlement_month = ?", v.PlayerID, month).First(&old).Error
			if err == nil {
				if old.Status != model.MonthlySettlementStatusPending {
					if old.TotalOrderCount != v.TotalOrderCount || old.TotalAmountCents != v.TotalAmountCents || old.TotalCommissionCents != v.TotalCommissionCents || old.TotalIncomeCents != v.TotalIncomeCents {
						return apierr.Conflict("已确认结算单发生差异，需要人工对账")
					}
					continue
				}
				v.ID = old.ID
				v.CreatedAt = old.CreatedAt
				v.BonusCents = old.BonusCents
				v.FinalIncomeCents += old.BonusCents
			} else if !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
			if err := tx.Save(v).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

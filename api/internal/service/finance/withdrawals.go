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

func (s *Service) RequestWithdraw(ctx context.Context, userID uint64, amount int64, method, account string) (*model.Withdraw, error) {
	if amount < 10000 || amount > 1000000000 || strings.TrimSpace(account) == "" || len(account) > 255 {
		return nil, apierr.BadRequest("提现金额或收款账户无效")
	}
	if method != "bank" && method != "wechat" && method != "alipay" {
		return nil, apierr.BadRequest("提现方式无效")
	}
	var result model.Withdraw
	err := s.transaction(ctx, func(tx *gorm.DB) error {
		var player model.Player
		if err := lock(tx).Where("user_id = ?", userID).First(&player).Error; err != nil {
			return err
		}
		var count int64
		if err := tx.Model(&model.Withdraw{}).Where("player_id = ? AND status IN ?", player.ID, []model.WithdrawStatus{model.WithdrawStatusPending, model.WithdrawStatusApproved}).Count(&count).Error; err != nil {
			return err
		}
		if count > 0 {
			return apierr.Conflict("仍有待审核或待发放的提现")
		}
		now := time.Now()
		day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
		month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
		for _, limit := range []struct {
			start  time.Time
			end    time.Time
			amount int64
		}{{day, day.AddDate(0, 0, 1), 5000000}, {month, month.AddDate(0, 1, 0), 20000000}} {
			var spent int64
			if err := tx.Model(&model.Withdraw{}).Where("player_id = ? AND created_at >= ? AND created_at < ? AND status IN ?", player.ID, limit.start, limit.end, []model.WithdrawStatus{model.WithdrawStatusPending, model.WithdrawStatusApproved, model.WithdrawStatusCompleted}).Select("COALESCE(SUM(amount_cents),0)").Scan(&spent).Error; err != nil {
				return err
			}
			if spent > limit.amount-amount {
				return apierr.BadRequest("超过每日或每月提现限额")
			}
		}
		// Only ledger-backed, settled income can be withdrawn; consumer top-ups
		// and unverified legacy records cannot increase the payroll allowance.
		var earned, used int64
		if err := tx.Model(&model.CommissionRecord{}).Joins("JOIN wallet_ledgers l ON l.key = 'commission:' || CAST(commission_records.id AS TEXT)").Where("commission_records.player_id = ? AND commission_records.settlement_status = ?", player.ID, model.SettlementStatusSettled).Select("COALESCE(SUM(player_income_cents),0)").Scan(&earned).Error; err != nil {
			return err
		}
		if err := tx.Model(&model.Withdraw{}).Where("player_id = ? AND status IN ?", player.ID, []model.WithdrawStatus{model.WithdrawStatusPending, model.WithdrawStatusApproved, model.WithdrawStatusCompleted}).Select("COALESCE(SUM(amount_cents),0)").Scan(&used).Error; err != nil {
			return err
		}
		if earned-used < amount {
			return apierr.BadRequest("已结算收益不足")
		}
		result = model.Withdraw{PlayerID: player.ID, UserID: userID, AmountCents: amount, Method: model.WithdrawMethod(method), AccountInfo: account, Status: model.WithdrawStatusPending}
		if err := tx.Create(&result).Error; err != nil {
			return err
		}
		return move(tx, userID, fmt.Sprintf("withdraw-reserve:%d", result.ID), -amount, 0, amount)
	})
	return &result, err
}

func ensureReserved(tx *gorm.DB, w *model.Withdraw) error {
	ok, err := hasMovement(tx, fmt.Sprintf("withdraw-reserve:%d", w.ID))
	if err != nil {
		return err
	}
	if !ok {
		return apierr.Conflict("历史提现未预留资金，请先人工对账")
	}
	var player model.Player
	if err := tx.First(&player, w.PlayerID).Error; err != nil {
		return err
	}
	if player.UserID != w.UserID {
		return apierr.Conflict("提现用户与陪玩师不匹配")
	}
	return nil
}

func salary(tx *gorm.DB, w *model.Withdraw, tax int64) (*model.SalaryPaymentRecord, error) {
	if tax < 0 || tax > w.AmountCents {
		return nil, apierr.BadRequest("代扣税额不能小于0或超过提现金额")
	}
	if w.SettlementCompanyID == nil {
		var assignment model.PlayerCompanyAssignment
		now := time.Now()
		if err := tx.Where("player_id = ? AND is_current = ? AND effective_date <= ? AND (end_date IS NULL OR end_date > ?)", w.PlayerID, true, now, now).First(&assignment).Error; err != nil {
			return nil, apierr.BadRequest("请先为陪玩师分配有效的结算公司")
		}
		var company model.SettlementCompany
		if err := tx.First(&company, assignment.SettlementCompanyID).Error; err != nil {
			return nil, err
		}
		if !company.IsActive() {
			return nil, apierr.BadRequest("结算公司已停用")
		}
		w.SetRoutingInfo(&company)
	}
	w.TaxDeductedCents = tax
	w.ActualAmountCents = w.AmountCents - tax
	var r model.SalaryPaymentRecord
	err := tx.Where("withdraw_id = ?", w.ID).First(&r).Error
	if err == nil && r.Status != "pending" {
		return nil, apierr.Conflict("工资记录已发放，不能修改")
	}
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	r.WithdrawID = w.ID
	r.PlayerID = w.PlayerID
	r.SettlementCompanyID = *w.SettlementCompanyID
	r.SettlementCompanyName = w.SettlementCompanyName
	r.AmountCents = w.AmountCents
	r.TaxDeductedCents = tax
	r.ActualAmountCents = w.ActualAmountCents
	r.BankAccount = w.PaymentBankAccount
	r.Status = "pending"
	if err := tx.Omit(clause.Associations).Save(&r).Error; err != nil {
		return nil, err
	}
	return &r, nil
}

func (s *Service) PrepareSalary(ctx context.Context, id uint64, tax int64) (*model.SalaryPaymentRecord, error) {
	var result *model.SalaryPaymentRecord
	err := s.transaction(ctx, func(tx *gorm.DB) error {
		var w model.Withdraw
		if err := lock(tx).First(&w, id).Error; err != nil {
			return err
		}
		if w.Status != model.WithdrawStatusApproved {
			return apierr.BadRequest("提现尚未审核通过")
		}
		if err := ensureReserved(tx, &w); err != nil {
			return err
		}
		var err error
		result, err = salary(tx, &w, tax)
		if err != nil {
			return err
		}
		return tx.Omit(clause.Associations).Save(&w).Error
	})
	return result, err
}

// ChangeWithdrawal records an admin decision or a reconciled manual payout.
// Completing requires an actual receipt; approval alone never means paid.
func (s *Service) ChangeWithdrawal(ctx context.Context, id, adminID uint64, target model.WithdrawStatus, remark, transactionNo string) error {
	if adminID == 0 {
		return apierr.Forbidden("缺少审核人")
	}
	return s.transaction(ctx, func(tx *gorm.DB) error {
		var w model.Withdraw
		if err := lock(tx).First(&w, id).Error; err != nil {
			return err
		}
		if w.Status == target {
			if target == model.WithdrawStatusCompleted && w.BankTransactionNo != transactionNo {
				return apierr.Conflict("发放流水与原记录不一致")
			}
			return nil
		}
		if err := ensureReserved(tx, &w); err != nil {
			return err
		}
		now := time.Now()
		switch target {
		case model.WithdrawStatusApproved:
			if w.Status != model.WithdrawStatusPending {
				return apierr.BadRequest("只能批准待审核提现")
			}
			if _, err := salary(tx, &w, 0); err != nil {
				return err
			}
			w.AdminRemark = remark
		case model.WithdrawStatusRejected:
			if w.Status != model.WithdrawStatusPending || strings.TrimSpace(remark) == "" {
				return apierr.BadRequest("只能拒绝待审核提现且须填写原因")
			}
			if err := move(tx, w.UserID, fmt.Sprintf("withdraw-release:%d", w.ID), w.AmountCents, 0, -w.AmountCents); err != nil {
				return err
			}
			w.RejectReason = remark
		case model.WithdrawStatusCompleted:
			transactionNo = strings.TrimSpace(transactionNo)
			if w.Status != model.WithdrawStatusApproved || transactionNo == "" || len(transactionNo) > 64 {
				return apierr.BadRequest("需审核通过并填写真实发放流水号")
			}
			var duplicate int64
			if err := tx.Model(&model.Withdraw{}).Where("id <> ? AND bank_transaction_no = ?", w.ID, transactionNo).Count(&duplicate).Error; err != nil {
				return err
			}
			if duplicate > 0 {
				return apierr.Conflict("发放流水号已使用")
			}
			var r model.SalaryPaymentRecord
			if err := lock(tx).Where("withdraw_id = ? AND status = ?", w.ID, "pending").First(&r).Error; err != nil {
				return apierr.BadRequest("缺少待发放工资记录")
			}
			if r.AmountCents != w.AmountCents || r.TaxDeductedCents != w.TaxDeductedCents || r.ActualAmountCents != w.AmountCents-w.TaxDeductedCents {
				return apierr.Conflict("工资金额与提现不匹配")
			}
			if err := move(tx, w.UserID, fmt.Sprintf("withdraw-paid:%d", w.ID), 0, 0, -w.AmountCents); err != nil {
				return err
			}
			r.Status = "completed"
			r.BankTransactionNo = transactionNo
			r.PaidAt = &now
			if err := tx.Omit(clause.Associations).Save(&r).Error; err != nil {
				return err
			}
			w.BankTransactionNo = transactionNo
			w.PaidAt = &now
			w.CompletedAt = &now
		default:
			return apierr.BadRequest("提现状态无效")
		}
		w.Status = target
		w.ProcessedBy = &adminID
		w.ProcessedAt = &now
		return tx.Omit(clause.Associations).Save(&w).Error
	})
}

func (s *Service) Income(ctx context.Context, playerID uint64, from, to *time.Time) (int64, int64, error) {
	query := s.db.WithContext(ctx).Model(&model.CommissionRecord{}).Joins("JOIN wallet_ledgers l ON l.key = 'commission:' || CAST(commission_records.id AS TEXT)").Where("player_id = ? AND settlement_status <> ?", playerID, "canceled")
	if from != nil {
		query = query.Where("commission_records.created_at >= ?", *from)
	}
	if to != nil {
		query = query.Where("commission_records.created_at < ?", *to)
	}
	var totals struct {
		Income int64
		Count  int64
	}
	err := query.Select("COALESCE(SUM(player_income_cents),0) AS income, COUNT(*) AS count").Scan(&totals).Error
	return totals.Income, totals.Count, err
}

type WithdrawStats struct {
	Pending                    int64 `json:"pending"`
	Approved                   int64 `json:"approved"`
	Completed                  int64 `json:"completed"`
	MonthlyVerifiedAmountCents int64 `json:"monthlyVerifiedAmountCents"`
}

// WithdrawStatistics counts stored applications and reports only reconciled
// payouts in the current month; legacy completed rows do not prove payment.
func (s *Service) WithdrawStatistics(ctx context.Context) (*WithdrawStats, error) {
	now := time.Now()
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	end := start.AddDate(0, 1, 0)
	var result WithdrawStats
	err := s.db.WithContext(ctx).Model(&model.Withdraw{}).
		Joins("LEFT JOIN wallet_ledgers l ON l.key = 'withdraw-paid:' || CAST(withdraws.id AS TEXT)").
		Select(`COALESCE(SUM(CASE WHEN status = 'pending' THEN 1 ELSE 0 END),0) AS pending,
		COALESCE(SUM(CASE WHEN status = 'approved' THEN 1 ELSE 0 END),0) AS approved,
		COALESCE(SUM(CASE WHEN status = 'completed' THEN 1 ELSE 0 END),0) AS completed,
		COALESCE(SUM(CASE WHEN status = 'completed' AND l.id IS NOT NULL AND completed_at >= ? AND completed_at < ? THEN amount_cents ELSE 0 END),0) AS monthly_verified_amount_cents`, start, end).
		Scan(&result).Error
	return &result, err
}

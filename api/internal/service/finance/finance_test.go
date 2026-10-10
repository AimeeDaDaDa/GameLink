package finance_test

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"gamelink/internal/model"
	commissionrepo "gamelink/internal/repository/commission"
	orderrepo "gamelink/internal/repository/implementations"
	payrepo "gamelink/internal/repository/order"
	userrepo "gamelink/internal/repository/user"
	withdrawrepo "gamelink/internal/repository/withdraw"
	"gamelink/internal/service/finance"
	payservice "gamelink/internal/service/payment"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type fixture struct {
	db            *gorm.DB
	service       *finance.Service
	buyer, earner uint64
	player        uint64
	item          uint64
}

func setup(t *testing.T) *fixture {
	t.Helper()
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)", filepath.ToSlash(filepath.Join(t.TempDir(), "finance.db")))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent), DisableForeignKeyConstraintWhenMigrating: true})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Player{}, &model.ServiceItem{}, &model.Order{}, &model.Payment{}, &model.Wallet{}, &model.WalletLedger{}, &model.CommissionRule{}, &model.CommissionRecord{}, &model.MonthlySettlement{}, &model.OrderDispute{}, &model.Withdraw{}, &model.SalaryPaymentRecord{}, &model.PlayerCompanyAssignment{}, &model.SettlementCompany{}, &model.RefundRecord{}))
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(8)
	t.Cleanup(func() { _ = sqlDB.Close() })
	buyer := model.User{Email: "buyer@test.example", Name: "Buyer", Phone: "13800000001"}
	earner := model.User{Email: "earner@test.example", Name: "Earner", Phone: "13800000002"}
	require.NoError(t, db.Create(&buyer).Error)
	require.NoError(t, db.Create(&earner).Error)
	player := model.Player{UserID: earner.ID, Nickname: "Player"}
	require.NoError(t, db.Create(&player).Error)
	// PlayerID intentionally differs from UserID, exposing the old refund bug.
	require.NotEqual(t, player.ID, earner.ID)
	item := model.ServiceItem{ItemCode: "service", Name: "Game", CommissionRate: 0.20, IsActive: true, SubCategory: model.SubCategorySolo}
	require.NoError(t, db.Create(&item).Error)
	require.NoError(t, db.Create(&model.CommissionRule{Name: "Default", Type: model.CommissionRuleTypeDefault, Rate: 20, IsActive: true}).Error)
	require.NoError(t, db.Create(&model.Wallet{UserID: buyer.ID, BalanceCents: 1000000}).Error)
	require.NoError(t, db.Create(&model.Wallet{UserID: earner.ID}).Error)
	company := model.SettlementCompany{Name: "Company", Status: model.CompanyStatusActive, CreditCode: "test-credit", BankAccount: "test-bank"}
	require.NoError(t, db.Create(&company).Error)
	require.NoError(t, db.Create(&model.PlayerCompanyAssignment{PlayerID: player.ID, SettlementCompanyID: company.ID, IsCurrent: true, EffectiveDate: time.Now().AddDate(0, -1, 0), AssignedBy: buyer.ID}).Error)
	return &fixture{db: db, service: finance.New(db), buyer: buyer.ID, earner: earner.ID, player: player.ID, item: item.ID}
}

func (f *fixture) order(t *testing.T, amount int64) *model.Order {
	t.Helper()
	order := &model.Order{UserID: f.buyer, PlayerID: &f.player, ItemID: f.item, TotalPriceCents: amount, UnitPriceCents: amount, Currency: model.CurrencyCNY, Status: model.OrderStatusPending, RequiredPlayers: 1}
	require.NoError(t, f.db.Create(order).Error)
	return order
}
func (f *fixture) wallet(t *testing.T, userID uint64) model.Wallet {
	t.Helper()
	var wallet model.Wallet
	require.NoError(t, f.db.Where("user_id = ?", userID).First(&wallet).Error)
	return wallet
}
func (f *fixture) earned(t *testing.T, amount int64) (*model.Order, *model.Payment, *model.CommissionRecord) {
	t.Helper()
	order := f.order(t, amount)
	p, err := f.service.PayWallet(context.Background(), f.buyer, order.ID, fmt.Sprintf("pay-%d", order.ID))
	require.NoError(t, err)
	require.NoError(t, f.db.Model(order).Update("status", model.OrderStatusInProgress).Error)
	_, err = f.service.CompleteOrder(context.Background(), order.ID, f.buyer, "user")
	require.NoError(t, err)
	var r model.CommissionRecord
	require.NoError(t, f.db.Where("order_id = ?", order.ID).First(&r).Error)
	return order, p, &r
}
func (f *fixture) mature(t *testing.T, order *model.Order, record *model.CommissionRecord) {
	t.Helper()
	require.NoError(t, f.db.Model(order).Update("completed_at", time.Now().AddDate(0, 0, -8)).Error)
	_, err := f.service.Unfreeze(context.Background(), record.ID)
	require.NoError(t, err)
}

func TestCompleteCreditsNetIncomeExactlyOnce(t *testing.T) {
	f := setup(t)
	order, _, r := f.earned(t, 20001)
	require.Equal(t, int64(4000), r.CommissionCents)
	require.Equal(t, int64(16001), r.PlayerIncomeCents)
	_, err := f.service.CompleteOrder(context.Background(), order.ID, f.buyer, "user")
	require.NoError(t, err)
	w := f.wallet(t, f.earner)
	require.Zero(t, w.BalanceCents)
	require.Equal(t, int64(16001), w.FrozenCents)
	var count int64
	require.NoError(t, f.db.Model(&model.CommissionRecord{}).Count(&count).Error)
	require.EqualValues(t, 1, count)
}

func TestUnpaidOrderCannotCreateIncome(t *testing.T) {
	f := setup(t)
	o := f.order(t, 20000)
	require.NoError(t, f.db.Model(o).Update("status", model.OrderStatusInProgress).Error)
	_, err := f.service.CompleteOrder(context.Background(), o.ID, f.buyer, "user")
	require.Error(t, err)
	require.Zero(t, f.wallet(t, f.earner).FrozenCents)
}

func TestPaymentConcurrentIdempotency(t *testing.T) {
	f := setup(t)
	o := f.order(t, 20000)
	var wg sync.WaitGroup
	errs := make([]error, 12)
	ids := make([]uint64, 12)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p, err := f.service.PayWallet(context.Background(), f.buyer, o.ID, "same-request")
			errs[i] = err
			if p != nil {
				ids[i] = p.ID
			}
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		require.NoError(t, err)
		require.Equal(t, ids[0], ids[i])
	}
	require.Equal(t, int64(980000), f.wallet(t, f.buyer).BalanceCents)
	_, err := f.service.PayWallet(context.Background(), f.earner, o.ID, "same-request")
	require.Error(t, err)
}

func TestPaymentFailureRollsBackDebitAndRecord(t *testing.T) {
	f := setup(t)
	o := f.order(t, 20000)
	require.NoError(t, f.db.Exec("CREATE TRIGGER reject_order_confirmation BEFORE UPDATE ON orders WHEN NEW.status = 'confirmed' BEGIN SELECT RAISE(ABORT, 'injected order failure'); END").Error)
	_, err := f.service.PayWallet(context.Background(), f.buyer, o.ID, "failed-pay")
	require.Error(t, err)
	require.Equal(t, int64(1000000), f.wallet(t, f.buyer).BalanceCents)
	var count int64
	require.NoError(t, f.db.Model(&model.Payment{}).Count(&count).Error)
	require.Zero(t, count)
	require.NoError(t, f.db.Model(&model.WalletLedger{}).Count(&count).Error)
	require.Zero(t, count)
}

func TestCommissionFailureRollsBackOrderCompletion(t *testing.T) {
	f := setup(t)
	o := f.order(t, 20000)
	_, err := f.service.PayWallet(context.Background(), f.buyer, o.ID, "pay")
	require.NoError(t, err)
	require.NoError(t, f.db.Model(o).Update("status", model.OrderStatusInProgress).Error)
	require.NoError(t, f.db.Exec("CREATE TRIGGER reject_income BEFORE INSERT ON wallet_ledgers WHEN NEW.key LIKE 'commission:%' BEGIN SELECT RAISE(ABORT, 'injected ledger failure'); END").Error)
	_, err = f.service.CompleteOrder(context.Background(), o.ID, f.buyer, "user")
	require.Error(t, err)
	require.NoError(t, f.db.First(o, o.ID).Error)
	require.Equal(t, model.OrderStatusInProgress, o.Status)
	require.Nil(t, o.CompletedAt)
	require.Zero(t, f.wallet(t, f.earner).FrozenCents)
	var count int64
	require.NoError(t, f.db.Model(&model.CommissionRecord{}).Count(&count).Error)
	require.Zero(t, count)
}

func TestT7UsesCompletionTimeAndBlocksDisputes(t *testing.T) {
	f := setup(t)
	o, _, r := f.earned(t, 20000)
	require.NoError(t, f.db.Model(r).Update("created_at", time.Now().AddDate(0, 0, -30)).Error)
	_, err := f.service.Unfreeze(context.Background(), r.ID)
	require.Error(t, err)
	require.NoError(t, f.db.Model(o).Update("completed_at", time.Now().AddDate(0, 0, -8)).Error)
	dispute := model.OrderDispute{OrderID: o.ID, InitiatorID: f.buyer, Status: model.DisputeStatusPending}
	require.NoError(t, f.db.Create(&dispute).Error)
	_, err = f.service.Unfreeze(context.Background(), r.ID)
	require.Error(t, err)
	require.Zero(t, f.wallet(t, f.earner).BalanceCents)
	require.NoError(t, f.db.Model(&dispute).Update("status", model.DisputeStatusRejected).Error)
	amount, err := f.service.Unfreeze(context.Background(), r.ID)
	require.NoError(t, err)
	require.Equal(t, int64(16000), amount)
	amount, err = f.service.Unfreeze(context.Background(), r.ID)
	require.NoError(t, err)
	require.Zero(t, amount)
	require.Equal(t, int64(16000), f.wallet(t, f.earner).BalanceCents)
}

func TestT7FailureRollsBackWalletRelease(t *testing.T) {
	f := setup(t)
	o, _, r := f.earned(t, 20000)
	require.NoError(t, f.db.Model(o).Update("completed_at", time.Now().AddDate(0, 0, -8)).Error)
	require.NoError(t, f.db.Exec("CREATE TRIGGER reject_settlement BEFORE UPDATE ON commission_records WHEN NEW.settlement_status = 'settled' BEGIN SELECT RAISE(ABORT, 'injected settlement failure'); END").Error)
	_, err := f.service.Unfreeze(context.Background(), r.ID)
	require.Error(t, err)
	w := f.wallet(t, f.earner)
	require.Zero(t, w.BalanceCents)
	require.Equal(t, int64(16000), w.FrozenCents)
}

func TestWithdrawRejectRestoresCorrectUserOnlyOnce(t *testing.T) {
	f := setup(t)
	o, _, r := f.earned(t, 40000)
	f.mature(t, o, r)
	w, err := f.service.RequestWithdraw(context.Background(), f.earner, 10000, "bank", "recipient")
	require.NoError(t, err)
	wallet := f.wallet(t, f.earner)
	require.Equal(t, int64(22000), wallet.BalanceCents)
	require.Equal(t, int64(10000), wallet.WithdrawFrozenCents)
	_, err = f.service.RequestWithdraw(context.Background(), f.earner, 10000, "bank", "recipient")
	require.Error(t, err)
	require.NoError(t, f.service.ChangeWithdrawal(context.Background(), w.ID, f.buyer, model.WithdrawStatusRejected, "wrong account", ""))
	require.NoError(t, f.service.ChangeWithdrawal(context.Background(), w.ID, f.buyer, model.WithdrawStatusRejected, "wrong account", ""))
	wallet = f.wallet(t, f.earner)
	require.Equal(t, int64(32000), wallet.BalanceCents)
	require.Zero(t, wallet.WithdrawFrozenCents)
	require.Equal(t, int64(960000), f.wallet(t, f.buyer).BalanceCents)
}

func TestConcurrentWithdrawReservesOnlyOnce(t *testing.T) {
	f := setup(t)
	o, _, r := f.earned(t, 40000)
	f.mature(t, o, r)
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = f.service.RequestWithdraw(context.Background(), f.earner, 20000, "bank", "recipient")
		}(i)
	}
	wg.Wait()
	success := 0
	for _, e := range errs {
		if e == nil {
			success++
		}
	}
	require.Equal(t, 1, success)
	require.Equal(t, int64(12000), f.wallet(t, f.earner).BalanceCents)
	require.Equal(t, int64(20000), f.wallet(t, f.earner).WithdrawFrozenCents)
}

func TestTopupsAndLegacyIncomeCannotBeWithdrawn(t *testing.T) {
	f := setup(t)
	require.NoError(t, f.db.Model(&model.Wallet{}).Where("user_id = ?", f.earner).Update("balance_cents", 100000).Error)
	_, err := f.service.RequestWithdraw(context.Background(), f.earner, 10000, "bank", "recipient")
	require.Error(t, err)
	legacy := model.CommissionRecord{OrderID: 999, PlayerID: f.player, PlayerIncomeCents: 100000, SettlementStatus: model.SettlementStatusSettled}
	require.NoError(t, f.db.Create(&legacy).Error)
	_, err = f.service.RequestWithdraw(context.Background(), f.earner, 10000, "bank", "recipient")
	require.Error(t, err)
}

func TestSalaryIsPersistedAndRequiresActualReceipt(t *testing.T) {
	f := setup(t)
	o, _, r := f.earned(t, 40000)
	f.mature(t, o, r)
	w, err := f.service.RequestWithdraw(context.Background(), f.earner, 20000, "bank", "recipient")
	require.NoError(t, err)
	require.Error(t, f.service.ChangeWithdrawal(context.Background(), w.ID, f.buyer, model.WithdrawStatusCompleted, "", "receipt"))
	require.NoError(t, f.service.ChangeWithdrawal(context.Background(), w.ID, f.buyer, model.WithdrawStatusApproved, "approved", ""))
	_, err = f.service.PrepareSalary(context.Background(), w.ID, -1)
	require.Error(t, err)
	_, err = f.service.PrepareSalary(context.Background(), w.ID, 20001)
	require.Error(t, err)
	salary, err := f.service.PrepareSalary(context.Background(), w.ID, 1200)
	require.NoError(t, err)
	require.NotZero(t, salary.ID)
	require.Equal(t, int64(18800), salary.ActualAmountCents)
	require.Error(t, f.service.ChangeWithdrawal(context.Background(), w.ID, f.buyer, model.WithdrawStatusCompleted, "", ""))
	require.NoError(t, f.service.ChangeWithdrawal(context.Background(), w.ID, f.buyer, model.WithdrawStatusCompleted, "", "bank-receipt-1"))
	require.NoError(t, f.service.ChangeWithdrawal(context.Background(), w.ID, f.buyer, model.WithdrawStatusCompleted, "", "bank-receipt-1"))
	require.Error(t, f.service.ChangeWithdrawal(context.Background(), w.ID, f.buyer, model.WithdrawStatusCompleted, "", "different-receipt"))
	require.NoError(t, f.db.First(salary, salary.ID).Error)
	require.Equal(t, "completed", salary.Status)
	require.NotNil(t, salary.PaidAt)
	require.Zero(t, f.wallet(t, f.earner).WithdrawFrozenCents)
	var count int64
	require.NoError(t, f.db.Model(&model.SalaryPaymentRecord{}).Count(&count).Error)
	require.EqualValues(t, 1, count)
}

func TestRejectFailureRollsBackRefundAndDecision(t *testing.T) {
	f := setup(t)
	o, _, r := f.earned(t, 40000)
	f.mature(t, o, r)
	w, err := f.service.RequestWithdraw(context.Background(), f.earner, 10000, "bank", "recipient")
	require.NoError(t, err)
	require.NoError(t, f.db.Exec("CREATE TRIGGER reject_decision BEFORE UPDATE ON withdraws WHEN NEW.status = 'rejected' BEGIN SELECT RAISE(ABORT, 'injected failure'); END").Error)
	require.Error(t, f.service.ChangeWithdrawal(context.Background(), w.ID, f.buyer, model.WithdrawStatusRejected, "reject", ""))
	require.Equal(t, int64(22000), f.wallet(t, f.earner).BalanceCents)
	require.Equal(t, int64(10000), f.wallet(t, f.earner).WithdrawFrozenCents)
	require.NoError(t, f.db.First(w, w.ID).Error)
	require.Equal(t, model.WithdrawStatusPending, w.Status)
}

func TestPartialRefundReversesNetIncomeWithoutDoubleCredit(t *testing.T) {
	f := setup(t)
	_, p, r := f.earned(t, 20001)
	resp, err := f.service.RefundWallet(context.Background(), p.ID, 1, "partial", nil, "refund-1")
	require.NoError(t, err)
	require.Equal(t, int64(20000), resp.RemainingAmount)
	_, err = f.service.RefundWallet(context.Background(), p.ID, 1, "partial", nil, "refund-1")
	require.NoError(t, err)
	require.Equal(t, int64(980000), f.wallet(t, f.buyer).BalanceCents)
	require.Equal(t, int64(16000), f.wallet(t, f.earner).FrozenCents)
	_, err = f.service.RefundWallet(context.Background(), p.ID, 20000, "full", nil, "refund-2")
	require.NoError(t, err)
	require.Equal(t, int64(1000000), f.wallet(t, f.buyer).BalanceCents)
	require.Zero(t, f.wallet(t, f.earner).FrozenCents)
	require.NoError(t, f.db.First(r, r.ID).Error)
	require.Equal(t, model.SettlementStatus("canceled"), r.SettlementStatus)
}

func TestRefundFailureRollsBackCustomerAndPlayerBalances(t *testing.T) {
	f := setup(t)
	_, p, _ := f.earned(t, 20000)
	require.NoError(t, f.db.Exec("CREATE TRIGGER reject_refund BEFORE UPDATE ON payments WHEN NEW.refunded_amount_cents > 0 BEGIN SELECT RAISE(ABORT, 'injected refund failure'); END").Error)
	_, err := f.service.RefundWallet(context.Background(), p.ID, 20000, "full", nil)
	require.Error(t, err)
	require.Equal(t, int64(980000), f.wallet(t, f.buyer).BalanceCents)
	require.Equal(t, int64(16000), f.wallet(t, f.earner).FrozenCents)
	var count int64
	require.NoError(t, f.db.Model(&model.RefundRecord{}).Count(&count).Error)
	require.Zero(t, count)
}

func TestGatewayAndUnsignedCallbacksCannotFabricateMoney(t *testing.T) {
	f := setup(t)
	o := f.order(t, 20000)
	svc := payservice.NewPaymentService(payrepo.NewPaymentRepository(f.db), orderrepo.NewOrderRepository(f.db))
	svc.SetFinance(f.service)
	for _, method := range []model.PaymentMethod{model.PaymentMethodWeChat, model.PaymentMethodAlipay, model.PaymentMethodCombined} {
		_, err := svc.CreatePayment(context.Background(), f.buyer, payservice.CreatePaymentRequest{OrderID: o.ID, Method: method})
		require.ErrorIs(t, err, finance.ErrGatewayUnavailable)
	}
	require.Error(t, svc.HandlePaymentCallback(context.Background(), "wechat", map[string]interface{}{"payment_id": uint64(1), "amount_cents": int64(20000)}))
	var count int64
	require.NoError(t, f.db.Model(&model.Payment{}).Count(&count).Error)
	require.Zero(t, count)
}

func TestManualCaptureRequiresReceiptAndMatchingOrder(t *testing.T) {
	f := setup(t)
	o := f.order(t, 20000)
	p := model.Payment{OrderID: o.ID, UserID: f.buyer, AmountCents: 20000, Currency: model.CurrencyCNY, Method: model.PaymentMethodWeChat, Status: model.PaymentStatusPending}
	require.NoError(t, f.db.Create(&p).Error)
	_, err := f.service.CaptureManual(context.Background(), p.ID, "")
	require.Error(t, err)
	_, err = f.service.CaptureManual(context.Background(), p.ID, "bank-receipt")
	require.NoError(t, err)
	_, err = f.service.CaptureManual(context.Background(), p.ID, "bank-receipt")
	require.NoError(t, err)
	require.NoError(t, f.db.First(o, o.ID).Error)
	require.Equal(t, model.OrderStatusConfirmed, o.Status)
	_, err = f.service.RefundWallet(context.Background(), p.ID, 20000, "refund", nil)
	require.ErrorIs(t, err, finance.ErrGatewayUnavailable)
	require.Equal(t, int64(1000000), f.wallet(t, f.buyer).BalanceCents)
}

func TestMonthlyReportIncludesT7RecordsWithoutDoubleCredit(t *testing.T) {
	f := setup(t)
	o, _, r := f.earned(t, 20000)
	f.mature(t, o, r)
	month := time.Now().AddDate(0, -1, 0).Format("2006-01")
	require.NoError(t, f.db.Model(r).Update("settlement_month", month).Error)
	require.NoError(t, f.service.BuildMonth(context.Background(), month))
	require.NoError(t, f.service.BuildMonth(context.Background(), month))
	var report model.MonthlySettlement
	require.NoError(t, f.db.First(&report).Error)
	require.Equal(t, int64(16000), report.TotalIncomeCents)
	require.Equal(t, int64(16000), f.wallet(t, f.earner).BalanceCents)
	require.Error(t, f.service.BuildMonth(context.Background(), "2026-13"))
	require.Error(t, f.service.BuildMonth(context.Background(), time.Now().Format("2006-01")))
}

func TestRefundPreservesConfirmedMonthlyStatementForReconciliation(t *testing.T) {
	f := setup(t)
	o, p, r := f.earned(t, 20000)
	f.mature(t, o, r)
	month := time.Now().AddDate(0, -1, 0).Format("2006-01")
	require.NoError(t, f.db.Model(r).Update("settlement_month", month).Error)
	require.NoError(t, f.service.BuildMonth(context.Background(), month))
	var statement model.MonthlySettlement
	require.NoError(t, f.db.First(&statement).Error)
	require.NoError(t, f.db.Model(&statement).Update("status", model.MonthlySettlementStatusConfirmed).Error)
	_, err := f.service.RefundWallet(context.Background(), p.ID, 10000, "partial refund", nil, "confirmed-statement-refund")
	require.NoError(t, err)
	require.NoError(t, f.db.First(&statement, statement.ID).Error)
	require.Equal(t, model.MonthlySettlementStatusConfirmed, statement.Status)
	require.EqualValues(t, 16000, statement.TotalIncomeCents)
	require.Error(t, f.service.BuildMonth(context.Background(), month))
	require.EqualValues(t, 8000, f.wallet(t, f.earner).BalanceCents)
}

func TestHistoricalReceiptCannotBeAcceptedAsVerified(t *testing.T) {
	f := setup(t)
	o := f.order(t, 20000)
	p := model.Payment{OrderID: o.ID, UserID: f.buyer, AmountCents: 20000, Currency: model.CurrencyCNY, Method: model.PaymentMethodWeChat, Status: model.PaymentStatusPaid, ProviderTradeNo: "legacy-receipt"}
	require.NoError(t, f.db.Create(&p).Error)
	_, err := f.service.CaptureManual(context.Background(), p.ID, "legacy-receipt")
	require.Error(t, err)
	require.NoError(t, f.db.First(&p, p.ID).Error)
	require.Nil(t, p.VerifiedAt)
}

func TestEarningsUseNetCommissionAndRepositoryBalance(t *testing.T) {
	f := setup(t)
	o, _, r := f.earned(t, 20000)
	f.mature(t, o, r)
	income, count, err := f.service.Income(context.Background(), f.player, nil, nil)
	require.NoError(t, err)
	require.Equal(t, int64(16000), income)
	require.EqualValues(t, 1, count)
	b, err := withdrawrepo.NewWithdrawRepository(f.db).GetPlayerBalance(context.Background(), f.player)
	require.NoError(t, err)
	require.Equal(t, int64(16000), b.AvailableBalance)
	// Real repository identities, not a first-page player scan.
	player, err := userrepo.NewPlayerRepository(f.db).GetByUserID(context.Background(), f.earner)
	require.NoError(t, err)
	require.Equal(t, f.player, player.ID)
	_, err = commissionrepo.NewCommissionRepository(f.db).GetRecordByOrderID(context.Background(), o.ID)
	require.NoError(t, err)
}

func TestIncomeDateFilterAndRefundedMonthlyStatement(t *testing.T) {
	f := setup(t)
	o, p, r := f.earned(t, 20000)
	f.mature(t, o, r)
	from, to := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	income, _, err := f.service.Income(context.Background(), f.player, &from, &to)
	require.NoError(t, err)
	require.Equal(t, int64(16000), income)
	month := time.Now().AddDate(0, -1, 0).Format("2006-01")
	require.NoError(t, f.db.Model(r).Update("settlement_month", month).Error)
	require.NoError(t, f.service.BuildMonth(context.Background(), month))
	_, err = f.service.RefundWallet(context.Background(), p.ID, 20000, "full", nil, "monthly-refund")
	require.NoError(t, err)
	require.NoError(t, f.service.BuildMonth(context.Background(), month))
	var report model.MonthlySettlement
	require.NoError(t, f.db.First(&report).Error)
	require.Zero(t, report.TotalIncomeCents)
	require.Zero(t, report.TotalOrderCount)
}

func TestWithdrawalReservationIsNotReleasedByT7(t *testing.T) {
	f := setup(t)
	o, _, r := f.earned(t, 40000)
	f.mature(t, o, r)
	_, err := f.service.RequestWithdraw(context.Background(), f.earner, 10000, "bank", "recipient")
	require.NoError(t, err)
	o, _, r = f.earned(t, 20000)
	f.mature(t, o, r)
	w := f.wallet(t, f.earner)
	require.Equal(t, int64(38000), w.BalanceCents)
	require.Zero(t, w.FrozenCents)
	require.Equal(t, int64(10000), w.WithdrawFrozenCents)
}

func TestPayoutFailureRollsBackSalaryAndReservation(t *testing.T) {
	f := setup(t)
	o, _, r := f.earned(t, 40000)
	f.mature(t, o, r)
	w, err := f.service.RequestWithdraw(context.Background(), f.earner, 20000, "bank", "recipient")
	require.NoError(t, err)
	require.NoError(t, f.service.ChangeWithdrawal(context.Background(), w.ID, f.buyer, model.WithdrawStatusApproved, "approve", ""))
	require.NoError(t, f.db.Exec("CREATE TRIGGER reject_payout BEFORE UPDATE ON withdraws WHEN NEW.status = 'completed' BEGIN SELECT RAISE(ABORT, 'injected payout failure'); END").Error)
	require.Error(t, f.service.ChangeWithdrawal(context.Background(), w.ID, f.buyer, model.WithdrawStatusCompleted, "", "payout-receipt"))
	require.Equal(t, int64(20000), f.wallet(t, f.earner).WithdrawFrozenCents)
	var salary model.SalaryPaymentRecord
	require.NoError(t, f.db.First(&salary).Error)
	require.Equal(t, "pending", salary.Status)
	require.Nil(t, salary.PaidAt)
}

func TestManualExternalRefundDoesNotCreditCustomerWallet(t *testing.T) {
	f := setup(t)
	o := f.order(t, 20000)
	p := model.Payment{OrderID: o.ID, UserID: f.buyer, Method: model.PaymentMethodWeChat, AmountCents: 20000, Currency: model.CurrencyCNY, Status: model.PaymentStatusPending}
	require.NoError(t, f.db.Create(&p).Error)
	_, err := f.service.CaptureManual(context.Background(), p.ID, "external-receipt")
	require.NoError(t, err)
	_, err = f.service.RefundManual(context.Background(), p.ID, 20000, "refunded outside", nil, "external-refund-request", "external-refund-receipt")
	require.NoError(t, err)
	_, err = f.service.RefundManual(context.Background(), p.ID, 20000, "retry", nil, "external-refund-request", "external-refund-receipt")
	require.NoError(t, err)
	_, err = f.service.RefundManual(context.Background(), p.ID, 20000, "different receipt", nil, "external-refund-request", "another-refund-receipt")
	require.Error(t, err)
	require.Equal(t, int64(1000000), f.wallet(t, f.buyer).BalanceCents)
	require.NoError(t, f.db.First(&p, p.ID).Error)
	require.Equal(t, model.PaymentStatusRefunded, p.Status)
	var count int64
	require.NoError(t, f.db.Model(&model.RefundRecord{}).Count(&count).Error)
	require.EqualValues(t, 1, count)
}

func TestDisputeResolutionRequiresActualRefund(t *testing.T) {
	f := setup(t)
	o, p, r := f.earned(t, 20000)
	d := &model.OrderDispute{OrderID: o.ID, InitiatorID: f.buyer, InitiatorType: model.DisputeInitiatorUser, Reason: "quality", TraceID: "test-trace"}
	require.NoError(t, f.service.OpenDispute(context.Background(), d))
	require.Error(t, f.service.ResolveDispute(context.Background(), d.ID, f.buyer, model.ResolutionRefund, "refund"))
	require.NoError(t, f.db.First(d, d.ID).Error)
	require.Equal(t, model.DisputeStatusPending, d.Status)
	require.NoError(t, f.db.First(r, r.ID).Error)
	require.Equal(t, model.SettlementStatusDisputed, r.SettlementStatus)
	_, err := f.service.RefundWallet(context.Background(), p.ID, 20000, "quality", nil, "dispute-refund")
	require.NoError(t, err)
	require.NoError(t, f.service.ResolveDispute(context.Background(), d.ID, f.buyer, model.ResolutionRefund, "refund registered"))
	require.Zero(t, f.wallet(t, f.earner).FrozenCents)
}

func TestDueSettlementProcessesMoreThanOneThousandRecords(t *testing.T) {
	f := setup(t)
	orders := make([]model.Order, 1001)
	old := time.Now().AddDate(0, 0, -8)
	for i := range orders {
		orders[i] = model.Order{UserID: f.buyer, PlayerID: &f.player, ItemID: f.item, TotalPriceCents: 1, Status: model.OrderStatusCompleted, CompletedAt: &old, Currency: model.CurrencyCNY}
	}
	require.NoError(t, f.db.CreateInBatches(&orders, 100).Error)
	records := make([]model.CommissionRecord, 1001)
	for i, o := range orders {
		records[i] = model.CommissionRecord{OrderID: o.ID, PlayerID: f.player, TotalAmountCents: 1, PlayerIncomeCents: 1, SettlementStatus: model.SettlementStatusPending}
	}
	require.NoError(t, f.db.CreateInBatches(&records, 100).Error)
	entries := make([]model.WalletLedger, 1001)
	for i, r := range records {
		entries[i] = model.WalletLedger{Key: fmt.Sprintf("commission:%d", r.ID), UserID: f.earner, FrozenDelta: 1}
	}
	require.NoError(t, f.db.CreateInBatches(&entries, 100).Error)
	require.NoError(t, f.db.Model(&model.Wallet{}).Where("user_id = ?", f.earner).Update("frozen_cents", 1001).Error)
	result, err := f.service.UnfreezeDue(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1001, result.SuccessCount)
	require.Zero(t, result.FailedCount)
	require.Equal(t, int64(1001), result.TotalUnfrozen)
	require.Equal(t, int64(1001), f.wallet(t, f.earner).BalanceCents)
}

func TestWithdrawalMonthlyStatsExcludeLegacyAndPriorMonthPayments(t *testing.T) {
	f := setup(t)
	now := time.Now()
	prior := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location()).Add(-time.Second)
	for _, tc := range []struct {
		amount   int64
		at       time.Time
		verified bool
	}{
		{10000, now, true}, {20000, prior, true}, {30000, now, false},
	} {
		w := model.Withdraw{PlayerID: f.player, UserID: f.earner, AmountCents: tc.amount, Status: model.WithdrawStatusCompleted, CompletedAt: &tc.at, Method: model.WithdrawMethodBank, AccountInfo: "test account"}
		require.NoError(t, f.db.Create(&w).Error)
		if tc.verified {
			require.NoError(t, f.db.Create(&model.WalletLedger{Key: fmt.Sprintf("withdraw-paid:%d", w.ID), UserID: f.earner, WithdrawDelta: -w.AmountCents}).Error)
		}
	}
	stats, err := f.service.WithdrawStatistics(context.Background())
	require.NoError(t, err)
	require.EqualValues(t, 3, stats.Completed)
	require.EqualValues(t, 10000, stats.MonthlyVerifiedAmountCents)
}

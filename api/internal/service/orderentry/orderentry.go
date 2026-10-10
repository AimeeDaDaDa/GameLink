// Package orderentry 实现客服录单（参考 delta-settlement）的报价计算与录单管理。
// 金额统一整数分，比例统一基点（1500 = 15%），禁止浮点财务运算。
package orderentry

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"gamelink/internal/model"
	"gamelink/pkg/apierr"
	"gorm.io/gorm"
)

// RuleVersion 当前录单规则版本（改规则时递增）。
const RuleVersion = "delta-v1"

// Service 录单服务。
type Service struct {
	db *gorm.DB
}

// New 创建录单服务。
func New(db *gorm.DB) *Service { return &Service{db: db} }

// ===== 报价计算 =====

// ParticipantInput 报价输入参与人。
type ParticipantInput struct {
	PlayerID    uint64 `json:"player_id"`
	Role        string `json:"role"`
	ClubRateBps int    `json:"club_rate_bps"`
}

// QuoteInput 报价输入。
type QuoteInput struct {
	OrderType        string            `json:"order_type"`
	GrossAmountCents int64             `json:"gross_amount_cents"`
	TipCents         int64             `json:"tip_cents"`
	DiscountCents    int64             `json:"discount_cents"`
	Participants     []ParticipantInput `json:"participants"`
}

// AllocationOutput 报价分账明细。
type AllocationOutput struct {
	PlayerID        uint64 `json:"player_id"`
	GrossShareCents int64  `json:"gross_share_cents"`
	ClubCents       int64  `json:"club_cents"`
	EarningCents    int64  `json:"earning_cents"`
	TipCents        int64  `json:"tip_cents"`
	SettlementCents int64  `json:"settlement_cents"`
}

// QuoteOutput 报价结果。
type QuoteOutput struct {
	QuoteID             string             `json:"quote_id"`
	RuleVersion         string             `json:"rule_version"`
	GrossAmountCents    int64              `json:"gross_amount_cents"`
	TipCents            int64              `json:"tip_cents"`
	DiscountCents       int64              `json:"discount_cents"`
	PayableCents        int64              `json:"payable_cents"`
	ClubCommissionCents int64              `json:"club_commission_cents"`
	ClubNetCents        int64              `json:"club_net_cents"`
	Allocations         []AllocationOutput `json:"allocations"`
}

// roundHalfUp 计算 amount × bps / 10000，四舍五入（整数分）。
func roundHalfUp(amount int64, bps int) int64 {
	if amount <= 0 || bps <= 0 {
		return 0
	}
	return (amount*int64(bps) + 5000) / 10000
}

// splitGross 按人数分摊金额：整除均分，余分归最后一人（副陪），保证合计等于原金额。
func splitGross(total int64, n int) []int64 {
	if n <= 1 {
		return []int64{total}
	}
	base := total / int64(n)
	rem := total % int64(n)
	out := make([]int64, n)
	for i := range out {
		out[i] = base
	}
	out[n-1] += rem
	return out
}

// Calculate 计算报价（纯逻辑，不动资金）。
func Calculate(in QuoteInput) (*QuoteOutput, error) {
	if in.GrossAmountCents <= 0 {
		return nil, apierr.BadRequest("原价必须大于 0")
	}
	if in.TipCents < 0 || in.DiscountCents < 0 {
		return nil, apierr.BadRequest("打赏和优惠不能为负")
	}
	if in.DiscountCents > in.GrossAmountCents {
		return nil, apierr.BadRequest("优惠不能超过原价")
	}
	if len(in.Participants) == 0 {
		return nil, apierr.BadRequest("至少需要一名陪玩")
	}
	orderType := model.OrderEntryType(in.OrderType)
	if orderType != model.OrderEntryTypeNormal && orderType != model.OrderEntryTypeDual &&
		orderType != model.OrderEntryTypeTrial && orderType != model.OrderEntryTypeOutbound {
		return nil, apierr.BadRequest("无效的订单类型")
	}
	for _, p := range in.Participants {
		if p.PlayerID == 0 {
			return nil, apierr.BadRequest("陪玩ID无效")
		}
		if p.ClubRateBps < 0 || p.ClubRateBps > 10000 {
			return nil, apierr.BadRequest("抽成比例无效")
		}
	}

	// 实际参与人数（体验/外派一人全额，两人平分；普通单一人；双护两人）
	grossShares := splitGross(in.GrossAmountCents, len(in.Participants))
	tipShares := splitGross(in.TipCents, len(in.Participants))

	var clubCommission int64
	allocations := make([]AllocationOutput, len(in.Participants))
	for i, p := range in.Participants {
		c := roundHalfUp(grossShares[i], p.ClubRateBps)
		a := grossShares[i] - c
		s := a + tipShares[i]
		clubCommission += c
		allocations[i] = AllocationOutput{
			PlayerID:        p.PlayerID,
			GrossShareCents: grossShares[i],
			ClubCents:       c,
			EarningCents:    a,
			TipCents:        tipShares[i],
			SettlementCents: s,
		}
	}

	payable := in.GrossAmountCents - in.DiscountCents + in.TipCents
	clubNet := clubCommission - in.DiscountCents

	return &QuoteOutput{
		QuoteID:             newQuoteID(),
		RuleVersion:         RuleVersion,
		GrossAmountCents:    in.GrossAmountCents,
		TipCents:            in.TipCents,
		DiscountCents:       in.DiscountCents,
		PayableCents:        payable,
		ClubCommissionCents: clubCommission,
		ClubNetCents:        clubNet,
		Allocations:         allocations,
	}, nil
}

// ===== 录单 CRUD =====

// SaveInput 保存录单输入。
type SaveInput struct {
	Action            string             `json:"action"` // draft / submit
	CustomerID        *uint64            `json:"customer_id"`
	BusinessDate      string             `json:"business_date"`
	GameID            *uint64            `json:"game_id"`
	ItemID            *uint64            `json:"item_id"`
	Platform          string             `json:"platform"`
	OrderType         string             `json:"order_type"`
	Theme             string             `json:"theme"`
	Note              string             `json:"note"`
	CollectionMode    string             `json:"collection_mode"`
	GrossAmountCents  int64              `json:"gross_amount_cents"`
	TipCents          int64              `json:"tip_cents"`
	DiscountCents     int64              `json:"discount_cents"`
	Participants      []ParticipantInput `json:"participants"`
	OperatorID        uint64             `json:"-"`
}

// SaveResult 保存结果。
type SaveResult struct {
	Entry  *model.OrderEntry `json:"entry"`
	Quote  *QuoteOutput      `json:"quote"`
	Order  *model.Order      `json:"order,omitempty"`
}

// idempotency 幂等检查：同 key+scope 同请求返回同一结果；同 key 不同请求 409。
func (s *Service) idempotency(ctx context.Context, key, scope, requestHash string) (*model.IdempotencyRecord, bool, error) {
	if key == "" {
		return nil, false, nil // 未提供幂等键，不检查
	}
	var rec model.IdempotencyRecord
	err := s.db.WithContext(ctx).Where("key = ? AND scope = ?", key, scope).First(&rec).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if rec.RequestHash != requestHash {
		return nil, false, apierr.Conflict("幂等键冲突：同一键不同请求")
	}
	return &rec, true, nil // 已存在，返回已有结果
}

func (s *Service) recordIdempotency(ctx context.Context, key, scope, requestHash string, result []byte, resourceID uint64) error {
	if key == "" {
		return nil
	}
	rec := &model.IdempotencyRecord{
		Key:         key,
		Scope:       scope,
		RequestHash: requestHash,
		Result:      string(result),
		ResourceID:  resourceID,
	}
	return s.db.WithContext(ctx).Create(rec).Error
}

// Save 保存录单（草稿或提交）。提交时在同一事务建立录单、财务快照、参与人和真实订单。
func (s *Service) Save(ctx context.Context, in SaveInput, idempotencyKey string) (*SaveResult, error) {
	if err := validateSaveInput(in); err != nil {
		return nil, err
	}
	quote, err := Calculate(QuoteInput{
		OrderType:        in.OrderType,
		GrossAmountCents: in.GrossAmountCents,
		TipCents:         in.TipCents,
		DiscountCents:    in.DiscountCents,
		Participants:     in.Participants,
	})
	if err != nil {
		return nil, err
	}

	requestHash := hashSaveInput(in)
	scope := fmt.Sprintf("%d:save", in.OperatorID)
	existing, hit, err := s.idempotency(ctx, idempotencyKey, scope, requestHash)
	if err != nil {
		return nil, err
	}
	if hit {
		var entry model.OrderEntry
		if err := s.db.WithContext(ctx).First(&entry, existing.ResourceID).Error; err != nil {
			return nil, apierr.Conflict("幂等记录存在但录单已丢失")
		}
		return &SaveResult{Entry: &entry, Quote: quote}, nil
	}

	submit := in.Action == "submit"

	var result *SaveResult
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		entry := &model.OrderEntry{
			EntryNo:        generateEntryNo(),
			Status:         model.OrderEntryStatusDraft,
			CustomerID:     in.CustomerID,
			BusinessDate:   in.BusinessDate,
			GameID:         in.GameID,
			ItemID:         in.ItemID,
			Platform:       model.OrderPlatform(in.Platform),
			OrderType:      model.OrderEntryType(in.OrderType),
			Theme:          in.Theme,
			Note:           in.Note,
			CollectionMode: model.CollectionMode(in.CollectionMode),
			CreatedBy:      in.OperatorID,
		}
		if submit {
			entry.Status = model.OrderEntryStatusSubmitted
		}
		if err := tx.Create(entry).Error; err != nil {
			return err
		}

		snap := &model.OrderFinancialSnapshot{
			EntryID:             entry.ID,
			GrossAmountCents:    quote.GrossAmountCents,
			TipCents:            quote.TipCents,
			DiscountCents:       quote.DiscountCents,
			PayableCents:        quote.PayableCents,
			Currency:            "CNY",
			RuleVersion:         quote.RuleVersion,
			ClubCommissionCents: quote.ClubCommissionCents,
			ClubNetCents:        quote.ClubNetCents,
			SnapshotAt:          time.Now(),
		}
		if err := tx.Create(snap).Error; err != nil {
			return err
		}

		for _, p := range in.Participants {
			part := &model.OrderParticipant{
				EntryID:     entry.ID,
				PlayerID:    p.PlayerID,
				Role:        model.OrderParticipantRole(p.Role),
				ClubRateBps: p.ClubRateBps,
			}
			if err := tx.Create(part).Error; err != nil {
				return err
			}
		}

		if submit {
			order, err := s.createOrderTx(tx, entry, snap, quote)
			if err != nil {
				return err
			}
			entry.OrderID = &order.ID
			snap.OrderID = &order.ID
			if err := tx.Model(entry).Update("order_id", order.ID).Error; err != nil {
				return err
			}
			if err := tx.Model(snap).Update("order_id", order.ID).Error; err != nil {
				return err
			}
			result = &SaveResult{Entry: entry, Quote: quote, Order: order}
		} else {
			result = &SaveResult{Entry: entry, Quote: quote}
		}
		return nil
	})
	if err != nil {
		return nil, apierr.InternalError("保存录单失败").WithDetails(err.Error())
	}

	if data, err := json.Marshal(result.Entry.ID); err == nil {
		_ = s.recordIdempotency(ctx, idempotencyKey, scope, requestHash, data, result.Entry.ID)
	}
	return result, nil
}

// createOrderTx 在事务内建立真实订单（复用基础订单模型）。
func (s *Service) createOrderTx(tx *gorm.DB, entry *model.OrderEntry, snap *model.OrderFinancialSnapshot, quote *QuoteOutput) (*model.Order, error) {
	if entry.CustomerID == nil || *entry.CustomerID == 0 {
		return nil, apierr.BadRequest("提交订单必须关联客户")
	}
	if entry.GameID == nil || entry.ItemID == nil {
		return nil, apierr.BadRequest("提交订单必须选择游戏和服务项目")
	}
	var primaryID uint64
	for _, p := range quote.Allocations {
		if p.PlayerID != 0 {
			primaryID = p.PlayerID
			break
		}
	}
	order := &model.Order{
		OrderNo:         model.GenerateEscortOrderNo(),
		UserID:          *entry.CustomerID,
		ItemID:          *entry.ItemID,
		GameID:          entry.GameID,
		Quantity:        1,
		UnitPriceCents:  quote.GrossAmountCents,
		TotalPriceCents: quote.PayableCents, // 兼容旧语义：TotalPriceCents 为应付
		Currency:        model.Currency(snap.Currency),
		Status:          model.OrderStatusPending,
		Title:           entry.Theme,
		Description:     entry.Note,
	}
	if primaryID != 0 {
		order.PlayerID = &primaryID
	}
	if err := tx.Create(order).Error; err != nil {
		return nil, err
	}
	// 写入分账明细（预览，未正式入账）
	for _, p := range quote.Allocations {
		alloc := &model.OrderAllocation{
			OrderID:         order.ID,
			PlayerID:        p.PlayerID,
			GrossShareCents: p.GrossShareCents,
			ClubCents:       p.ClubCents,
			EarningCents:    p.EarningCents,
			TipCents:        p.TipCents,
			SettlementCents: p.SettlementCents,
			Posted:          false,
		}
		if err := tx.Create(alloc).Error; err != nil {
			return nil, err
		}
	}
	return order, nil
}

// Void 作废未入账录单。
func (s *Service) Void(ctx context.Context, entryID, operatorID uint64, reason string) error {
	if reason == "" {
		return apierr.BadRequest("作废需要填写原因")
	}
	var entry model.OrderEntry
	if err := s.db.WithContext(ctx).First(&entry, entryID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return apierr.NotFound("录单不存在")
		}
		return err
	}
	if entry.Status == model.OrderEntryStatusVoided {
		return apierr.Conflict("录单已作废")
	}
	// 已提交且有关联订单的，需通过退款流程，不能直接作废
	if entry.Status == model.OrderEntryStatusSubmitted && entry.OrderID != nil {
		return apierr.BadRequest("已提交录单不能直接作废，请走退款流程")
	}
	return s.db.WithContext(ctx).Model(&entry).Updates(map[string]any{
		"status":      model.OrderEntryStatusVoided,
		"void_reason": reason,
	}).Error
}

// Get 获取录单详情（含快照、参与人、分账）。
func (s *Service) Get(ctx context.Context, entryID uint64) (*model.OrderEntry, error) {
	var entry model.OrderEntry
	if err := s.db.WithContext(ctx).First(&entry, entryID).Error; err != nil {
		return nil, apierr.NotFound("录单不存在")
	}
	return &entry, nil
}

// ===== 辅助 =====

func newQuoteID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "q_" + hex.EncodeToString(b)
}

func generateEntryNo() string {
	return fmt.Sprintf("ENT%s", time.Now().Format("20060102150405"))
}

func hashSaveInput(in SaveInput) string {
	data, _ := json.Marshal(in)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func validateSaveInput(in SaveInput) error {
	if in.Action != "draft" && in.Action != "submit" {
		return apierr.BadRequest("action 必须为 draft 或 submit")
	}
	if in.BusinessDate == "" {
		return apierr.BadRequest("业务日期不能为空")
	}
	if in.Action == "submit" {
		if in.CustomerID == nil || *in.CustomerID == 0 {
			return apierr.BadRequest("提交必须关联客户")
		}
		if in.GameID == nil || *in.GameID == 0 {
			return apierr.BadRequest("提交必须选择游戏")
		}
		if in.ItemID == nil || *in.ItemID == 0 {
			return apierr.BadRequest("提交必须选择服务项目")
		}
		if len(in.Participants) == 0 {
			return apierr.BadRequest("提交必须选择陪玩")
		}
		if in.GrossAmountCents <= 0 {
			return apierr.BadRequest("原价必须大于 0")
		}
	}
	if in.Platform == "" {
		return apierr.BadRequest("平台不能为空")
	}
	return nil
}

package finance

import (
	"context"
	"fmt"
	"gamelink/internal/model"
	"time"
)

type UnfreezeResult struct {
	ProcessedCount int      `json:"processedCount"`
	SuccessCount   int      `json:"successCount"`
	FailedCount    int      `json:"failedCount"`
	TotalUnfrozen  int64    `json:"totalUnfrozen"`
	FailedIDs      []uint64 `json:"failedIds"`
	Errors         []string `json:"errors"`
}

func (s *Service) UnfreezeDue(ctx context.Context) (*UnfreezeResult, error) {
	result := &UnfreezeResult{FailedIDs: []uint64{}, Errors: []string{}}
	var ids []uint64
	err := s.db.WithContext(ctx).Model(&model.CommissionRecord{}).
		Joins("JOIN orders o ON o.id = commission_records.order_id").
		Where("commission_records.settlement_status IN ? AND o.status = ? AND o.completed_at <= ?", []model.SettlementStatus{model.SettlementStatusPending, model.SettlementStatusDisputed}, model.OrderStatusCompleted, time.Now().AddDate(0, 0, -7)).
		Order("commission_records.id").Pluck("commission_records.id", &ids).Error
	if err != nil {
		return nil, err
	}
	result.ProcessedCount = len(ids)
	for _, id := range ids {
		amount, err := s.Unfreeze(ctx, id)
		if err != nil {
			result.FailedCount++
			result.FailedIDs = append(result.FailedIDs, id)
			result.Errors = append(result.Errors, fmt.Sprintf("record %d: %s", id, err))
			continue
		}
		result.SuccessCount++
		result.TotalUnfrozen += amount
	}
	return result, nil
}

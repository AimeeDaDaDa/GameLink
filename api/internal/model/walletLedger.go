package model

import "time"

// WalletLedger is an immutable record of a committed wallet movement.
// Income frozen for settlement and funds reserved for withdrawals are separate.
type WalletLedger struct {
	ID            uint64    `gorm:"primaryKey" json:"id"`
	Key           string    `gorm:"size:160;not null;uniqueIndex" json:"key"`
	UserID        uint64    `gorm:"not null;index" json:"userId"`
	BalanceDelta  int64     `json:"balanceDelta"`
	FrozenDelta   int64     `json:"frozenDelta"`
	WithdrawDelta int64     `json:"withdrawDelta"`
	BalanceAfter  int64     `json:"balanceAfter"`
	FrozenAfter   int64     `json:"frozenAfter"`
	WithdrawAfter int64     `json:"withdrawAfter"`
	CreatedAt     time.Time `json:"createdAt"`
}

func (WalletLedger) TableName() string { return "wallet_ledgers" }

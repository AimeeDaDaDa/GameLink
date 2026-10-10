package model

import "time"

// OrderEntryStatus 录单状态
type OrderEntryStatus string

const (
	OrderEntryStatusDraft     OrderEntryStatus = "draft"     // 草稿
	OrderEntryStatusSubmitted OrderEntryStatus = "submitted" // 已提交
	OrderEntryStatusVoided    OrderEntryStatus = "voided"    // 已作废
)

// OrderPlatform 平台（端游/手游）
type OrderPlatform string

const (
	OrderPlatformPC     OrderPlatform = "pc"
	OrderPlatformMobile OrderPlatform = "mobile"
)

// OrderEntryType 订单类型
type OrderEntryType string

const (
	OrderEntryTypeNormal   OrderEntryType = "normal"
	OrderEntryTypeDual     OrderEntryType = "dual"
	OrderEntryTypeTrial    OrderEntryType = "trial"
	OrderEntryTypeOutbound OrderEntryType = "outbound"
)

// CollectionMode 收款方式
type CollectionMode string

const (
	CollectionModeUncollected CollectionMode = "uncollected" // 暂不收款
	CollectionModeManual      CollectionMode = "manual"      // 人工收款
	CollectionModeWallet      CollectionMode = "wallet"      // 老板预存代扣
)

// OrderParticipantRole 参与人角色
type OrderParticipantRole string

const (
	OrderParticipantRolePrimary   OrderParticipantRole = "primary"
	OrderParticipantRoleSecondary OrderParticipantRole = "secondary"
)

// OrderEntry 客服录单（参考 delta-settlement 的录单工作台）。
// 草稿/提交状态与业务订单分离；提交后才建立真实 Order 并锁定财务快照。
type OrderEntry struct {
	Base
	EntryNo        string           `json:"entryNo" gorm:"column:entry_no;size:64;uniqueIndex"`                     // 录单号
	Status         OrderEntryStatus `json:"status" gorm:"size:16;index;default:'draft'"`                            // draft/submitted/voided
	OrderID        *uint64          `json:"orderId,omitempty" gorm:"column:order_id;index"`                         // 提交后关联的业务订单
	CustomerID     *uint64          `json:"customerId,omitempty" gorm:"column:customer_id;index"`                   // 老板（客户用户ID），草稿可空
	BusinessDate   string           `json:"businessDate" gorm:"column:business_date;size:10;index"`                 // YYYY-MM-DD，按 Asia/Shanghai
	GameID         *uint64          `json:"gameId,omitempty" gorm:"column:game_id;index"`                           // 游戏
	ItemID         *uint64          `json:"itemId,omitempty" gorm:"column:item_id;index"`                           // 服务项目
	Platform       OrderPlatform    `json:"platform" gorm:"size:16;default:'pc'"`                                   // pc/mobile
	OrderType      OrderEntryType   `json:"orderType" gorm:"column:order_type;size:16;default:'normal'"`            // normal/dual/trial/outbound
	Theme          string           `json:"theme,omitempty" gorm:"size:100"`                                        // 主题玩法
	Note           string           `json:"note,omitempty" gorm:"type:text"`                                        // 备注
	CollectionMode CollectionMode   `json:"collectionMode" gorm:"column:collection_mode;size:16;default:'uncollected'"` // 收款方式
	Version        int              `json:"version" gorm:"default:0"`                                               // 乐观锁
	CreatedBy      uint64           `json:"createdBy" gorm:"column:created_by"`                                     // 录单者
	VoidReason     string           `json:"voidReason,omitempty" gorm:"column:void_reason;type:text"`               // 作废原因
}

// TableName 指定表名
func (OrderEntry) TableName() string { return "order_entries" }

// OrderFinancialSnapshot 录单财务快照（保存时锁定，改默认规则不重算历史）。
// 金额统一整数分，比例统一基点（1500=15%）。
type OrderFinancialSnapshot struct {
	Base
	EntryID            uint64     `json:"entryId" gorm:"column:entry_id;uniqueIndex"`             // 一条录单一个快照
	OrderID            *uint64    `json:"orderId,omitempty" gorm:"column:order_id;index"`         // 提交后回填
	GrossAmountCents   int64      `json:"grossAmountCents" gorm:"column:gross_amount_cents;not null"` // 原价
	TipCents           int64      `json:"tipCents" gorm:"column:tip_cents;default:0"`             // 老板打赏
	DiscountCents      int64      `json:"discountCents" gorm:"column:discount_cents;default:0"`   // 优惠（俱乐部承担）
	PayableCents       int64      `json:"payableCents" gorm:"column:payable_cents;not null"`      // 老板应付 = 原价-优惠+打赏
	Currency           string     `json:"currency" gorm:"size:3;default:'CNY'"`
	RuleVersion        string     `json:"ruleVersion,omitempty" gorm:"column:rule_version;size:64"` // 规则版本
	ClubCommissionCents int64     `json:"clubCommissionCents" gorm:"column:club_commission_cents;default:0"` // 俱乐部毛抽成
	ClubNetCents       int64      `json:"clubNetCents" gorm:"column:club_net_cents;default:0"`    // 俱乐部净额 = 毛抽成-优惠
	SnapshotAt         time.Time  `json:"snapshotAt" gorm:"column:snapshot_at"`
}

// TableName 指定表名
func (OrderFinancialSnapshot) TableName() string { return "order_financial_snapshots" }

// OrderParticipant 订单参与人（主/副陪玩），规则快照。
type OrderParticipant struct {
	Base
	EntryID        uint64                `json:"entryId" gorm:"column:entry_id;index"`
	OrderID        *uint64               `json:"orderId,omitempty" gorm:"column:order_id;index"`
	PlayerID       uint64                `json:"playerId" gorm:"column:player_id;not null;index"`
	Role           OrderParticipantRole  `json:"role" gorm:"size:16;default:'primary'"`
	PlayerType     string                `json:"playerType,omitempty" gorm:"column:player_type;size:16;default:'regular'"` // regular/external
	ClubRateBps    int                   `json:"clubRateBps" gorm:"column:club_rate_bps;default:0"`     // 抽成基点
	DepositRateBps int                   `json:"depositRateBps" gorm:"column:deposit_rate_bps;default:0"` // 押金基点
	ExemptDeposit  bool                  `json:"exemptDeposit" gorm:"column:exempt_deposit;default:false"` // 免押金
}

// TableName 指定表名
func (OrderParticipant) TableName() string { return "order_participants" }

// OrderAllocation 正式分账明细（服务完成入账时生成，与预览区分）。
type OrderAllocation struct {
	Base
	OrderID          uint64 `json:"orderId" gorm:"column:order_id;index"`
	ParticipantID    uint64 `json:"participantId" gorm:"column:participant_id;index"`
	PlayerID         uint64 `json:"playerId" gorm:"column:player_id;index"`
	GrossShareCents  int64  `json:"grossShareCents" gorm:"column:gross_share_cents;default:0"`   // 原价分摊
	ClubCents        int64  `json:"clubCents" gorm:"column:club_cents;default:0"`                // 抽成
	DepositCents     int64  `json:"depositCents" gorm:"column:deposit_cents;default:0"`          // 押金暂扣
	EarningCents     int64  `json:"earningCents" gorm:"column:earning_cents;default:0"`          // 服务净收益
	TipCents         int64  `json:"tipCents" gorm:"column:tip_cents;default:0"`                  // 打赏分摊
	SettlementCents  int64  `json:"settlementCents" gorm:"column:settlement_cents;default:0"`    // 本单应结算 = 净收益+打赏
	Posted           bool   `json:"posted" gorm:"default:false;index"`                           // 是否已正式入账
	PostedAt         *time.Time `json:"postedAt,omitempty" gorm:"column:posted_at"`
}

// TableName 指定表名
func (OrderAllocation) TableName() string { return "order_allocations" }

// IdempotencyRecord 幂等键记录（请求哈希 + 业务结果，防重复保存/扣款）。
type IdempotencyRecord struct {
	Base
	Key          string `json:"key" gorm:"size:128;not null;index:idx_idem_scope_key,priority:2"`
	Scope        string `json:"scope" gorm:"size:64;not null;index:idx_idem_scope_key,priority:1"` // 操作者:操作类型
	RequestHash  string `json:"requestHash" gorm:"column:request_hash;size:64"`
	Result       string `json:"result" gorm:"type:text"` // 业务结果（JSON）
	ResourceID   uint64 `json:"resourceId" gorm:"column:resource_id;index"` // 关联资源ID（如录单ID）
}

// TableName 指定表名
func (IdempotencyRecord) TableName() string { return "idempotency_records" }

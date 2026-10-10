package orderentry

import "testing"

// 对应文档第 6.2 节验收示例（P1 不含押金部分）。
func TestCalculate_NormalWithTipAndDiscount(t *testing.T) {
	out, err := Calculate(QuoteInput{
		OrderType:        "normal",
		GrossAmountCents: 10000, // 原价 100 元
		TipCents:         2000,  // 打赏 20 元
		DiscountCents:    1000,  // 优惠 10 元
		Participants: []ParticipantInput{
			{PlayerID: 1, Role: "primary", ClubRateBps: 1500}, // 抽成 15%
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.PayableCents != 11000 {
		t.Errorf("payable = %d, want 11000", out.PayableCents)
	}
	if out.ClubCommissionCents != 1500 {
		t.Errorf("clubCommission = %d, want 1500", out.ClubCommissionCents)
	}
	if out.ClubNetCents != 500 {
		t.Errorf("clubNet = %d, want 500", out.ClubNetCents)
	}
	alloc := out.Allocations[0]
	if alloc.ClubCents != 1500 {
		t.Errorf("alloc.club = %d, want 1500", alloc.ClubCents)
	}
	if alloc.EarningCents != 8500 {
		t.Errorf("alloc.earning = %d, want 8500", alloc.EarningCents)
	}
	if alloc.TipCents != 2000 {
		t.Errorf("alloc.tip = %d, want 2000", alloc.TipCents)
	}
	if alloc.SettlementCents != 10500 {
		t.Errorf("alloc.settlement = %d, want 10500", alloc.SettlementCents)
	}
}

func TestCalculate_DualIndependentRates(t *testing.T) {
	out, err := Calculate(QuoteInput{
		OrderType:        "dual",
		GrossAmountCents: 10000,
		TipCents:         2000,
		DiscountCents:    1000,
		Participants: []ParticipantInput{
			{PlayerID: 1, Role: "primary", ClubRateBps: 1500},
			{PlayerID: 2, Role: "secondary", ClubRateBps: 1000},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.PayableCents != 11000 {
		t.Errorf("payable = %d, want 11000", out.PayableCents)
	}
	if out.ClubCommissionCents != 1250 {
		t.Errorf("clubCommission = %d, want 1250", out.ClubCommissionCents)
	}
	if out.ClubNetCents != 250 {
		t.Errorf("clubNet = %d, want 250", out.ClubNetCents)
	}
	// 主：gross 5000, club 750, earning 4250, tip 1000, settlement 5250
	primary := out.Allocations[0]
	if primary.ClubCents != 750 || primary.EarningCents != 4250 || primary.SettlementCents != 5250 {
		t.Errorf("primary alloc wrong: %+v", primary)
	}
	// 副：gross 5000, club 500, earning 4500, tip 1000, settlement 5500
	secondary := out.Allocations[1]
	if secondary.ClubCents != 500 || secondary.EarningCents != 4500 || secondary.SettlementCents != 5500 {
		t.Errorf("secondary alloc wrong: %+v", secondary)
	}
}

func TestCalculate_DualRemainderCents(t *testing.T) {
	out, err := Calculate(QuoteInput{
		OrderType:        "dual",
		GrossAmountCents: 10001, // 100.01 元
		Participants: []ParticipantInput{
			{PlayerID: 1, Role: "primary", ClubRateBps: 0},
			{PlayerID: 2, Role: "secondary", ClubRateBps: 0},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Allocations[0].GrossShareCents != 5000 {
		t.Errorf("primary gross = %d, want 5000", out.Allocations[0].GrossShareCents)
	}
	if out.Allocations[1].GrossShareCents != 5001 {
		t.Errorf("secondary gross = %d, want 5001", out.Allocations[1].GrossShareCents)
	}
}

func TestCalculate_ZeroRateNoCommission(t *testing.T) {
	out, err := Calculate(QuoteInput{
		OrderType:        "normal",
		GrossAmountCents: 10000,
		Participants: []ParticipantInput{
			{PlayerID: 1, Role: "primary", ClubRateBps: 0},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.ClubCommissionCents != 0 {
		t.Errorf("clubCommission = %d, want 0", out.ClubCommissionCents)
	}
	if out.Allocations[0].EarningCents != 10000 {
		t.Errorf("earning = %d, want 10000", out.Allocations[0].EarningCents)
	}
}

func TestCalculate_InvalidDiscount(t *testing.T) {
	_, err := Calculate(QuoteInput{
		OrderType:        "normal",
		GrossAmountCents: 10000,
		DiscountCents:    20000, // 优惠超过原价
		Participants: []ParticipantInput{
			{PlayerID: 1, Role: "primary", ClubRateBps: 1500},
		},
	})
	if err == nil {
		t.Error("expected error for discount > gross")
	}
}

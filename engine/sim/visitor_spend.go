package sim

import "math"

// Visitor spend budget (LLM-644). A traveler's buying power is what he
// ARRIVED with, not what the village has since paid him: SpendBudget seeds
// equal to the purse at spawn and only ever goes down as he pays coin out.
// Sales still credit Coins — those takings leave the map with him at
// despawn, which is what makes a forced-seller factor a net coin drain.
//
// Every door coin can leave an actor by must ask SpendableCoins and settle
// with drawVisitorSpend (see the VisitorState.SpendBudget doc). The doors:
//
//   - Pay (pay_commands.go) — bare transfer; gate + draw
//   - commitPayTransfer (pay_with_item_commands.go) — quote/deposit charge;
//     gated upstream by buyerCanAfford, draw at commit
//   - deliver_order balance (order_commands.go) — gate + draw
//   - lodging auto-renewal (lodger_rebook.go) — gate + draw
//   - labor settle (labor_settle.go) — gated at accept/settle via
//     employerCanCoverLaborReward → buyerCanAfford; draw at transfer
//   - order refund (order.go) — coin RETURNING to a visitor buyer restores
//     the budget it was drawn from (refundVisitorSpend)
//   - AdjustActorHoldings (holdings_commands.go) — operator fiat; a positive
//     coin delta raises the budget too, a debit needs nothing (SpendableCoins
//     min()s against the wallet)

// SpendableCoins is the coin this actor can actually put into a purchase
// right now — the single affordability figure every gate, steer, and
// perception line should quote. For a resident it is the wallet. For a
// visitor it is the smaller of wallet and remaining spend budget, so a
// factor flush with bale proceeds still reads as spent-out once his
// arrival purse is gone.
func (a *Actor) SpendableCoins() int {
	if a.VisitorState == nil {
		return a.Coins
	}
	if a.VisitorState.SpendBudget < a.Coins {
		return a.VisitorState.SpendBudget
	}
	return a.Coins
}

// SpendableCoins is the ActorSnapshot mirror of Actor.SpendableCoins, for
// perception and other read-path consumers working over a published
// snapshot. Same semantics; the VisitorState clone carries the budget.
func (a *ActorSnapshot) SpendableCoins() int {
	if a.VisitorState == nil {
		return a.Coins
	}
	if a.VisitorState.SpendBudget < a.Coins {
		return a.VisitorState.SpendBudget
	}
	return a.Coins
}

// drawVisitorSpend records amount coins of visitor spending against the
// payer's budget. A no-op for residents. Clamps at 0 rather than erroring:
// the affordability gates upstream own rejection; by settle time the coin
// IS moving (a labor reward, a validated charge) and a stale budget must
// not be able to wedge a transfer that has already debited the wallet.
func drawVisitorSpend(a *Actor, amount int) {
	if a == nil || a.VisitorState == nil || amount <= 0 {
		return
	}
	if a.VisitorState.SpendBudget <= amount {
		a.VisitorState.SpendBudget = 0
		return
	}
	a.VisitorState.SpendBudget -= amount
}

// refundVisitorSpend restores budget for coin returned to a visitor buyer
// (an order refund). Bounded in practice by what the refunded charge drew;
// no cap is kept because the budget is a remaining-allowance counter, not
// a mirror of the seed purse. The addition saturates at MaxInt (code_review):
// the wallet credit is overflow-guarded by its caller, but this independent
// counter is not, and a wrapped-negative budget would read as spendable 0
// with a fat wallet — a silently frozen visitor.
func refundVisitorSpend(a *Actor, amount int) {
	if a == nil || a.VisitorState == nil || amount <= 0 {
		return
	}
	if a.VisitorState.SpendBudget > math.MaxInt-amount {
		a.VisitorState.SpendBudget = math.MaxInt
		return
	}
	a.VisitorState.SpendBudget += amount
}

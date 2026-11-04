package ledger

import "github.com/tribe-africa/tribe-backend/pkg/money"

// This file holds the POSTING RULES: small functions that say which
// accounts to debit and credit for each kind of event. They do not touch
// the database, so they are easy to read and reason about.
//
// Remember: a user wallet is a LIABILITY (Tribe owes it to the user), so
//   - money INTO a user wallet  = CREDIT the user account
//   - money OUT of a user wallet = DEBIT the user account
// and the company vault is an ASSET, so
//   - money arriving at the company = DEBIT the vault
//   - money leaving the company     = CREDIT the vault

// DepositLegs: money arrives from outside into a user's wallet.
//
//	Dr Company Vault (asset goes up)       amount
//	Cr User Wallet   (we owe the user more) amount
func DepositLegs(companyVault uint64, userWallet uint64, amount money.Amount) []Leg {
	return []Leg{
		{AccountID: companyVault, Side: Debit, Amount: amount, Memo: "Money received"},
		{AccountID: userWallet, Side: Credit, Amount: amount, Memo: "Deposit"},
	}
}

// TreasuryFundingLegs: the company puts its own money into the vault.
//
//	Dr Company Vault     amount
//	Cr Treasury Funding  amount (equity: the owners' money in the business)
func TreasuryFundingLegs(companyVault uint64, treasuryFunding uint64, amount money.Amount) []Leg {
	return []Leg{
		{AccountID: companyVault, Side: Debit, Amount: amount, Memo: "Treasury funding"},
		{AccountID: treasuryFunding, Side: Credit, Amount: amount, Memo: "Treasury funding"},
	}
}

// CustomerCreditLegs: an admin gives a customer money from the company's own
// funds (goodwill, a refund made by hand, a promo). It is a cost to Tribe, so
// it is booked as an expense. The vault does not move: the money is still at
// the bank, but now we owe it to the customer.
//
//	Dr Customer Adjustments (expense goes up)      amount
//	Cr User Wallet          (we owe the user more) amount
func CustomerCreditLegs(customerAdjustments uint64, userWallet uint64, amount money.Amount) []Leg {
	return []Leg{
		{AccountID: customerAdjustments, Side: Debit, Amount: amount, Memo: "Manual customer credit"},
		{AccountID: userWallet, Side: Credit, Amount: amount, Memo: "Credit from Tribe"},
	}
}

// CustomerDebitLegs: an admin takes money back out of a customer's wallet
// (a credit made in error, a reversed deposit). It is the mirror of a credit,
// so the expense goes down and the money is free company money again.
//
//	Dr User Wallet          (we owe the user less) amount
//	Cr Customer Adjustments (expense goes down)    amount
func CustomerDebitLegs(userWallet uint64, customerAdjustments uint64, amount money.Amount) []Leg {
	return []Leg{
		{AccountID: userWallet, Side: Debit, Amount: amount, Memo: "Adjustment by Tribe"},
		{AccountID: customerAdjustments, Side: Credit, Amount: amount, Memo: "Manual customer debit"},
	}
}

// InternalTransferLegs: one Tribe user pays another in the same asset.
//
//	Dr Sender Wallet    amount + fee
//	Cr Receiver Wallet  amount
//	Cr Fee Revenue      fee (only when there is a fee)
func InternalTransferLegs(senderWallet uint64, receiverWallet uint64, feeRevenue uint64, amount money.Amount, fee money.Amount) []Leg {
	legs := []Leg{
		{AccountID: senderWallet, Side: Debit, Amount: amount.Add(fee), Memo: "Sent to Tribe user"},
		{AccountID: receiverWallet, Side: Credit, Amount: amount, Memo: "Received from Tribe user"},
	}
	if fee.IsPositive() {
		legs = append(legs, Leg{AccountID: feeRevenue, Side: Credit, Amount: fee, Memo: "Transfer fee"})
	}
	return legs
}

// PayoutStartLegs: a user sends money OUT of Tribe (to a bank or wallet).
// We take the money now and park it until the provider confirms.
//
//	Dr User Wallet        amount + fee
//	Cr Payouts In Transit amount
//	Cr Fee Revenue        fee
func PayoutStartLegs(userWallet uint64, payoutInTransit uint64, feeRevenue uint64, amount money.Amount, fee money.Amount) []Leg {
	legs := []Leg{
		{AccountID: userWallet, Side: Debit, Amount: amount.Add(fee), Memo: "Payout"},
		{AccountID: payoutInTransit, Side: Credit, Amount: amount, Memo: "Payout waiting for provider"},
	}
	if fee.IsPositive() {
		legs = append(legs, Leg{AccountID: feeRevenue, Side: Credit, Amount: fee, Memo: "Payout fee"})
	}
	return legs
}

// PayoutSucceededLegs: the provider confirmed the payout, the money left the company.
//
//	Dr Payouts In Transit amount
//	Cr Company Vault      amount (asset goes down)
func PayoutSucceededLegs(payoutInTransit uint64, companyVault uint64, amount money.Amount) []Leg {
	return []Leg{
		{AccountID: payoutInTransit, Side: Debit, Amount: amount, Memo: "Payout confirmed"},
		{AccountID: companyVault, Side: Credit, Amount: amount, Memo: "Paid out by provider"},
	}
}

// PayoutFailedLegs: the provider rejected the payout, give the money back.
//
//	Dr Payouts In Transit amount
//	Cr User Wallet        amount
func PayoutFailedLegs(payoutInTransit uint64, userWallet uint64, amount money.Amount) []Leg {
	return []Leg{
		{AccountID: payoutInTransit, Side: Debit, Amount: amount, Memo: "Payout failed"},
		{AccountID: userWallet, Side: Credit, Amount: amount, Memo: "Payout refunded"},
	}
}

// PurchaseSucceededLegs: the provider confirmed a bill payment or gift card
// purchase, so the money really left the company.
//
// When the customer paid, their money was converted into Purchases In
// Transit (a suspense account) instead of straight out of the vault, so
// nothing is lost if the provider then says no. On confirmation:
//
//	Dr Purchases In Transit amount
//	Cr Company Vault        amount (asset goes down: we paid the provider)
//
// If the provider says no, the original posting is reversed instead (see
// ReversalLegs), which returns the customer's money, fee and all.
func PurchaseSucceededLegs(purchaseInTransit uint64, companyVault uint64, amount money.Amount) []Leg {
	return []Leg{
		{AccountID: purchaseInTransit, Side: Debit, Amount: amount, Memo: "Purchase confirmed"},
		{AccountID: companyVault, Side: Credit, Amount: amount, Memo: "Paid to provider"},
	}
}

// ReversalLegs is the exact mirror of an entry: every debit becomes a credit
// and every credit a debit, same accounts, same amounts. Posting it undoes
// the original completely (conversion, fee and FX margin included). A
// mirror of a balanced entry is always balanced.
func ReversalLegs(original []Leg) []Leg {
	reversed := make([]Leg, 0, len(original))
	for _, leg := range original {
		oppositeSide := Debit
		if leg.Side == Debit {
			oppositeSide = Credit
		}
		reversed = append(reversed, Leg{AccountID: leg.AccountID, Side: oppositeSide, Amount: leg.Amount, Memo: "Reversal: " + leg.Memo})
	}
	return reversed
}

// ConversionAccounts are the accounts a conversion (swap) touches.
type ConversionAccounts struct {
	Source           uint64 // where the money comes from (usually the user's from-asset wallet)
	SourceClearing   uint64 // Swap Clearing of the from-asset
	SourceFeeRevenue uint64 // Fee Revenue of the from-asset
	NairaClearing    uint64 // Swap Clearing NGN (only used on the NGN rail)
	NairaMargin      uint64 // FX Margin Revenue NGN (only used on the NGN rail)
	TargetClearing   uint64 // Swap Clearing of the to-asset
	Target           uint64 // where the money goes (user's to-asset wallet, a card, or the vault)
}

// ConversionAmounts are the numbers of a conversion, all in smallest units.
type ConversionAmounts struct {
	SourceAmount money.Amount  // what is converted (fee NOT included)
	Fee          money.Amount  // fee in the from-asset
	TargetAmount money.Amount  // what arrives in the to-asset
	NairaIn      *money.Amount // NGN rail: Naira received for selling the from-asset
	NairaOut     *money.Amount // NGN rail: Naira paid for buying the to-asset
}

// ConversionLegs: turn one asset into another, for swaps, card funding,
// paying bills or gift cards with any asset.
//
// Direct route (e.g. USDT -> USDC with one provider):
//
//	Dr Source            sourceAmount + fee   (user gives the from-asset)
//	Cr Source Clearing   sourceAmount         (company now holds it)
//	Cr Fee Revenue       fee
//	Dr Target Clearing   targetAmount         (company gives the to-asset)
//	Cr Target            targetAmount         (user receives it)
//
// NGN rail (e.g. USD -> NGN -> GBP) adds the two Naira legs in the middle,
// so the books show exactly how much Naira we got and spent:
//
//	Dr Naira Clearing    nairaIn   (we sold USD for this much Naira)
//	Cr Naira Clearing    nairaOut  (we spent this much Naira buying GBP)
//	Cr FX Margin Revenue nairaIn - nairaOut (our spread, earned in Naira)
func ConversionLegs(accounts ConversionAccounts, amounts ConversionAmounts) []Leg {
	legs := []Leg{
		{AccountID: accounts.Source, Side: Debit, Amount: amounts.SourceAmount.Add(amounts.Fee), Memo: "Converted from"},
		{AccountID: accounts.SourceClearing, Side: Credit, Amount: amounts.SourceAmount, Memo: "Bought from user"},
	}
	if amounts.Fee.IsPositive() {
		legs = append(legs, Leg{AccountID: accounts.SourceFeeRevenue, Side: Credit, Amount: amounts.Fee, Memo: "Conversion fee"})
	}

	if amounts.NairaIn != nil && amounts.NairaOut != nil {
		nairaIn, nairaOut := *amounts.NairaIn, *amounts.NairaOut
		if nairaIn.IsPositive() {
			legs = append(legs, Leg{AccountID: accounts.NairaClearing, Side: Debit, Amount: nairaIn, Memo: "NGN rail: Naira received"})
		}
		if nairaOut.IsPositive() {
			legs = append(legs, Leg{AccountID: accounts.NairaClearing, Side: Credit, Amount: nairaOut, Memo: "NGN rail: Naira paid"})
		}
		margin := nairaIn.Subtract(nairaOut)
		if margin.IsPositive() {
			legs = append(legs, Leg{AccountID: accounts.NairaMargin, Side: Credit, Amount: margin, Memo: "NGN rail: FX margin"})
		} else if margin.IsNegative() {
			legs = append(legs, Leg{AccountID: accounts.NairaMargin, Side: Debit, Amount: margin.Negate(), Memo: "NGN rail: FX margin (loss)"})
		}
	}

	legs = append(legs,
		Leg{AccountID: accounts.TargetClearing, Side: Debit, Amount: amounts.TargetAmount, Memo: "Sold to user"},
		Leg{AccountID: accounts.Target, Side: Credit, Amount: amounts.TargetAmount, Memo: "Converted to"},
	)
	return legs
}

// FeeLegs: charge a standalone fee (for example the $1 card creation fee).
//
//	Dr User Wallet  fee
//	Cr Fee Revenue  fee
func FeeLegs(userWallet uint64, feeRevenue uint64, fee money.Amount, memo string) []Leg {
	return []Leg{
		{AccountID: userWallet, Side: Debit, Amount: fee, Memo: memo},
		{AccountID: feeRevenue, Side: Credit, Amount: fee, Memo: memo},
	}
}

// MoveLegs: move money between two accounts of the same asset, for example
// user wallet -> cashpoint escrow, card -> wallet, referral -> wallet.
//
//	Dr From  amount
//	Cr To    amount
func MoveLegs(fromAccount uint64, toAccount uint64, amount money.Amount, memo string) []Leg {
	return []Leg{
		{AccountID: fromAccount, Side: Debit, Amount: amount, Memo: memo},
		{AccountID: toAccount, Side: Credit, Amount: amount, Memo: memo},
	}
}

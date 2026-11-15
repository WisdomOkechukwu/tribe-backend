package ledger

import "github.com/tribe-africa/tribe-backend/pkg/money"

// This file holds the POSTING RULES: small functions that say which
// accounts to debit and credit for each kind of event. They do not touch
// the database, so they are easy to read and reason about.
//
// The money itself sits with the PROVIDERS. The books only mirror it:
//   - a provider vault is an ASSET (what the provider holds for us):
//     money arriving at a provider = DEBIT its vault,
//     money leaving a provider     = CREDIT its vault;
//   - a user wallet is a LIABILITY (Tribe owes it to the user):
//     money INTO a user wallet   = CREDIT the user account,
//     money OUT of a user wallet = DEBIT the user account;
//   - Tribe Funds is EQUITY (Tribe's own share of the money at the providers):
//     Tribe earns (a fee, a margin, a top-up) = CREDIT Tribe Funds,
//     Tribe pays (a credit, a reward)         = DEBIT Tribe Funds.

// DepositLegs: money arrives at a provider for a user's wallet.
//
//	Dr Provider Vault (the provider holds more) amount
//	Cr User Wallet    (we owe the user more)    amount
func DepositLegs(providerVault uint64, userWallet uint64, amount money.Amount) []Leg {
	return []Leg{
		{AccountID: providerVault, Side: Debit, Amount: amount, Memo: "Money received"},
		{AccountID: userWallet, Side: Credit, Amount: amount, Memo: "Deposit"},
	}
}

// TreasuryFundingLegs: the owners put Tribe's own money into a provider.
//
//	Dr Provider Vault  amount
//	Cr Tribe Funds     amount (source: treasury)
func TreasuryFundingLegs(providerVault uint64, tribeFunds uint64, amount money.Amount) []Leg {
	return []Leg{
		{AccountID: providerVault, Side: Debit, Amount: amount, Memo: "Treasury funding"},
		{AccountID: tribeFunds, Side: Credit, Amount: amount, Memo: "Treasury funding", Source: SourceTreasury},
	}
}

// CustomerCreditLegs: an admin gives a customer money (goodwill, a refund
// made by hand, a promo). It is paid out of Tribe Funds: Tribe's own share of
// the money at the providers, never another customer's money. No provider
// vault moves: the money is still at the provider, but now we owe it to the
// customer instead of keeping it.
//
//	Dr Tribe Funds  (Tribe keeps less)      amount
//	Cr User Wallet  (we owe the user more)  amount
func CustomerCreditLegs(tribeFunds uint64, userWallet uint64, amount money.Amount) []Leg {
	return []Leg{
		{AccountID: tribeFunds, Side: Debit, Amount: amount, Memo: "Manual customer credit", Source: SourceAdjustment},
		{AccountID: userWallet, Side: Credit, Amount: amount, Memo: "Credit from Tribe"},
	}
}

// CustomerDebitLegs: an admin takes money back out of a customer's wallet
// (a credit made in error, a reversed deposit). The mirror of a credit.
//
//	Dr User Wallet  (we owe the user less) amount
//	Cr Tribe Funds  (Tribe keeps more)     amount
func CustomerDebitLegs(userWallet uint64, tribeFunds uint64, amount money.Amount) []Leg {
	return []Leg{
		{AccountID: userWallet, Side: Debit, Amount: amount, Memo: "Adjustment by Tribe"},
		{AccountID: tribeFunds, Side: Credit, Amount: amount, Memo: "Manual customer debit", Source: SourceAdjustment},
	}
}

// InternalTransferLegs: one Tribe user pays another in the same asset. The
// money stays at the same provider; only who we owe it to changes.
//
//	Dr Sender Wallet    amount + fee
//	Cr Receiver Wallet  amount
//	Cr Tribe Funds      fee (only when there is a fee)
func InternalTransferLegs(senderWallet uint64, receiverWallet uint64, tribeFunds uint64, amount money.Amount, fee money.Amount) []Leg {
	legs := []Leg{
		{AccountID: senderWallet, Side: Debit, Amount: amount.Add(fee), Memo: "Sent to Tribe user"},
		{AccountID: receiverWallet, Side: Credit, Amount: amount, Memo: "Received from Tribe user"},
	}
	if fee.IsPositive() {
		legs = append(legs, Leg{AccountID: tribeFunds, Side: Credit, Amount: fee, Memo: "Transfer fee", Source: SourceFee})
	}
	return legs
}

// PayoutLegs: a user sends money OUT of Tribe (to a bank or wallet). The
// provider is told to pay, so its vault goes down straight away. If it
// refuses, PayoutFailedLegs puts the amount back (the fee is kept).
//
//	Dr User Wallet     amount + fee
//	Cr Provider Vault  amount (the provider pays it out)
//	Cr Tribe Funds     fee
func PayoutLegs(userWallet uint64, providerVault uint64, tribeFunds uint64, amount money.Amount, fee money.Amount) []Leg {
	legs := []Leg{
		{AccountID: userWallet, Side: Debit, Amount: amount.Add(fee), Memo: "Payout"},
		{AccountID: providerVault, Side: Credit, Amount: amount, Memo: "Paid out by provider"},
	}
	if fee.IsPositive() {
		legs = append(legs, Leg{AccountID: tribeFunds, Side: Credit, Amount: fee, Memo: "Payout fee", Source: SourceFee})
	}
	return legs
}

// PayoutFailedLegs: the provider refused the payout, so the money never left
// it. The amount goes back to the user; the fee stays earned.
//
//	Dr Provider Vault  amount
//	Cr User Wallet     amount
func PayoutFailedLegs(providerVault uint64, userWallet uint64, amount money.Amount) []Leg {
	return []Leg{
		{AccountID: providerVault, Side: Debit, Amount: amount, Memo: "Payout refused by provider"},
		{AccountID: userWallet, Side: Credit, Amount: amount, Memo: "Payout refunded"},
	}
}

// ReversalLegs is the exact mirror of an entry: every debit becomes a credit
// and every credit a debit, same accounts, same amounts. Posting it undoes
// the original completely (conversion, fee and margin included). A mirror of
// a balanced entry is always balanced.
func ReversalLegs(original []Leg) []Leg {
	reversed := make([]Leg, 0, len(original))
	for _, leg := range original {
		oppositeSide := Debit
		if leg.Side == Debit {
			oppositeSide = Credit
		}
		reversed = append(reversed, Leg{AccountID: leg.AccountID, Side: oppositeSide, Amount: leg.Amount, Memo: "Reversal: " + leg.Memo, Source: leg.Source})
	}
	return reversed
}

// VaultTransferLegs: money moves from one provider to another in the same
// currency (for example dollars sent to the card issuer to back a card).
//
//	Dr Receiving Vault  amount
//	Cr Sending Vault    amount
func VaultTransferLegs(fromVault uint64, toVault uint64, amount money.Amount, memo string) []Leg {
	return []Leg{
		{AccountID: toVault, Side: Debit, Amount: amount, Memo: memo},
		{AccountID: fromVault, Side: Credit, Amount: amount, Memo: memo},
	}
}

// ConversionAccounts are the accounts a conversion (swap) touches.
type ConversionAccounts struct {
	Source      uint64 // where the money comes from (usually the user's from-asset wallet)
	SourceVault uint64 // the from-asset's home provider vault (it sells the from-asset)
	SourceFunds uint64 // Tribe Funds of the from-asset (the fee lands here)
	NairaVault  uint64 // the Naira home provider vault (NGN rail only)
	NairaFunds  uint64 // Tribe Funds NGN (the rail margin lands here)
	TargetVault uint64 // the to-asset's home provider vault (it buys the to-asset)
	TargetFunds uint64 // Tribe Funds of the to-asset (the margin of other routes lands here)
	Target      uint64 // where the money goes (a user wallet, a card, or a vault that pays a third party)
}

// ConversionAmounts are the numbers of a conversion, all in smallest units.
//
// Our margin is recorded in one of two ways:
//   - NGN rail: NairaIn and NairaOut, the Naira value of each side at market.
//     The gap is the margin, earned in Naira.
//   - Any other route: MarketReceived, what the provider gives us at the
//     market rate. The gap to TargetAmount is the margin, in the to-asset.
type ConversionAmounts struct {
	SourceAmount   money.Amount  // what is converted (fee NOT included)
	Fee            money.Amount  // fee in the from-asset
	TargetAmount   money.Amount  // what arrives for the user, in the to-asset
	NairaIn        *money.Amount // NGN rail: Naira received for selling the from-asset
	NairaOut       *money.Amount // NGN rail: Naira paid for buying the to-asset
	MarketReceived *money.Amount // other routes: the to-asset amount at market, before our margin
}

// ConversionLegs: turn one asset into another, for swaps, card funding,
// paying bills or gift cards with any asset. The providers do the actual
// converting: the from-asset leaves its home vault, the to-asset arrives in
// its home vault.
//
// Direct route (USDT -> USDC at the crypto provider). It sells 100 USDT and
// gives us 100.00 USDC at market; the user gets 99.80:
//
//	Dr Source             sourceAmount + fee   (user gives USDT)
//	Cr Source Vault       sourceAmount         (the provider sells it)
//	Cr Tribe Funds        fee                  (USDT)
//	Dr Target Vault       marketReceived       (the provider buys USDC)
//	Cr Target             targetAmount         (user receives it)
//	Cr Tribe Funds        marketReceived - targetAmount (USDC margin)
//
// NGN rail (USD -> NGN -> GBP) records the Naira in the middle instead:
//
//	Dr Naira Vault        nairaIn   (USD sold for this much Naira)
//	Cr Naira Vault        nairaOut  (Naira spent buying GBP)
//	Cr Tribe Funds NGN    nairaIn - nairaOut (our margin)
//	Dr Target Vault       targetAmount (the GBP bought)
//
// A negative margin (a loss) is a DEBIT of Tribe Funds instead.
func ConversionLegs(accounts ConversionAccounts, amounts ConversionAmounts) []Leg {
	legs := []Leg{
		{AccountID: accounts.Source, Side: Debit, Amount: amounts.SourceAmount.Add(amounts.Fee), Memo: "Converted from"},
		{AccountID: accounts.SourceVault, Side: Credit, Amount: amounts.SourceAmount, Memo: "Sold by provider"},
	}
	if amounts.Fee.IsPositive() {
		legs = append(legs, Leg{AccountID: accounts.SourceFunds, Side: Credit, Amount: amounts.Fee, Memo: "Conversion fee", Source: SourceFee})
	}

	if amounts.NairaIn != nil && amounts.NairaOut != nil {
		nairaIn, nairaOut := *amounts.NairaIn, *amounts.NairaOut
		if nairaIn.IsPositive() {
			legs = append(legs, Leg{AccountID: accounts.NairaVault, Side: Debit, Amount: nairaIn, Memo: "NGN rail: Naira received"})
		}
		if nairaOut.IsPositive() {
			legs = append(legs, Leg{AccountID: accounts.NairaVault, Side: Credit, Amount: nairaOut, Memo: "NGN rail: Naira paid"})
		}
		legs = append(legs, marginLegs(accounts.NairaFunds, nairaIn.Subtract(nairaOut), "NGN rail: FX margin")...)
	}

	// What the provider gives us in the to-asset: the market value when we
	// know it (the margin is then the gap), otherwise exactly what the user gets.
	targetCost := amounts.TargetAmount
	if amounts.MarketReceived != nil {
		targetCost = *amounts.MarketReceived
	}
	if targetCost.IsPositive() {
		legs = append(legs, Leg{AccountID: accounts.TargetVault, Side: Debit, Amount: targetCost, Memo: "Bought by provider"})
	}
	legs = append(legs, Leg{AccountID: accounts.Target, Side: Credit, Amount: amounts.TargetAmount, Memo: "Converted to"})
	if amounts.MarketReceived != nil {
		legs = append(legs, marginLegs(accounts.TargetFunds, targetCost.Subtract(amounts.TargetAmount), "Conversion margin")...)
	}
	return legs
}

// marginLegs books a conversion margin in Tribe Funds: a credit when we
// earned it, a debit when it was a loss, nothing when it is exactly zero.
func marginLegs(tribeFunds uint64, margin money.Amount, memo string) []Leg {
	if margin.IsPositive() {
		return []Leg{{AccountID: tribeFunds, Side: Credit, Amount: margin, Memo: memo, Source: SourceMargin}}
	}
	if margin.IsNegative() {
		return []Leg{{AccountID: tribeFunds, Side: Debit, Amount: margin.Negate(), Memo: memo + " (loss)", Source: SourceMargin}}
	}
	return nil
}

// FeeLegs: charge a standalone fee (for example the $1 card creation fee).
//
//	Dr User Wallet  fee
//	Cr Tribe Funds  fee
func FeeLegs(userWallet uint64, tribeFunds uint64, fee money.Amount, memo string) []Leg {
	return []Leg{
		{AccountID: userWallet, Side: Debit, Amount: fee, Memo: memo},
		{AccountID: tribeFunds, Side: Credit, Amount: fee, Memo: memo, Source: SourceFee},
	}
}

// MoveLegs: move money between two accounts of the same asset, for example
// user wallet -> cashpoint codes, card -> wallet, referral -> wallet, or a
// wallet -> the vault of a provider that pays a third party (a bill).
//
//	Dr From  amount
//	Cr To    amount
func MoveLegs(fromAccount uint64, toAccount uint64, amount money.Amount, memo string) []Leg {
	return []Leg{
		{AccountID: fromAccount, Side: Debit, Amount: amount, Memo: memo},
		{AccountID: toAccount, Side: Credit, Amount: amount, Memo: memo},
	}
}

// ReferralRewardLegs: Tribe pays an inviter their share of what it earned on
// a friend's transaction. It comes out of Tribe Funds NGN; no provider vault
// moves (the Naira is still at the provider, but now we owe it to the inviter).
//
//	Dr Tribe Funds NGN         amount (source: referral_reward)
//	Cr Inviter's referral NGN  amount
func ReferralRewardLegs(tribeFundsNaira uint64, inviterReferral uint64, amount money.Amount) []Leg {
	return []Leg{
		{AccountID: tribeFundsNaira, Side: Debit, Amount: amount, Memo: "Referral reward", Source: SourceReferralReward},
		{AccountID: inviterReferral, Side: Credit, Amount: amount, Memo: "Referral reward"},
	}
}

# How Tribe keeps its books

Tribe uses **double-entry accounting** in the Nexum style. This page explains the rules, the accounts and every posting the code makes, with numbers. The code lives in `app/services/ledger/`.

> **Status:** the gift-card and cashpoint postings are marked **pending accountant sign-off**. Settlement, payroll and period close are **not built yet** (only placeholder tables exist).

---

## 1. Three ideas

1. **Every account has a category, and the category decides which side increases it.**

   | Category | Increases with | Decreases with | Example |
   |---|---|---|---|
   | asset | debit | credit | Company Vault |
   | expense | debit | credit | Referral Expense |
   | contra_asset | debit | credit | (not used yet) |
   | suspense_asset | debit | credit | (not used yet) |
   | liability | credit | debit | a user's wallet |
   | revenue | credit | debit | Fee Revenue |
   | equity | credit | debit | Treasury Funding |
   | contra_liability | credit | debit | (not used yet) |
   | suspense_liability | credit | debit | Swap Clearing, Payouts In Transit, Purchases In Transit |
   | summary | credit | debit | Income Summary (for period close, later) |

   `ledger.NormalSideForCategory` returns this, and a unit test pins it to the table.

2. **Every money movement is a journal entry made of legs**, and **for every asset in the entry, debits = credits**. `ledger.Post` refuses anything else. Money is never created or destroyed: it only moves between accounts.

3. **A user's wallet is a liability.** It is money Tribe owes the user. When a user receives ₦100 we **credit** their wallet (we owe more). When they spend it we **debit** it (we owe less).

## 2. The accounts

**Per user, per asset**

| Code | Name | Category |
|---|---|---|
| `user_primary` | the wallet | liability |
| `user_referral` | referral earnings waiting to be withdrawn (NGN) | liability |
| `user_card` | the balance on one virtual card (USD, one account per card) | liability |

**Company, one of each per asset** (`ledger.CompanyAccountTemplates`)

| Code | Name | Category | What it means |
|---|---|---|---|
| `company_vault` | Company Vault | asset | money we actually hold at banks and providers |
| `treasury_funding` | Treasury Funding | equity | money the owners put in |
| `swap_clearing` | Swap Clearing | suspense_liability | assets bought from or sold to users during conversions, waiting for provider settlement |
| `payout_in_transit` | Payouts In Transit | suspense_liability | money sent out that the provider has not confirmed yet |
| `purchase_in_transit` | Purchases In Transit | suspense_liability | bills and gift cards the customer paid for that the provider has not confirmed yet |
| `fee_revenue` | Fee Revenue | revenue | fees we charge |
| `fx_margin_revenue` | FX Margin Revenue | revenue | our spread on the NGN rail |
| `provider_expense` | Provider Expense | expense | what providers charge us |
| `referral_expense` | Referral Expense | expense | bonuses we pay for invites |
| `customer_adjustments` | Customer Adjustments | expense | manual credits to customers (debits reduce it); created on first use |
| `cashpoint_escrow` | Cashpoint Escrow | liability | Naira held for cashpoint codes not yet collected |
| `summary` | Income Summary | summary | used by period close (later) |

## 3. Balances: snapshot + legs

There is **no balance column**. A balance is always:

```
balance = latest daily snapshot's closing_balance
        + signed sum of legs with id > snapshot.last_journal_leg_id
```

"Signed" means + for the increasing side and − for the other (table above). One function, `ledger.BalanceOf`, does this everywhere.

**Why it is correct:** a leg for an account is only inserted while that account's row is locked (`SELECT … FOR UPDATE`). So, for one account, leg ids grow in commit order, and "every leg after id N" never misses a late commit.

**Daily snapshots** (`ledger:daily-snapshot`, 00:05 Lagos time):

- For each account, in its own short transaction, the job locks the row, takes yesterday's snapshot plus the legs since, and writes today's row.
- UNIQUE(account, date) makes a second run do nothing.
- Backfill a day with `artisan ledger:snapshot 2026-10-23`.

**Checks:**

- `artisan ledger:verify` and the 00:30 cron prove two things: every entry balances per asset, and every snapshot equals the full sum of its legs.
- The admin "Verify ledger" button runs the same check.

## 4. How a posting is written (`ledger.Post`)

1. Validate the legs: at least 2, every amount > 0, the side is debit or credit, and the account is set.
2. Insert the `journal_entries` header. Its `reference` is UNIQUE and built from the request's idempotency record (e.g. `SWP-01J…:POST`). If it already exists, return the old entry: **a retry never posts twice.**
3. Lock every touched account with `FOR UPDATE`, **in ascending id order**. Opposite transfers (A→B and B→A) therefore never deadlock.
4. Check debits = credits **per asset**.
5. For user accounts, refuse to go below zero (`INSUFFICIENT_FUNDS`).
6. Insert the legs. `journal_legs` is insert-only: a database trigger blocks UPDATE and DELETE. Mistakes are fixed with `ledger.Reverse`, which posts the mirror entry.

Every leg also stores the asset's USD and NGN rate at that moment, so reports can value it later.

## 5. Every posting, with numbers

Amounts are in smallest units in the database. They are shown here in whole units for reading. The rules are pure functions in `rules.go`, each with a test in `rules_test.go` proving it balances.

### Test top-up (seeders and tests only, no route) `DepositLegs`
User gets $100.
| Account | Dr | Cr |
|---|---|---|
| Company Vault USD | 100 | |
| User wallet USD | | 100 |

### Treasury funding `TreasuryFundingLegs` (two admins)
1. An admin **requests** it. Nothing is posted.
2. A **different** admin approves it, and only then is the entry posted.

| Account | Dr | Cr |
|---|---|---|
| Company Vault USD | 5,000 | |
| Treasury Funding USD | | 5,000 |

### Manual customer credit `CustomerCreditLegs` (two admins, super admin approves)
An admin requests, a **different** admin who is a super admin approves, and only then is it posted.
The credit must fit the vault's **free funds** (`ledger.FreeFundsOf`):
`free = Company Vault − (user wallets + referral + card balances + Cashpoint Escrow + Payouts In Transit + Purchases In Transit)` in that currency.
Swap Clearing is left out on purpose: it can only make free funds smaller. The vault does not move (the money is still at the bank); we now owe it to the customer, and the cost is an expense.
Approvals in one currency queue up on a lock on that currency's Customer Adjustments account, so two approvals cannot both spend the same free funds.

Ada gets $30 of goodwill.
| Account | Dr | Cr |
|---|---|---|
| Customer Adjustments USD (expense) | 30 | |
| Ada wallet USD | | 30 |

### Manual customer debit `CustomerDebitLegs` (two admins, super admin approves)
The mirror of a credit: money taken back out of a wallet (for example a credit made in error). It must fit the wallet balance (`ledger.Post` also refuses to take a user wallet below zero).
| Account | Dr | Cr |
|---|---|---|
| Ada wallet USD | 30 | |
| Customer Adjustments USD (expense) | | 30 |

### Tribe-to-Tribe send `InternalTransferLegs`
Ada sends Bola $20 with a $0.10 fee.
| Account | Dr | Cr |
|---|---|---|
| Ada wallet USD | 20.10 | |
| Bola wallet USD | | 20.00 |
| Fee Revenue USD | | 0.10 |

### Send to a bank or wallet `PayoutStartLegs` → `PayoutSucceededLegs` / `PayoutFailedLegs`
Ada sends $50 out with a $0.50 fee. The money is taken now and parked.
| Account | Dr | Cr |
|---|---|---|
| Ada wallet USD | 50.50 | |
| Payouts In Transit USD | | 50.00 |
| Fee Revenue USD | | 0.50 |

The worker asks the provider. A webhook also triggers this check, but the webhook body is never trusted.

- Delivered: Dr Payouts In Transit 50 / Cr Company Vault 50. The money left the company.
- Failed: Dr Payouts In Transit 50 / Cr Ada wallet 50. A refund of the amount. The rule does not refund the fee today; confirm with the accountant whether a failed payout should also return it.

### Swap: direct route `ConversionLegs`
USDT → USDC on the crypto provider (rail off). 100 USDT converted, fee 0.50, user gets 99.80 USDC.
| Account | Dr | Cr |
|---|---|---|
| User wallet USDT | 100.50 | |
| Swap Clearing USDT | | 100.00 |
| Fee Revenue USDT | | 0.50 |
| Swap Clearing USDC | 99.80 | |
| User wallet USDC | | 99.80 |

Each asset balances on its own: USDT has 100.50 = 100 + 0.50, and USDC has 99.80 = 99.80.

### Swap: NGN rail `ConversionLegs` with Naira legs
USD → GBP. The global provider has `uses_ngn_rail` on, so the route is USD → NGN → GBP. We convert $20 (fee $0.10).

- Selling $20 brings in ₦30,773.
- Buying £15.03 costs ₦30,606.
- The difference is our margin.

| Account | Dr | Cr |
|---|---|---|
| User wallet USD | 20.10 | |
| Swap Clearing USD | | 20.00 |
| Fee Revenue USD | | 0.10 |
| Swap Clearing NGN | 30,773 | |
| Swap Clearing NGN | | 30,606 |
| FX Margin Revenue NGN | | 167 |
| Swap Clearing GBP | 15.03 | |
| User wallet GBP | | 15.03 |

The Naira legs make the rail **visible in the books**: how much Naira we received and paid, and what we earned. If the spread is negative, the margin leg is a debit (a loss). Each hop is also saved in `swap_legs` with its rate and provider.

### Card funding, bills and buying gift cards (`PostConversion`)
The same conversion shape. Only the **target** changes:

| Product | Target account | Notes |
|---|---|---|
| Fund a card | that card's `user_card` USD account | Withdraw is the reverse, back into a wallet |
| Pay a bill | Purchases In Transit NGN | then settled or reversed, see below |
| Buy a gift card | Purchases In Transit USD | then settled or reversed, see below; **pending accountant sign-off** |

If the user pays in the same asset, there are no clearing legs: just Dr wallet / Cr target (plus the fee).

### Bills and gift cards wait in Purchases In Transit (`PurchaseSucceededLegs` / `ReversalLegs`)
The biller or gift card provider is called **after** the money is taken, outside any database transaction, so a slow provider never holds row locks. Until it answers, the money waits in Purchases In Transit (like a payout). Ada pays a ₦5,000 bill from NGN with a ₦50 fee:

| Entry | Account | Dr | Cr |
|---|---|---|---|
| `:POST` | Ada wallet NGN | 5,050 | |
| | Purchases In Transit NGN | | 5,000 |
| | Fee Revenue NGN | | 50 |
| `:SETTLE` (biller confirmed) | Purchases In Transit NGN | 5,000 | |
| | Company Vault NGN | | 5,000 |

- **Confirmed** (`PurchaseSucceededLegs`): the money leaves the vault. After both entries the result is exactly what the old one-step posting gave (Dr Ada 5,050 / Cr Vault 5,000 / Cr Fee 50).
- **Refused** by the provider (a final "no"): the `:POST` entry is **reversed** (`ReversalLegs`, reference `…:POST:REVERSAL`). Every leg is mirrored, so Ada gets back 5,050 (the fee too) and, for a purchase paid in another currency, the clearing and FX-margin legs are undone as well. Unlike a failed payout, the fee is refunded; confirm with the accountant.
- **No answer** (timeout): nothing is posted. The purchase stays pending and is asked about again (queue job, then the 5-minute sweeper). A timeout is never treated as a refusal, because the provider may have delivered.

At any moment, Purchases In Transit equals the purchases still waiting for their provider. It counts as money owed to customers in free funds.

### Selling a gift card (**pending accountant sign-off**)
The user sells a $50 card for ₦74,000. The market value is ₦76,900.
| Account | Dr | Cr |
|---|---|---|
| Company Vault USD | 50 | |
| Swap Clearing USD | | 50 |
| Swap Clearing NGN | 76,900 | |
| Swap Clearing NGN | | 74,000 |
| FX Margin Revenue NGN | | 2,900 |
| Swap Clearing NGN | 74,000 | |
| User wallet NGN | | 74,000 |

### Cashpoints `MoveLegs` (**pending accountant sign-off**)
- **Create** ₦10,000: Dr User wallet NGN / Cr Cashpoint Escrow NGN.
- **Collect** (another user redeems the code): Dr Cashpoint Escrow / Cr the collector's wallet.
- **Cancel**: Dr Cashpoint Escrow / Cr the creator's wallet.

### Referrals `MoveLegs`
- **Award**: Dr Referral Expense NGN / Cr User referral NGN.
- **Withdraw**: Dr User referral NGN / Cr User wallet NGN.

### Delete a card `MoveLegs`
Dr Card USD / Cr User wallet USD (the remaining card balance).

### Standalone fee `FeeLegs`
For example the card creation fee: Dr User wallet / Cr Fee Revenue.

## 6. Pricing (where margins and fees come from)
- **Rates:** the collector stores every asset's USD and NGN rate every 10 s (`exchange_rates`, `exchange_rate_latest`).
- **Margins:** `rate_margins` sets a buy and a sell margin % per asset. Selling uses the rate lowered by the sell margin; buying uses it raised by the buy margin. On the rail, the Naira difference is the FX margin above.
- **Fees:** `transaction_fees` sets, per transaction type, a percent, a flat fee (in `flat_fee_asset`), a cap and an active flag. Fees round **up**. Amounts paid to users round **down**.
- **Maths:** everything is exact fractions (`big.Rat`), rounded once at the end. Floats are never used.

## 7. Later (placeholders only)
| Table | Future feature |
|---|---|
| `settlement_sessions` | Clearing the Swap Clearing accounts against real provider settlements |
| `payroll_runs` | Paying staff from the books |
| `period_close_runs` | Monthly close: move revenue and expense into Income Summary, then equity |

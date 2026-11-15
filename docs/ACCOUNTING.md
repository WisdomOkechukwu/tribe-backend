# How Tribe keeps its books

Tribe uses **double-entry accounting** in the Nexum style. This page explains the rules, the accounts and every posting the code makes, with numbers. The code lives in `app/services/ledger/`.

> **Status:** the gift-card and cashpoint postings are marked **pending accountant sign-off**. Settlement, payroll and period close are **not built yet** (only placeholder tables exist).

**The providers hold the money; Tribe's books mirror them** (since 2026-11-15, migration `00017`). Crypto Desk holds the coins, Global FX the fiat, the card issuer the money on cards, the biller and the gift card provider their floats. Tribe is not a treasury keeper, so the ledger has only three kinds of account:

| Kind | Accounts | Category | What it is |
|---|---|---|---|
| **Provider vaults** | `provider_vault`, owner `provider:<id>`, one per provider per currency it handles (`provider_assets`) | asset | what that provider holds for Tribe, e.g. "Crypto Desk · BTC", "Global FX · USD", "Bills · NGN" |
| **Customer balances** | `user_primary`, `user_referral`, `user_card`, `user_cashpoint` | liability | what Tribe owes its customers |
| **Tribe Funds** | `tribe_funds`, one per currency | equity | Tribe's own share of the money in the vaults |

Because every entry balances per currency, **in every currency**:

```
Tribe Funds = Σ provider vaults − Σ owed to customers
```

That is not a separate calculation that could drift: it is what double entry guarantees. Fees, margins and treasury top-ups go **into** Tribe Funds; manual credits and referral rewards come **out** of it. The admin Inventory page shows both sides of the equation per currency, and every integration test checks it (`assertBooksBalance`).

Every Tribe Funds leg carries a **source** (`journal_legs.earning_source`): `fee`, `margin`, `treasury`, `adjustment` or `referral_reward`, so one account can still be split in reports.

**Home provider.** Each currency has one home provider (`assets.conversion_provider_id`): deposits, manual credits, conversions and payouts of that currency use its vault. Products with their own provider use theirs: bills the biller's provider (`billers.provider_id`), gift cards the brand's provider, cards the card issuer.

Retired on 2026-11-15: Company Vault, Treasury Funding, Swap Clearing, Payouts In Transit, Purchases In Transit, Earnings Vault, Referral Vault, Provider Expense, Cashpoint Escrow and Income Summary. The change was made on a fresh database.

---

## 1. Three ideas

1. **Every account has a category, and the category decides which side increases it.**

   | Category | Increases with | Decreases with | Example |
   |---|---|---|---|
   | asset | debit | credit | a provider vault |
   | liability | credit | debit | a user's wallet |
   | equity | credit | debit | Tribe Funds |

   (`expense`, `revenue`, the contra, suspense and summary categories still exist in `ledger.NormalSideForCategory` for later, but no account uses them.)

2. **Every money movement is a journal entry made of legs**, and **for every asset in the entry, debits = credits**. `ledger.Post` refuses anything else. Money is never created or destroyed: it only moves between accounts.

3. **A user's wallet is a liability, a provider vault is an asset.** When a user deposits ₦100 at Global FX we **debit** Global FX's vault (it holds more) and **credit** the wallet (we owe more).

## 2. Balances: snapshot + legs

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

## 3. How a posting is written (`ledger.Post`)

1. Validate the legs: at least 2, every amount > 0, the side is debit or credit, and the account is set.
2. Insert the `journal_entries` header. Its `reference` is UNIQUE and built from the request's idempotency record (e.g. `SWP-01J…:POST`). If it already exists, return the old entry: **a retry never posts twice.**
3. Lock every touched account with `FOR UPDATE`, **in ascending id order**. Opposite transfers (A→B and B→A) therefore never deadlock.
4. Check debits = credits **per asset**.
5. For user accounts, refuse to go below zero (`INSUFFICIENT_FUNDS`). Provider vaults and Tribe Funds may go below zero; Inventory flags it.
6. Insert the legs. `journal_legs` is insert-only: a database trigger blocks UPDATE and DELETE. Mistakes are fixed with `ledger.Reverse`, which posts the mirror entry.

Every leg also stores the asset's USD and NGN rate at that moment, so reports can value it later.

## 4. Every posting, with numbers

Amounts are in smallest units in the database. They are shown here in whole units for reading. The rules are pure functions in `rules.go`, each with a test in `rules_test.go` proving it balances; `TestTribeFundsAlwaysEqualsVaultsMinusOwed` replays all of them and checks the rule above after each.

### Deposit / test top-up `DepositLegs`
Ada gets $100 (it arrives at Global FX, USD's home provider).
| Account | Dr | Cr |
|---|---|---|
| Global FX · USD | 100 | |
| Ada wallet USD | | 100 |

### Treasury funding `TreasuryFundingLegs` (two admins)
1. An admin **requests** it, choosing the provider vault (`treasury_fundings.provider_id`; empty = the home provider). Nothing is posted.
2. A **different** admin approves it, and only then is the entry posted.

| Account | Dr | Cr |
|---|---|---|
| Bills · NGN | 2,000,000 | |
| Tribe Funds NGN (`treasury`) | | 2,000,000 |

The seeder funds opening floats this way (the bills and gift card providers, and Tribe's own Naira, dollars and USDT).

### Manual customer credit `CustomerCreditLegs` (two admins, super admin approves)
Paid from the currency's **Tribe Funds**, and refused when it is short (`INSUFFICIENT_TRIBE_FUNDS`): only Tribe's own share of the money at the providers is ever handed out. No vault moves; we now owe the money to the customer instead of keeping it. Approvals in one currency queue up on a lock on that currency's Tribe Funds.
| Account | Dr | Cr |
|---|---|---|
| Tribe Funds USD (`adjustment`) | 30 | |
| Ada wallet USD | | 30 |

### Manual customer debit `CustomerDebitLegs`
The mirror: Dr Ada wallet / Cr Tribe Funds (`adjustment`). It must fit the wallet.

### Tribe-to-Tribe send `InternalTransferLegs`
The money stays at the same provider; only who we owe it to changes. Ada sends Bola $20 with a $0.10 fee.
| Account | Dr | Cr |
|---|---|---|
| Ada wallet USD | 20.10 | |
| Bola wallet USD | | 20.00 |
| Tribe Funds USD (`fee`) | | 0.10 |

### Send to a bank or wallet `PayoutLegs` / `PayoutFailedLegs`
The home provider is told to pay, so its vault goes down **at once**. Ada sends $50 out with a $0.50 fee:
| Account | Dr | Cr |
|---|---|---|
| Ada wallet USD | 50.50 | |
| Global FX · USD | | 50.00 |
| Tribe Funds USD (`fee`) | | 0.50 |

The worker asks the provider (a webhook also triggers this check, but its body is never trusted).
- Delivered: nothing more to post; the payout is completed.
- Refused: the money never left the provider. `:REFUND` posts Dr Global FX · USD 50 / Cr Ada wallet 50. The fee is kept (confirm with the accountant). The vault is the one saved on the transaction (`payout_vault_account_id`), even if the home provider changed since.

### Swap: direct route `ConversionLegs`
USDT → USDC at Crypto Desk. 100 USDT converted, fee 0.50. At market the 100 USDT buy 100.00 USDC; after our margin the user gets 99.80.
| Account | Dr | Cr |
|---|---|---|
| Ada wallet USDT | 100.50 | |
| Crypto Desk · USDT (sold) | | 100.00 |
| Tribe Funds USDT (`fee`) | | 0.50 |
| Crypto Desk · USDC (bought, at market) | 100.00 | |
| Ada wallet USDC | | 99.80 |
| Tribe Funds USDC (`margin`) | | 0.20 |

The market value is worked out when the quote is priced (`rates.Price.MarketReceived`, saved as `quotes.market_to_amount`), so every route that is not the NGN rail books its margin in the to-asset. A negative margin is a debit of Tribe Funds.

### Swap: NGN rail `ConversionLegs` with Naira legs
USD → GBP at Global FX with `uses_ngn_rail` on: USD → NGN → GBP. We convert $20 (fee $0.10). Selling $20 brings in ₦30,773; buying £15.03 costs ₦30,606.
| Account | Dr | Cr |
|---|---|---|
| Ada wallet USD | 20.10 | |
| Global FX · USD (sold) | | 20.00 |
| Tribe Funds USD (`fee`) | | 0.10 |
| Global FX · NGN (Naira received) | 30,773 | |
| Global FX · NGN (Naira paid) | | 30,606 |
| Tribe Funds NGN (`margin`) | | 167 |
| Global FX · GBP (bought) | 15.03 | |
| Ada wallet GBP | | 15.03 |

Each hop is also saved in `swap_legs` with its rate and provider.

### Cards (`PostConversion` + `VaultTransferLegs`)
The card issuer holds the money on every card, so each card movement also moves the dollars between USD's home provider and the issuer. Ada funds a card with $25 from her USD wallet:
| Account | Dr | Cr |
|---|---|---|
| Ada wallet USD | 25.25 | |
| Ada's card USD | | 25.00 |
| Tribe Funds USD (`fee`) | | 0.25 |
| Card Issuer · USD | 25.00 | |
| Global FX · USD | | 25.00 |

Withdrawing and closing a card do the reverse transfer, so **the card issuer's vault always equals the money on its cards** (checked by every integration test).

### Bills and gift cards (`PostConversion`, then `ReversalLegs` on a refusal)
The money leaves the product provider's vault when it is posted. Ada pays a ₦5,000 bill from NGN with a ₦50 fee:
| Account | Dr | Cr |
|---|---|---|
| Ada wallet NGN | 5,050 | |
| Bills · NGN (pays the biller) | | 5,000 |
| Tribe Funds NGN (`fee`) | | 50 |

The biller is then asked **outside** any database transaction.
- **Confirmed:** nothing more to post; the bill is completed (and a waiting referral share is paid).
- **Refused** (a final "no"): the `:POST` entry is **reversed** (`…:POST:REVERSAL`): the vault, the wallet (fee included), and any conversion and margin legs all go back.
- **No answer:** nothing is posted; it stays pending and is asked about again. A timeout is never treated as a refusal.

A gift card purchase is the same with the gift card provider's dollar vault (**pending accountant sign-off**). Paid in another currency, the conversion's target is that vault.

### Selling a gift card (**pending accountant sign-off**)
The user sells a $50 card for ₦74,000; the market value is ₦76,900. The gift card provider credits us the card's value, Global FX sells the dollars, the user is paid:
| Account | Dr | Cr |
|---|---|---|
| Gift Cards · USD (credited the card) | 50 | |
| Global FX · USD (sold) | | 50 |
| Global FX · NGN (Naira received) | 76,900 | |
| Global FX · NGN (Naira paid) | | 74,000 |
| Tribe Funds NGN (`margin`) | | 2,900 |
| Global FX · NGN (bought) | 74,000 | |
| Ada wallet NGN | | 74,000 |

### Cashpoints `MoveLegs` (**pending accountant sign-off**)
The Naira stays at the provider the whole time; only who it is owed to changes.
- **Create** ₦10,000: Dr creator's wallet / Cr creator's cashpoint balance (`user_cashpoint`).
- **Collect**: Dr creator's cashpoint balance / Cr the collector's wallet.
- **Cancel**: Dr creator's cashpoint balance / Cr creator's wallet.

### Referrals: a share of earnings, paid from Tribe Funds (`ReferralRewardLegs`)
When a customer who was **invited** transacts, the **referral share** (admin setting `referral_settings.share_percent`, 2% to start) of what Tribe earned on it goes to the person who invited them. Customers nobody invited share nothing. Code: `app/services/referrals`, and `ledger.Entry.ReferralShare`.

**How much.** `ledger.Post` reads the entry's own legs: in each currency, the `fee` and `margin` credits to Tribe Funds minus its `fee`/`margin` debits. Each positive amount × the share is rounded **down**, valued in Naira at the market rate stored on the entry, and rounded down again. Shares under one smallest unit are not paid.

**Paid** out of Tribe Funds NGN (Tribe's other-currency earnings are not converted):
| Account | Dr | Cr |
|---|---|---|
| Tribe Funds NGN (`referral_reward`) | 15.40 | |
| Inviter's referral balance NGN | | 15.40 |

- Swaps, Tribe sends, cards and gift card sales are final at once: the reward is in the **same entry**.
- Bills, gift card purchases and payouts wait for a provider: the share is only recorded (`referral_earnings.status = accrued`). When the provider confirms (or a payout is refused but its fee kept), `referrals.PayAccrued` posts it as `REFERRAL-<earning id>:PAY`. If a purchase is refused, the earning is `cancelled`; nothing was paid.
- Rewards are paid even if Tribe Funds NGN is short (Tribe owes them); Inventory then shows Tribe Funds negative so an admin funds it.

Each share is one row in `referral_earnings` (`transaction_id` UNIQUE). The inviter gets an inbox message naming the friend and the kind of transaction, never its details.

- **Withdraw**: Dr User referral NGN / Cr User wallet NGN (`MoveLegs`).

### Standalone fee `FeeLegs`
Dr User wallet / Cr Tribe Funds (`fee`).

## 5. Pricing (where margins and fees come from)
- **Rates:** the collector stores every asset's USD and NGN rate every 10 s (`exchange_rates`, `exchange_rate_latest`).
- **Margins:** `rate_margins` sets a buy and a sell margin % per asset. Selling uses the rate lowered by the sell margin; buying uses it raised by the buy margin. On the rail, the Naira difference is the margin; on every other route it is the market value of what is received minus what the user gets. Both land in Tribe Funds.
- **Fees:** `transaction_fees` sets, per transaction type, a percent, a flat fee (in `flat_fee_asset`), a cap and an active flag. Fees round **up**. Amounts paid to users round **down**.
- **Maths:** everything is exact fractions (`big.Rat`), rounded once at the end. Floats are never used.

## 6. Later (placeholders only)
| Table | Future feature |
|---|---|
| `settlement_sessions` | Clearing the Swap Clearing accounts against real provider settlements |
| `payroll_runs` | Paying staff from the books |
| `period_close_runs` | Monthly close: move revenue and expense into Income Summary, then equity |

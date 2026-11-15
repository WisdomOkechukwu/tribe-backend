# Vaults: the proposal (superseded on 2026-11-15)

> **Status:** superseded the same day by **provider vaults** (`docs/ACCOUNTING.md`): the owner decided the providers, not Tribe, hold the money, so the ledger now has only provider vaults, customer balances and Tribe Funds. The Earnings and Referral Vaults below were built and then removed; the referral percentage rule (answer 4) was kept, paid from Tribe Funds with no pot. This page is kept for the history of the decision.
>
> **The owner's answers (2026-11-15):**
> 1. Customer credits come from the **Earnings Vault**, refused when it is short.
> 2. **One** Earnings Vault per currency; the source (fee, margin, referral share, adjustment) is a tag on each leg.
> 3. The Referral Vault is **funded automatically** by each transaction of an invited customer: the referral share (an admin setting, 2% to start) of what Tribe earned goes to it, the rest stays in the Earnings Vault, and the vault pays the inviter. There is no admin "fund the pot" flow, and customers nobody invited fund nothing.
> 4. There is **no flat ₦25**. The reward is the referral share of the earnings, converted to Naira at the market rate. It applies to every kind of earning transaction; a share under one smallest unit rounds down to nothing, so tiny transactions cannot farm it.
>
> The local `tribe_core` database was rebuilt from scratch for this change (no opening-balance entries were needed).

The request: because money really sits with providers, the books should be organised around **vaults**: one per currency that mirrors what the provider holds, one for company earnings, one for referral earnings, and customers are credited from a vault.

This page says what already matches that, what does not, the design I recommend, and the decisions only the owner (and the accountant) can make.

---

## 1. What exists today, in vault terms

| What you asked for | What the ledger has | Matches? |
|---|---|---|
| A vault per currency, mirroring the provider | `company_vault`, one per currency ("money held at providers and banks"). Admin → Inventory shows them. | **Yes.** One per currency, not one per provider. |
| Customers are credited from the vault | A manual credit posts `customer_adjustments → wallet` and is refused unless it fits the vault's **free funds** (`ledger.FreeFundsOf`: vault minus everything owed to customers). | **In effect, yes.** The credit can never exceed what the vault holds beyond customers' money. But it is recorded against an expense account, not against a named pot. |
| A vault for company earnings | Two revenue accounts per currency: `fee_revenue` and `fx_margin_revenue`. | **Partly.** See the two gaps below. |
| A vault for referral earnings | `referral_expense` (may go below zero) pays into each customer's `user_referral` balance. | **No.** There is no pot that can run out, and nothing funds it. |

Two gaps that matter for an earnings vault:

1. **Conversion margin is only recorded on the Naira rail.** A direct conversion (USDT → USDC) and any conversion with Naira on one side post no margin leg: the spread stays inside `swap_clearing`, mixed with money in transit. So "what we earned" is understated today.
2. **The ₦25 referral reward is not triggered by anything.** `AwardReferral` exists, but its only caller is the seeder. No customer transaction awards it.

Everything else in the chart (`swap_clearing`, `payout_in_transit`, `purchase_in_transit`, `cashpoint_escrow`) is plumbing: it holds money for the seconds or hours between "left the customer" and "arrived". It is what keeps each currency's debits equal to its credits, so it stays, out of sight.

---

## 2. Recommended design

Keep double entry (it is what makes the books provable) and present it as four kinds of pot per currency:

| Pot | Account | Rule |
|---|---|---|
| **Currency vault** | `company_vault` (unchanged) | Real money at the provider. Only moves when money really enters or leaves: treasury funding, deposits, payouts, purchases. |
| **Customer money** | wallets, card balances, referral balances, cashpoint escrow (unchanged) | What we owe customers. Vault ≥ customer money, always. |
| **Earnings vault** | new `earnings_vault`, replacing `fee_revenue` + `fx_margin_revenue` as where income lands (the split by source is kept as a tag on each leg) | Every fee and every conversion margin credits it. May not go below zero. |
| **Referral vault** | new `referral_vault` (Naira), replacing `referral_expense` | Funded from the earnings vault by an admin (two people, like treasury funding). A reward debits it; when it is empty, rewards wait. May not go below zero. |

Flows that change:

- **Fee on any transaction:** customer wallet → `earnings_vault` (was `fee_revenue`).
- **Conversion margin, every route:** the margin is worked out when the quote is priced and posted to `earnings_vault` in the currency it was earned in (today only the Naira rail posts it).
- **Manual credit to a customer:** `earnings_vault → wallet`, refused when the earnings vault is short. A manual debit goes the other way.
- **Funding the referral pot:** `earnings_vault → referral_vault`, requested by one admin, approved by another.
- **Referral reward:** `referral_vault → the referrer's referral balance`, ₦25 per qualifying transaction.
- **Free funds** becomes simply: earnings vault + treasury funding not yet spent. The Inventory page shows the four pots per currency instead of raw accounts.

What does not change: wallets, cards, cashpoints, payouts, purchases, the clearing accounts, `journal_legs` being insert-only, and every existing journal entry. The new accounts open with the balances the old ones hold, moved by one dated journal entry per currency so history stays intact.

---

## 3. What it would take

1. A migration adding the two account templates, and one opening entry per currency moving `fee_revenue` + `fx_margin_revenue` into `earnings_vault` and `referral_expense` into `referral_vault`.
2. New posting rules in `app/services/ledger/rules.go`, each with a balance test in `rules_test.go` (money rule 8), and the margin calculation for direct routes in `rates/pricing.go`.
3. The referral trigger in the swap, send, bill and card services, with an idempotent reference per transaction.
4. Admin: Inventory shows vaults; a "fund referral vault" request/approve flow.
5. `docs/ACCOUNTING.md` rewritten for the new chart, and the accountant's sign-off (the gift-card and cashpoint postings are still waiting for theirs).

This touches every posting that earns money, so it should land as one reviewed change, not in pieces.

---

## 4. Decisions needed before building

1. **Where do customer credits come from?** Recommended: the earnings vault (the company's own money), refused when it is short. The alternative is today's rule: anything in the currency vault that is not owed to customers, which includes treasury funding.
2. **One earnings vault, or fees and margin kept apart?** Recommended: one vault per currency, with the source kept as a tag so reports can still split them.
3. **How is the referral vault funded, and what happens when it is empty?** Recommended: an admin moves money from earnings, a second admin approves, and rewards queue until it is funded. The alternative is drawing each ₦25 straight from earnings with no separate pot.
4. **Which transactions earn the ₦25, and is it built now?** The app says "every time an invited friend transacts". Recommended: completed swaps, sends, bills, gift-card purchases and card top-ups, once per transaction, with a minimum transaction size so a ₦10 send cannot farm it.

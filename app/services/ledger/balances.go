package ledger

import (
	"context"
	"database/sql"

	"gorm.io/gorm"

	"github.com/tribe-africa/tribe-backend/app/models"
	"github.com/tribe-africa/tribe-backend/database"
	"github.com/tribe-africa/tribe-backend/pkg/money"
)

// AccountBalance is an account together with its current balance.
type AccountBalance struct {
	Account models.LedgerAccount
	Balance money.Amount
}

// BalanceOf returns an account's current balance.
//
// How it works (the "snapshot + journal" method):
//
//	balance = latest daily snapshot's closing balance
//	        + every leg posted AFTER that snapshot
//
// Example: the snapshot for 22 Oct says ₦5,000 (last leg id 900). Since then
// leg 950 credited ₦1,000 and leg 990 debited ₦300. Balance = 5,000 + 1,000 - 300 = ₦5,700.
//
// Without snapshots we would add up every leg since the account opened,
// which gets slower every day. With snapshots we only add today's legs.
func (service *Service) BalanceOf(requestContext context.Context, accountID uint64) (money.Amount, error) {
	var account models.LedgerAccount
	if err := service.database.WithContext(requestContext).Where("id = ?", accountID).Take(&account).Error; err != nil {
		return money.Zero(), err
	}
	return service.balanceOfAccount(service.database.WithContext(requestContext), account)
}

// BalanceInTransaction locks one account and returns its balance, using the
// caller's transaction. Use it when you are about to move the WHOLE balance
// (for example closing a card): the lock stops a leg from landing between
// reading the balance and posting.
func (service *Service) BalanceInTransaction(databaseTransaction *gorm.DB, accountID uint64) (money.Amount, error) {
	var account models.LedgerAccount
	if err := database.LockForUpdate(databaseTransaction, &account, "id = ?", accountID); err != nil {
		return money.Zero(), err
	}
	return service.balanceOfAccount(databaseTransaction, account)
}

// BalanceInQuery returns an account's balance using the caller's query or
// transaction, without locking it (for limit checks before ledger.Post,
// which takes the locks itself).
func (service *Service) BalanceInQuery(query *gorm.DB, account models.LedgerAccount) (money.Amount, error) {
	return service.balanceOfAccount(query, account)
}

// balanceOfAccount does the actual sum. Inside Post it runs on the locked
// transaction, so nobody can add a leg to this account while we count.
func (service *Service) balanceOfAccount(query *gorm.DB, account models.LedgerAccount) (money.Amount, error) {
	// 1. The latest snapshot. Ordering by snapshot_date lets MySQL read the
	// last entry of the UNIQUE (ledger_account_id, snapshot_date) index
	// directly, instead of sorting every snapshot the account ever had (one
	// per day). Later days always cover at least as many legs, and every
	// snapshot is an exact total of the legs up to its last_journal_leg_id,
	// so "snapshot + the legs after it" is exact whichever one we use.
	var snapshot models.LedgerDailySnapshot
	snapshotBalance := money.Zero()
	lastLegInSnapshot := uint64(0)
	err := query.
		Where("ledger_account_id = ?", account.ID).
		Order("snapshot_date DESC").
		Limit(1).
		Take(&snapshot).Error
	if err == nil {
		snapshotBalance, err = money.ParseUnits(snapshot.ClosingBalance)
		if err != nil {
			return money.Zero(), err
		}
		lastLegInSnapshot = snapshot.LastJournalLegID
	} else if !database.IsNotFound(err) {
		return money.Zero(), err
	}

	// 2. Everything posted after it: debits count +, credits count -.
	var netDebits sql.NullString
	err = query.Raw(
		`SELECT SUM(CASE WHEN side = 'debit' THEN amount ELSE -amount END)
		   FROM journal_legs
		  WHERE ledger_account_id = ? AND id > ?`,
		account.ID, lastLegInSnapshot,
	).Scan(&netDebits).Error
	if err != nil {
		return money.Zero(), err
	}
	sinceSnapshot, err := money.ParseUnits(netDebits.String)
	if err != nil {
		return money.Zero(), err
	}

	// 3. Turn "net debits" into "how much the account went up". For a
	// credit-normal account (like a user wallet) credits increase it.
	if account.NormalSide == Credit {
		sinceSnapshot = sinceSnapshot.Negate()
	}
	return snapshotBalance.Add(sinceSnapshot), nil
}

// BalancesForUser returns the ledger accounts of a user with their balances.
// Pass account codes (for example CodeUserPrimary) to get only those;
// pass none to get every account, card accounts included. It reads them all
// with batched queries (see balancesOfAccounts).
func (service *Service) BalancesForUser(requestContext context.Context, userID string, codes ...string) ([]AccountBalance, error) {
	query := service.database.WithContext(requestContext).Where("user_id = ?", userID)
	if len(codes) > 0 {
		query = query.Where("code IN ?", codes)
	}
	var accounts []models.LedgerAccount
	err := query.Order("id ASC").Find(&accounts).Error
	if err != nil {
		return nil, err
	}
	return service.BalancesForAccounts(requestContext, accounts)
}

// BalancesForAccounts returns balances for a list of accounts (used by the
// wallet screen and admin pages), in the same order as the list.
//
// These are DISPLAY reads. They never decide whether money may move: Post
// re-checks the balance of every account it debits on the locked rows.
func (service *Service) BalancesForAccounts(requestContext context.Context, accounts []models.LedgerAccount) ([]AccountBalance, error) {
	byID, err := service.balancesOfAccounts(service.database.WithContext(requestContext), accounts)
	if err != nil {
		return nil, err
	}
	balances := make([]AccountBalance, 0, len(accounts))
	for _, account := range accounts {
		balances = append(balances, AccountBalance{Account: account, Balance: byID[account.ID]})
	}
	return balances, nil
}

// balanceBatchSize is how many accounts one batched balance query covers.
const balanceBatchSize = 500

// balancesOfAccounts computes many balances with ONE query per 500
// accounts, instead of two queries per account. It uses exactly the same
// method as balanceOfAccount (latest snapshot + legs after it):
//
//	SELECT each account's id,
//	       the closing balance and last leg of its latest snapshot (or 0),
//	       the debits minus credits of its legs AFTER that snapshot
//	  FROM ledger_accounts ... WHERE id IN (...)
//
// How MySQL runs it (checked with EXPLAIN on a year of snapshots):
//   - "newest" lists each account's latest snapshot date. Because the
//     UNIQUE (account, date) index is sorted, MySQL jumps to the last entry
//     of each account ("Using index for group-by") instead of reading all
//     365 snapshots of it.
//   - "latest" then fetches that one snapshot row by the same unique key.
//   - the legs after it are summed with the (account, id) index.
//
// One SQL statement also reads one consistent moment, which two separate
// queries per account could not promise. (No CTEs or window functions:
// this must run on MySQL 5.7 too.)
//
// Sums come back as text and are parsed with money.ParseUnits: DECIMAL(38,0)
// arithmetic in MySQL is exact, and nothing passes through a float.
func (service *Service) balancesOfAccounts(query *gorm.DB, accounts []models.LedgerAccount) (map[uint64]money.Amount, error) {
	balances := make(map[uint64]money.Amount, len(accounts))
	for start := 0; start < len(accounts); start += balanceBatchSize {
		batch := accounts[start:min(start+balanceBatchSize, len(accounts))]
		accountIDs := make([]uint64, 0, len(batch))
		normalSides := make(map[uint64]string, len(batch))
		for _, account := range batch {
			accountIDs = append(accountIDs, account.ID)
			normalSides[account.ID] = account.NormalSide
		}
		var rows []struct {
			LedgerAccountID uint64
			SnapshotBalance sql.NullString
			NetDebits       sql.NullString
		}
		err := query.Raw(`
			SELECT accounts.id AS ledger_account_id,
			       latest.closing_balance AS snapshot_balance,
			       (SELECT SUM(CASE WHEN legs.side = 'debit' THEN legs.amount ELSE -legs.amount END)
			          FROM journal_legs legs
			         WHERE legs.ledger_account_id = accounts.id
			           AND legs.id > COALESCE(latest.last_journal_leg_id, 0)) AS net_debits
			  FROM ledger_accounts accounts
			  LEFT JOIN (SELECT ledger_account_id, MAX(snapshot_date) AS snapshot_date
			               FROM ledger_daily_snapshots
			              WHERE ledger_account_id IN ?
			              GROUP BY ledger_account_id) newest
			         ON newest.ledger_account_id = accounts.id
			  LEFT JOIN ledger_daily_snapshots latest
			         ON latest.ledger_account_id = newest.ledger_account_id
			        AND latest.snapshot_date = newest.snapshot_date
			 WHERE accounts.id IN ?`, accountIDs, accountIDs).Scan(&rows).Error
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			snapshotBalance, err := money.ParseUnits(row.SnapshotBalance.String)
			if err != nil {
				return nil, err
			}
			sinceSnapshot, err := money.ParseUnits(row.NetDebits.String)
			if err != nil {
				return nil, err
			}
			// Same rule as balanceOfAccount: for a credit-normal account
			// (like a user wallet) credits increase the balance.
			if normalSides[row.LedgerAccountID] == Credit {
				sinceSnapshot = sinceSnapshot.Negate()
			}
			balances[row.LedgerAccountID] = snapshotBalance.Add(sinceSnapshot)
		}
	}
	return balances, nil
}

// FreeFunds is one currency seen the way the providers see it:
//
//	Vaults     everything the providers hold for us in that currency
//	Owed       what we owe customers: every wallet, referral, card and cashpoint balance
//	Free       Vaults - Owed: the money at the providers that is Tribe's own
//	TribeFunds the Tribe Funds account's balance
//
// Because every entry balances per currency, Free and TribeFunds are always
// the same number, reached two ways. Showing both proves it: if they ever
// differed, a posting would have broken the rules. Manual credits and
// referral rewards are paid out of Tribe Funds.
type FreeFunds struct {
	AssetCode  string
	Vaults     money.Amount
	Owed       money.Amount
	Free       money.Amount // may be negative when the providers hold less than we owe
	TribeFunds money.Amount
}

// owedUserCodes are the customer accounts: money Tribe owes its customers.
var owedUserCodes = []string{CodeUserPrimary, CodeUserReferral, CodeUserCard, CodeUserCashpoint}

// FreeFundsOf adds up FreeFunds for one asset. Pass databaseTransaction
// when calling inside a transaction, so the read sees the same data as the
// posting. It gives exactly the numbers FreeFundsForAssets gives.
func (service *Service) FreeFundsOf(query *gorm.DB, assetCode string) (FreeFunds, error) {
	results, err := service.FreeFundsForAssets(query, []string{assetCode})
	if err != nil {
		return FreeFunds{AssetCode: assetCode, Vaults: money.Zero(), Owed: money.Zero(), Free: money.Zero(), TribeFunds: money.Zero()}, err
	}
	return results[assetCode], nil
}

// FreeFundsForAssets adds up FreeFunds for many assets at once (the admin
// Inventory page), keyed by asset code, with a fixed number of queries
// however many currencies there are:
//
//  1. one query loads every provider vault and Tribe Funds account of these assets,
//  2. one batched balance query (balancesOfAccounts) sums them,
//  3. one query sums what we owe customers, grouped by currency.
//
// It is a display read; pass databaseTransaction to read inside a transaction.
func (service *Service) FreeFundsForAssets(query *gorm.DB, assetCodes []string) (map[string]FreeFunds, error) {
	results := make(map[string]FreeFunds, len(assetCodes))
	if len(assetCodes) == 0 {
		return results, nil
	}

	// 1 and 2: the provider vaults and Tribe Funds, with their balances.
	var companyAccounts []models.LedgerAccount
	if err := query.Where("asset_code IN ? AND ((owner_key LIKE ? AND code = ?) OR (owner_key = ? AND code = ?))",
		assetCodes, ProviderOwnerPrefix+"%", CodeProviderVault, ownerCompany, CodeTribeFunds).
		Find(&companyAccounts).Error; err != nil {
		return nil, err
	}
	balances, err := service.balancesOfAccounts(query, companyAccounts)
	if err != nil {
		return nil, err
	}
	vaultsByAsset := map[string]money.Amount{}
	fundsByAsset := map[string]money.Amount{}
	for _, account := range companyAccounts {
		balance, found := balances[account.ID]
		if !found {
			balance = money.Zero()
		}
		if account.Code == CodeTribeFunds {
			fundsByAsset[account.AssetCode] = balance
			continue
		}
		total, found := vaultsByAsset[account.AssetCode]
		if !found {
			total = money.Zero()
		}
		vaultsByAsset[account.AssetCode] = total.Add(balance)
	}

	// 3: what we owe customers, per currency, in ONE statement.
	//
	// The inner SELECT has one row per customer account (owedUserCodes) in
	// the asked currencies, with:
	//   - snapshot_balance: the closing balance of its latest daily snapshot
	//     (NULL when it has none yet),
	//   - net_debits: debits minus credits of the legs posted AFTER that
	//     snapshot (the (account, id) index on journal_legs).
	// The (asset_code, code) index picks the customer accounts.
	//
	// "newest" lists every account's latest snapshot date. It is NOT
	// limited to the customer accounts on purpose: alone on one table, MySQL
	// answers it with ONE jump per account through the UNIQUE (account,
	// date) index (EXPLAIN: "Using index for group-by"). Every way of
	// limiting it (a join, IN (SELECT ...), or a correlated MAX per account)
	// loses that shortcut on MySQL 5.7 and reads EVERY snapshot of every
	// account instead. "latest" then fetches that one snapshot row by the
	// same unique key.
	//
	// The outer SELECT adds them up per (currency, normal side). The side
	// matters because a snapshot's closing balance is already "positive =
	// normal side", while the later legs are raw debits minus credits and
	// must be flipped for credit-normal accounts (done in Go below).
	// No CTEs or window functions, so it runs on MySQL 5.7 as well. Sums
	// come back as text: DECIMAL(38,0) is exact and never touches a float.
	var groups []struct {
		AssetCode      string
		NormalSide     string
		SnapshotTotal  sql.NullString
		NetDebitsTotal sql.NullString
	}
	err = query.Raw(`
		SELECT owed.asset_code AS asset_code,
		       owed.normal_side AS normal_side,
		       SUM(COALESCE(owed.snapshot_balance, 0)) AS snapshot_total,
		       SUM(COALESCE(owed.net_debits, 0)) AS net_debits_total
		  FROM (
		        SELECT accounts.asset_code,
		               accounts.normal_side,
		               latest.closing_balance AS snapshot_balance,
		               (SELECT SUM(CASE WHEN legs.side = 'debit' THEN legs.amount ELSE -legs.amount END)
		                  FROM journal_legs legs
		                 WHERE legs.ledger_account_id = accounts.id
		                   AND legs.id > COALESCE(latest.last_journal_leg_id, 0)) AS net_debits
		          FROM ledger_accounts accounts
		          LEFT JOIN (SELECT ledger_account_id, MAX(snapshot_date) AS snapshot_date
		                       FROM ledger_daily_snapshots
		                      GROUP BY ledger_account_id) newest
		                 ON newest.ledger_account_id = accounts.id
		          LEFT JOIN ledger_daily_snapshots latest
		                 ON latest.ledger_account_id = newest.ledger_account_id
		                AND latest.snapshot_date = newest.snapshot_date
		         WHERE accounts.asset_code IN ?
		           AND accounts.user_id IS NOT NULL
		           AND accounts.code IN ?
		       ) owed
		 GROUP BY owed.asset_code, owed.normal_side`,
		assetCodes, owedUserCodes,
	).Scan(&groups).Error
	if err != nil {
		return nil, err
	}
	owedByAsset := make(map[string]money.Amount, len(assetCodes))
	for _, group := range groups {
		snapshotTotal, err := money.ParseUnits(group.SnapshotTotal.String)
		if err != nil {
			return nil, err
		}
		sinceSnapshots, err := money.ParseUnits(group.NetDebitsTotal.String)
		if err != nil {
			return nil, err
		}
		if group.NormalSide == Credit {
			sinceSnapshots = sinceSnapshots.Negate()
		}
		owed, found := owedByAsset[group.AssetCode]
		if !found {
			owed = money.Zero()
		}
		owedByAsset[group.AssetCode] = owed.Add(snapshotTotal).Add(sinceSnapshots)
	}

	orZero := func(amounts map[string]money.Amount, assetCode string) money.Amount {
		if amount, found := amounts[assetCode]; found {
			return amount
		}
		return money.Zero()
	}
	for _, assetCode := range assetCodes {
		vaults, owed := orZero(vaultsByAsset, assetCode), orZero(owedByAsset, assetCode)
		results[assetCode] = FreeFunds{
			AssetCode: assetCode, Vaults: vaults, Owed: owed, Free: vaults.Subtract(owed),
			TribeFunds: orZero(fundsByAsset, assetCode),
		}
	}
	return results, nil
}

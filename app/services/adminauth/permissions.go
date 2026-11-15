// Package adminauth signs admins in, checks what they are allowed to do,
// manages the team and writes the audit log.
package adminauth

// Permission is one thing an admin may see or do. The keys match the admin
// panel (tribe-admin/lib/domain/permissions.ts) plus the new finance pages.
type Permission struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Group string `json:"group"`
	Risk  string `json:"risk"` // view | action | sensitive
}

// Catalog is every permission. Pages need the "*.view" key; buttons need the action keys.
var Catalog = []Permission{
	{"overview.view", "See the overview", "overview", "view"},
	{"customers.view", "See customers", "customers", "view"},
	{"customers.pii", "Unmasked personal data", "customers", "sensitive"},
	{"customers.labels", "Edit labels", "customers", "action"},
	{"customers.freeze", "Freeze & unfreeze accounts", "customers", "action"},
	{"adjustments.request", "Request manual credits & debits", "customers", "action"},
	{"adjustments.approve", "Approve manual credits & debits (super admins only, not your own)", "customers", "sensitive"},
	{"kyc.view", "See KYC checks", "kyc", "view"},
	{"kyc.media", "View KYC images", "kyc", "sensitive"},
	{"kyc.decide", "Approve & reject checks", "kyc", "action"},
	{"transactions.view", "See transactions & legs", "transactions", "view"},
	{"currencies.view", "See currencies", "currencies", "view"},
	{"currencies.manage", "Add currencies & switch them on or off", "currencies", "action"},
	{"inventory.view", "See inventory & ledger", "inventory", "view"},
	{"inventory.topup", "Request treasury funding", "inventory", "action"},
	{"inventory.approve", "Approve treasury funding (not your own)", "inventory", "sensitive"},
	{"markups.view", "See fees & margins", "markups", "view"},
	{"markups.edit", "Change fees & margins", "markups", "action"},
	{"support.view", "See the inbox", "support", "view"},
	{"support.reply", "Reply to customers", "support", "action"},
	{"support.resolve", "Resolve & reopen", "support", "action"},
	{"support.simulate", "Use the simulator", "support", "action"},
	{"api.view", "See the API trail", "api", "view"},
	{"api.bodies", "See request & response bodies", "api", "sensitive"},
	{"audit.view", "See the audit log", "audit", "view"},
	{"team.view", "See the team", "team", "view"},
	{"team.manage", "Manage teammates", "team", "action"},
	// New with the Go backend:
	{"ledger.view", "See the ledger & snapshots", "ledger", "view"},
	{"providers.view", "See providers", "providers", "view"},
	{"providers.manage", "Change providers & NGN rail", "providers", "action"},
	{"services.view", "See service switches", "services", "view"},
	{"services.manage", "Switch services on & off", "services", "action"},
	{"rates.view", "See live rates", "rates", "view"},
	{"card_designs.manage", "Manage card designs", "card_designs", "action"},
	{"jobs.view", "See queues & cron runs", "jobs", "view"},
	{"jobs.manage", "Retry jobs & run cron", "jobs", "action"},
}

// AllPermissions returns every permission key (for a super admin).
func AllPermissions() []string {
	keys := make([]string, 0, len(Catalog))
	for _, permission := range Catalog {
		keys = append(keys, permission.Key)
	}
	return keys
}

// IsPermission is true if the key exists in the catalog.
func IsPermission(key string) bool {
	for _, permission := range Catalog {
		if permission.Key == key {
			return true
		}
	}
	return false
}

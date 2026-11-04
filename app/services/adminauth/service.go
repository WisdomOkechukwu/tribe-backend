package adminauth

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"

	"github.com/tribe-africa/tribe-backend/app/apperrors"
	"github.com/tribe-africa/tribe-backend/app/cache"
	"github.com/tribe-africa/tribe-backend/app/models"
	"github.com/tribe-africa/tribe-backend/app/services/security"
	"github.com/tribe-africa/tribe-backend/config"
	"github.com/tribe-africa/tribe-backend/database"
	"github.com/tribe-africa/tribe-backend/pkg/ids"
)

// Service handles admin sign-in, the team and the audit log.
type Service struct {
	database *gorm.DB
	redis    *redis.Client
	toolkit  *security.Toolkit
	settings config.Config
}

// NewService creates the admin auth service.
func NewService(connection *gorm.DB, redisClient *redis.Client, toolkit *security.Toolkit, settings config.Config) *Service {
	return &Service{database: connection, redis: redisClient, toolkit: toolkit, settings: settings}
}

// Admin is a signed-in admin with their permissions.
type Admin struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Email       string   `json:"email"`
	Color       string   `json:"color"`
	Status      string   `json:"status"`
	Permissions []string `json:"permissions"`
	SessionID   string   `json:"-"`
}

// Can is true when the admin has a permission.
func (admin *Admin) Can(permission string) bool {
	for _, key := range admin.Permissions {
		if key == permission {
			return true
		}
	}
	return false
}

// IsSuperAdmin is true when the admin holds EVERY permission in the catalog.
// There is no separate role column: "super admin" means nothing is withheld.
// Approving manual credits and debits is limited to super admins.
func (admin *Admin) IsSuperAdmin() bool {
	for _, permission := range Catalog {
		if !admin.Can(permission.Key) {
			return false
		}
	}
	return true
}

// LoginResult is returned by Login.
type LoginResult struct {
	Admin       *Admin    `json:"admin"`
	AccessToken string    `json:"access_token"`
	ExpiresAt   time.Time `json:"expires_at"`
}

// Login checks an admin's email and password and starts a session.
func (service *Service) Login(requestContext context.Context, email string, password string, ipAddress string, userAgent string) (*LoginResult, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	invalid := apperrors.New(http.StatusUnauthorized, "INVALID_CREDENTIALS", "Email or password is not correct.")
	// 5 attempts per email per 15 minutes, counted atomically BEFORE the
	// slow password hash (see cache.TakeAttempt). Success clears the count.
	failureKey := "admin-login-failures:" + email
	if !cache.TakeAttempt(requestContext, service.redis, failureKey, 5, 15*time.Minute) {
		return nil, apperrors.New(http.StatusTooManyRequests, "TOO_MANY_ATTEMPTS", "Too many attempts. Try again in 15 minutes.")
	}

	var adminUser models.AdminUser
	err := service.database.WithContext(requestContext).Where("email = ?", email).Take(&adminUser).Error
	if database.IsNotFound(err) {
		service.toolkit.DummyPasswordCheck(password)
		return nil, invalid
	}
	if err != nil {
		return nil, err
	}
	if !service.toolkit.CheckPassword(adminUser.PasswordHash, password) {
		return nil, invalid // already counted by TakeAttempt above
	}
	cache.ForgetAttempts(requestContext, service.redis, failureKey)
	if adminUser.Status != "active" {
		return nil, apperrors.New(http.StatusForbidden, "SUSPENDED", "This admin account is suspended.")
	}

	now := time.Now().UTC()
	session := models.AdminSession{
		ID: ids.New(), AdminUserID: adminUser.ID, IPAddress: ipAddress, UserAgent: truncate(userAgent, 255),
		ExpiresAt: now.Add(service.settings.Auth.AdminSessionLifetime), CreatedAt: now,
	}
	if err := service.database.WithContext(requestContext).Create(&session).Error; err != nil {
		return nil, err
	}
	service.database.WithContext(requestContext).Model(&models.AdminUser{}).Where("id = ?", adminUser.ID).Update("last_login_at", now)
	token, expiresAt, err := service.toolkit.IssueAccessToken(security.AudienceAdmin, adminUser.ID, session.ID, "", service.settings.Auth.AdminSessionLifetime)
	if err != nil {
		return nil, err
	}
	admin, err := service.LoadAdmin(requestContext, adminUser.ID, session.ID)
	if err != nil {
		return nil, err
	}
	service.Audit(requestContext, admin, "auth.login", adminUser.Email, "Signed in", nil, nil)
	return &LoginResult{Admin: admin, AccessToken: token, ExpiresAt: expiresAt}, nil
}

// Logout ends an admin session.
func (service *Service) Logout(requestContext context.Context, admin *Admin) error {
	service.redis.Del(requestContext, "admin-session:"+admin.SessionID)
	return service.database.WithContext(requestContext).Model(&models.AdminSession{}).
		Where("id = ?", admin.SessionID).Update("revoked_at", time.Now().UTC()).Error
}

// LoadAdmin loads an admin and their permissions, and checks the session is
// still valid. Cached in Redis for 30 seconds; permission changes clear it.
func (service *Service) LoadAdmin(requestContext context.Context, adminID string, sessionID string) (*Admin, error) {
	cacheKey := "admin-session:" + sessionID
	if cached, err := service.redis.Get(requestContext, cacheKey).Bytes(); err == nil {
		var admin Admin
		if json.Unmarshal(cached, &admin) == nil && admin.ID == adminID {
			admin.SessionID = sessionID
			return &admin, nil
		}
	}
	var session models.AdminSession
	if err := service.database.WithContext(requestContext).Where("id = ? AND admin_user_id = ?", sessionID, adminID).Take(&session).Error; err != nil {
		return nil, apperrors.ErrUnauthenticated
	}
	if session.RevokedAt != nil || session.ExpiresAt.Before(time.Now().UTC()) {
		return nil, apperrors.ErrUnauthenticated
	}
	var adminUser models.AdminUser
	if err := service.database.WithContext(requestContext).Where("id = ?", adminID).Take(&adminUser).Error; err != nil {
		return nil, apperrors.ErrUnauthenticated
	}
	if adminUser.Status != "active" {
		return nil, apperrors.New(http.StatusForbidden, "SUSPENDED", "This admin account is suspended.")
	}
	var permissions []string
	service.database.WithContext(requestContext).Model(&models.AdminUserPermission{}).Where("admin_user_id = ?", adminID).Order("permission").Pluck("permission", &permissions)
	admin := &Admin{ID: adminUser.ID, Name: adminUser.Name, Email: adminUser.Email, Color: adminUser.Color, Status: adminUser.Status, Permissions: permissions, SessionID: sessionID}
	if encoded, err := json.Marshal(admin); err == nil {
		service.redis.Set(requestContext, cacheKey, encoded, 30*time.Second)
	}
	return admin, nil
}

// ─── Team ────────────────────────────────────────────────────────────

// NewAdminInput creates a teammate.
type NewAdminInput struct {
	Email       string
	Name        string
	Password    string // empty = generate a temporary one
	Color       string
	Permissions []string
	CreatedBy   *string
}

// CreateAdmin adds a teammate and returns it (with the temporary password if one was generated).
func (service *Service) CreateAdmin(requestContext context.Context, input NewAdminInput) (*TeamMember, error) {
	email := strings.ToLower(strings.TrimSpace(input.Email))
	if !strings.Contains(email, "@") || len(strings.TrimSpace(input.Name)) < 2 {
		return nil, apperrors.Validation(map[string][]string{"email": {"Enter a name and a valid email."}})
	}
	password := input.Password
	temporary := ""
	if password == "" {
		temporary = "Tribe-" + ids.RandomToken(5) + "!"
		password = temporary
	}
	now := time.Now().UTC()
	adminUser := models.AdminUser{
		ID: ids.New(), Name: strings.TrimSpace(input.Name), Email: email, PasswordHash: service.toolkit.HashPassword(password),
		Color: input.Color, Status: "active", CreatedBy: input.CreatedBy, Version: 1, CreatedAt: now, UpdatedAt: now,
	}
	err := database.WithTransaction(requestContext, service.database, func(databaseTransaction *gorm.DB) error {
		if err := databaseTransaction.Create(&adminUser).Error; err != nil {
			if database.IsDuplicateEntry(err) {
				return apperrors.New(http.StatusConflict, "EMAIL_TAKEN", "A teammate with that email already exists.")
			}
			return err
		}
		return replacePermissions(databaseTransaction, adminUser.ID, input.Permissions)
	})
	if err != nil {
		return nil, err
	}
	member, err := service.teamMember(requestContext, adminUser.ID)
	if member != nil {
		member.TemporaryPassword = temporary
	}
	return member, err
}

// TeamMember is a teammate as shown on the Team page.
type TeamMember struct {
	ID                string     `json:"id"`
	Name              string     `json:"name"`
	Email             string     `json:"email"`
	Color             string     `json:"color"`
	Status            string     `json:"status"`
	Permissions       []string   `json:"permissions"`
	LastLoginAt       *time.Time `json:"last_login_at"`
	CreatedAt         time.Time  `json:"created_at"`
	Version           int        `json:"version"`
	TemporaryPassword string     `json:"temporary_password,omitempty"`
}

// ListTeam returns every admin.
func (service *Service) ListTeam(requestContext context.Context) ([]TeamMember, error) {
	var adminUsers []models.AdminUser
	if err := service.database.WithContext(requestContext).Order("created_at").Find(&adminUsers).Error; err != nil {
		return nil, err
	}
	members := make([]TeamMember, 0, len(adminUsers))
	for _, adminUser := range adminUsers {
		member, err := service.teamMember(requestContext, adminUser.ID)
		if err != nil {
			return nil, err
		}
		members = append(members, *member)
	}
	return members, nil
}

// teamMember loads one teammate with permissions.
func (service *Service) teamMember(requestContext context.Context, adminID string) (*TeamMember, error) {
	var adminUser models.AdminUser
	if err := service.database.WithContext(requestContext).Where("id = ?", adminID).Take(&adminUser).Error; err != nil {
		return nil, apperrors.NotFound("Teammate")
	}
	var permissions []string
	service.database.WithContext(requestContext).Model(&models.AdminUserPermission{}).Where("admin_user_id = ?", adminID).Order("permission").Pluck("permission", &permissions)
	return &TeamMember{ID: adminUser.ID, Name: adminUser.Name, Email: adminUser.Email, Color: adminUser.Color, Status: adminUser.Status,
		Permissions: permissions, LastLoginAt: adminUser.LastLoginAt, CreatedAt: adminUser.CreatedAt, Version: adminUser.Version}, nil
}

// TeamChanges updates a teammate.
type TeamChanges struct {
	Name          *string
	Color         *string
	Status        *string
	Permissions   []string
	ResetPassword bool
	Version       int
}

// UpdateTeammate changes a teammate with lock + version, and keeps the
// guard rails: you can't lock yourself out, and there must always be at
// least one active admin with team.manage.
func (service *Service) UpdateTeammate(requestContext context.Context, actingAdmin *Admin, teammateID string, changes TeamChanges) (*TeamMember, error) {
	// The slow (64 MB) password hash runs BEFORE the transaction, so the
	// teammate's row is not locked while it works.
	temporary := ""
	temporaryHash := ""
	if changes.ResetPassword {
		temporary = "Tribe-" + ids.RandomToken(5) + "!"
		temporaryHash = service.toolkit.HashPassword(temporary)
	}
	err := database.WithTransaction(requestContext, service.database, func(databaseTransaction *gorm.DB) error {
		var adminUser models.AdminUser
		if err := database.LockForUpdate(databaseTransaction, &adminUser, "id = ?", teammateID); err != nil {
			return apperrors.NotFound("Teammate")
		}
		if changes.Version != 0 && changes.Version != adminUser.Version {
			return apperrors.ErrStaleData
		}
		isSelf := teammateID == actingAdmin.ID
		if isSelf && changes.Status != nil && *changes.Status != "active" {
			return apperrors.New(http.StatusUnprocessableEntity, "SELF_LOCKOUT", "You can't suspend yourself.")
		}
		if isSelf && changes.Permissions != nil && !contains(changes.Permissions, "team.manage") {
			return apperrors.New(http.StatusUnprocessableEntity, "SELF_LOCKOUT", "You can't remove your own team.manage permission.")
		}
		updates := map[string]any{"updated_at": time.Now().UTC(), "version": adminUser.Version + 1}
		if changes.Name != nil {
			updates["name"] = strings.TrimSpace(*changes.Name)
		}
		if changes.Color != nil {
			updates["color"] = *changes.Color
		}
		if changes.Status != nil {
			updates["status"] = *changes.Status
		}
		if changes.ResetPassword {
			updates["password_hash"] = temporaryHash // hashed before the transaction
		}
		if err := databaseTransaction.Model(&models.AdminUser{}).Where("id = ?", teammateID).Updates(updates).Error; err != nil {
			return err
		}
		if changes.Permissions != nil {
			if err := replacePermissions(databaseTransaction, teammateID, changes.Permissions); err != nil {
				return err
			}
		}
		// Guard rail: at least one active manager must remain.
		var managers int64
		databaseTransaction.Raw(`SELECT COUNT(*) FROM admin_users admins
		    JOIN admin_user_permissions permissions ON permissions.admin_user_id = admins.id AND permissions.permission = 'team.manage'
		   WHERE admins.status = 'active'`).Scan(&managers)
		if managers == 0 {
			return apperrors.New(http.StatusUnprocessableEntity, "LAST_MANAGER", "At least one active teammate must be able to manage the team.")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	service.forgetAdminSessions(requestContext, teammateID)
	member, err := service.teamMember(requestContext, teammateID)
	if member != nil {
		member.TemporaryPassword = temporary
	}
	return member, err
}

// RemoveTeammate deletes a teammate (not yourself, not the last manager).
func (service *Service) RemoveTeammate(requestContext context.Context, actingAdmin *Admin, teammateID string) error {
	if teammateID == actingAdmin.ID {
		return apperrors.New(http.StatusUnprocessableEntity, "SELF_LOCKOUT", "You can't remove yourself.")
	}
	err := database.WithTransaction(requestContext, service.database, func(databaseTransaction *gorm.DB) error {
		var adminUser models.AdminUser
		if err := database.LockForUpdate(databaseTransaction, &adminUser, "id = ?", teammateID); err != nil {
			return apperrors.NotFound("Teammate")
		}
		if err := databaseTransaction.Where("admin_user_id = ?", teammateID).Delete(&models.AdminUserPermission{}).Error; err != nil {
			return err
		}
		databaseTransaction.Model(&models.AdminSession{}).Where("admin_user_id = ? AND revoked_at IS NULL", teammateID).Update("revoked_at", time.Now().UTC())
		return databaseTransaction.Where("id = ?", teammateID).Delete(&models.AdminUser{}).Error
	})
	service.forgetAdminSessions(requestContext, teammateID)
	return err
}

// forgetAdminSessions clears cached sessions so permission changes apply at once.
func (service *Service) forgetAdminSessions(requestContext context.Context, adminID string) {
	var sessionIDs []string
	service.database.WithContext(requestContext).Model(&models.AdminSession{}).Where("admin_user_id = ?", adminID).Pluck("id", &sessionIDs)
	for _, sessionID := range sessionIDs {
		service.redis.Del(requestContext, "admin-session:"+sessionID)
	}
}

// replacePermissions swaps an admin's permission rows.
func replacePermissions(databaseTransaction *gorm.DB, adminID string, permissions []string) error {
	if err := databaseTransaction.Where("admin_user_id = ?", adminID).Delete(&models.AdminUserPermission{}).Error; err != nil {
		return err
	}
	rows := []models.AdminUserPermission{}
	for _, permission := range permissions {
		if IsPermission(permission) {
			rows = append(rows, models.AdminUserPermission{AdminUserID: adminID, Permission: permission})
		}
	}
	if len(rows) == 0 {
		return nil
	}
	return databaseTransaction.Create(&rows).Error
}

// ─── Audit log ───────────────────────────────────────────────────────

// Audit records an admin action with before/after values.
func (service *Service) Audit(requestContext context.Context, admin *Admin, action string, target string, summary string, before any, after any) {
	entry := models.AuditLog{ID: ids.New(), Action: action, Target: target, Summary: truncate(summary, 255), CreatedAt: time.Now().UTC()}
	if admin != nil {
		entry.AdminUserID, entry.AdminName = &admin.ID, admin.Name
	}
	if before != nil {
		entry.BeforeData, _ = json.Marshal(before)
	}
	if after != nil {
		entry.AfterData, _ = json.Marshal(after)
	}
	service.database.WithContext(requestContext).Create(&entry)
}

// contains is true if list has value.
func contains(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

// truncate cuts text to a maximum length.
func truncate(text string, maximumLength int) string {
	if len(text) <= maximumLength {
		return text
	}
	return text[:maximumLength]
}

package database

import (
	"github.com/pressly/goose/v3"
	"gorm.io/gorm"

	"github.com/tribe-africa/tribe-backend/database/migrations"
)

// Migrate runs every migration that has not run yet (like `php artisan migrate`).
// The .sql files live in database/migrations and are built into the binary.
func Migrate(connection *gorm.DB) error {
	pool, err := connection.DB()
	if err != nil {
		return err
	}
	goose.SetBaseFS(migrations.Files)
	if err := goose.SetDialect("mysql"); err != nil {
		return err
	}
	return goose.Up(pool, ".")
}

// Rollback undoes the most recent migration (like `php artisan migrate:rollback --step=1`).
func Rollback(connection *gorm.DB) error {
	pool, err := connection.DB()
	if err != nil {
		return err
	}
	goose.SetBaseFS(migrations.Files)
	if err := goose.SetDialect("mysql"); err != nil {
		return err
	}
	return goose.Down(pool, ".")
}

// MigrationStatus prints which migrations have run (like `php artisan migrate:status`).
func MigrationStatus(connection *gorm.DB) error {
	pool, err := connection.DB()
	if err != nil {
		return err
	}
	goose.SetBaseFS(migrations.Files)
	if err := goose.SetDialect("mysql"); err != nil {
		return err
	}
	return goose.Status(pool, ".")
}

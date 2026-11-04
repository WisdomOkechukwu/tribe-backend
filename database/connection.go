// Package database opens the MySQL connection and holds the helpers that
// keep money safe: transactions, row locks and automatic retries.
package database

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"regexp"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/tribe-africa/tribe-backend/config"
)

// allowedDatabaseName protects the other databases on the same MySQL server
// (the MAMP server also hosts unrelated Laravel apps). We only ever touch
// "tribe_core" or a throw-away "tribe_core_test_*" database.
var allowedDatabaseName = regexp.MustCompile(`^tribe_core(_test_[a-z0-9_]+)?$`)

// CheckDatabaseName refuses to run against any database we do not own.
func CheckDatabaseName(name string) error {
	if !allowedDatabaseName.MatchString(name) {
		return fmt.Errorf("refusing to use database %q: only \"tribe_core\" or \"tribe_core_test_*\" are allowed", name)
	}
	return nil
}

// buildDataSourceName turns the config into the text the MySQL driver needs.
// When includeDatabase is false we connect to the server only (used to
// create the database the first time).
func buildDataSourceName(settings config.DatabaseConfig, includeDatabase bool) string {
	driverConfig := mysqldriver.NewConfig()
	driverConfig.User = settings.Username
	driverConfig.Passwd = settings.Password
	if settings.Socket != "" {
		driverConfig.Net = "unix"
		driverConfig.Addr = settings.Socket
	} else {
		driverConfig.Net = "tcp"
		driverConfig.Addr = settings.Host + ":" + settings.Port
	}
	if includeDatabase {
		driverConfig.DBName = settings.Database
	}
	driverConfig.ParseTime = true                 // turn DATETIME columns into Go time.Time
	driverConfig.Loc = time.UTC                   // we store every time in UTC
	driverConfig.Collation = "utf8mb4_unicode_ci" // full unicode (emoji, accents)
	driverConfig.MultiStatements = true           // needed by migrations that create triggers
	// Fail instead of hanging forever if a MySQL on another server stalls.
	driverConfig.Timeout = settings.ConnectTimeout
	driverConfig.ReadTimeout = settings.ReadTimeout
	driverConfig.WriteTimeout = settings.WriteTimeout
	driverConfig.Params = map[string]string{
		// Same rules on MySQL 5.7 (MAMP) and 8.4 (production).
		"time_zone": "'+00:00'",
		"sql_mode":  "'ONLY_FULL_GROUP_BY,STRICT_TRANS_TABLES,NO_ZERO_IN_DATE,NO_ZERO_DATE,ERROR_FOR_DIVISION_BY_ZERO,NO_ENGINE_SUBSTITUTION'",
	}
	return driverConfig.FormatDSN()
}

// CreateDatabaseIfMissing creates the database (like `CREATE DATABASE tribe`)
// the first time. It is used by `artisan migrate`.
func CreateDatabaseIfMissing(settings config.DatabaseConfig) error {
	if err := CheckDatabaseName(settings.Database); err != nil {
		return err
	}
	serverConnection, err := sql.Open("mysql", buildDataSourceName(settings, false))
	if err != nil {
		return err
	}
	defer serverConnection.Close()
	statement := fmt.Sprintf("CREATE DATABASE IF NOT EXISTS `%s` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci", settings.Database)
	_, err = serverConnection.Exec(statement)
	return err
}

// DropDatabase deletes a test database. It refuses anything except tribe_core_test_*.
func DropDatabase(settings config.DatabaseConfig) error {
	if !regexp.MustCompile(`^tribe_core_test_[a-z0-9_]+$`).MatchString(settings.Database) {
		return fmt.Errorf("refusing to drop %q: only tribe_core_test_* databases can be dropped", settings.Database)
	}
	serverConnection, err := sql.Open("mysql", buildDataSourceName(settings, false))
	if err != nil {
		return err
	}
	defer serverConnection.Close()
	_, err = serverConnection.Exec(fmt.Sprintf("DROP DATABASE IF EXISTS `%s`", settings.Database))
	return err
}

// Connect opens the connection pool. GORM is our "Eloquent": it maps Go
// structs (models) to tables.
func Connect(settings config.DatabaseConfig) (*gorm.DB, error) {
	if err := CheckDatabaseName(settings.Database); err != nil {
		return nil, err
	}
	connection, err := gorm.Open(mysql.Open(buildDataSourceName(settings, true)), &gorm.Config{
		// Log slow queries and errors, but not "record not found" (we expect those).
		Logger: logger.New(log.New(os.Stdout, "", log.LstdFlags), logger.Config{
			SlowThreshold: 500 * time.Millisecond, LogLevel: logger.Warn, IgnoreRecordNotFoundError: true, Colorful: false,
		}),
		// Times are always saved in UTC with microseconds.
		NowFunc: func() time.Time { return time.Now().UTC().Truncate(time.Microsecond) },
		// We never want GORM to silently create or change tables. Migrations do that.
		DisableAutomaticPing: false,
	})
	if err != nil {
		return nil, err
	}

	pool, err := connection.DB()
	if err != nil {
		return nil, err
	}
	// Pool sizes come from .env (DB_MAX_OPEN_CONNECTIONS etc.; defaults 25 /
	// 10 / 30 min / 5 min) so they can be fitted to the MySQL server.
	pool.SetMaxOpenConns(settings.MaxOpenConnections)
	pool.SetMaxIdleConns(settings.MaxIdleConnections)
	pool.SetConnMaxLifetime(settings.ConnectionLifetime)
	pool.SetConnMaxIdleTime(settings.ConnectionIdleTime)
	if err := pool.Ping(); err != nil {
		return nil, fmt.Errorf("cannot reach MySQL: %w", err)
	}
	return connection, nil
}

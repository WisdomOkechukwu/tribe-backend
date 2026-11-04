// Command artisan is our version of Laravel's `php artisan`.
//
// Usage:
//
//	go run ./cmd/artisan migrate              create the database (if needed) and run migrations
//	go run ./cmd/artisan migrate:rollback     undo the last migration
//	go run ./cmd/artisan migrate:status       list migrations and whether they ran
//	go run ./cmd/artisan migrate:fresh        (tribe_test_* only) drop, create and migrate
//	go run ./cmd/artisan db:seed              fill lookup tables + demo users and admins
//	go run ./cmd/artisan loadtest:seed [count]  (never production) funded customers for cmd/loadtest (default 200)
//	go run ./cmd/artisan ledger:snapshot [YYYY-MM-DD]   write daily snapshots for a day (default: yesterday)
//	go run ./cmd/artisan ledger:verify [--incremental]  check the books balance and snapshots match the journal
//	go run ./cmd/artisan admin:create <email> <name> <password>   create a super admin
//	go run ./cmd/artisan route:list           print every API route
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/tribe-africa/tribe-backend/app/bootstrap"
	"github.com/tribe-africa/tribe-backend/app/services/adminauth"
	"github.com/tribe-africa/tribe-backend/app/services/ledger"
	"github.com/tribe-africa/tribe-backend/config"
	"github.com/tribe-africa/tribe-backend/database"
	"github.com/tribe-africa/tribe-backend/database/seeders"
	"github.com/tribe-africa/tribe-backend/routes"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("usage: artisan <command>. See the comment at the top of cmd/artisan/main.go")
		os.Exit(1)
	}
	if err := run(os.Args[1], os.Args[2:]); err != nil {
		slog.Error("command failed", "command", os.Args[1], "error", err)
		os.Exit(1)
	}
}

// run executes one command by name.
func run(command string, arguments []string) error {
	settings, err := config.Load()
	if err != nil {
		return err
	}
	requestContext := context.Background()

	switch command {
	case "migrate":
		if err := database.CreateDatabaseIfMissing(settings.Database); err != nil {
			return err
		}
		// No read/write timeout for migrations: adding an index to a big
		// table can take minutes, and a timeout would stop it half-way.
		settings.Database.ReadTimeout, settings.Database.WriteTimeout = 0, 0
		connection, err := database.Connect(settings.Database)
		if err != nil {
			return err
		}
		return database.Migrate(connection)

	case "migrate:rollback":
		settings.Database.ReadTimeout, settings.Database.WriteTimeout = 0, 0 // see migrate
		connection, err := database.Connect(settings.Database)
		if err != nil {
			return err
		}
		return database.Rollback(connection)

	case "migrate:status":
		connection, err := database.Connect(settings.Database)
		if err != nil {
			return err
		}
		return database.MigrationStatus(connection)

	case "migrate:fresh":
		if err := database.DropDatabase(settings.Database); err != nil {
			return err
		}
		if err := database.CreateDatabaseIfMissing(settings.Database); err != nil {
			return err
		}
		connection, err := database.Connect(settings.Database)
		if err != nil {
			return err
		}
		return database.Migrate(connection)

	case "db:seed":
		application, err := bootstrap.New(settings)
		if err != nil {
			return err
		}
		defer application.Close()
		return seeders.Run(requestContext, application)

	case "loadtest:seed":
		application, err := bootstrap.New(settings)
		if err != nil {
			return err
		}
		defer application.Close()
		count := 200
		if len(arguments) > 0 {
			if _, err := fmt.Sscan(arguments[0], &count); err != nil || count < 1 {
				return fmt.Errorf("usage: artisan loadtest:seed [count]")
			}
		}
		if err := seeders.SeedLoadTestCustomers(requestContext, application, count); err != nil {
			return err
		}
		fmt.Printf("%d load-test customers ready (loadtest-N@tribe.test, password %s, PIN %s)\n", count, seeders.LoadTestPassword, seeders.LoadTestPin)
		return nil

	case "ledger:snapshot":
		application, err := bootstrap.New(settings)
		if err != nil {
			return err
		}
		defer application.Close()
		snapshotDate := time.Now().In(settings.App.TimeZone).AddDate(0, 0, -1).Format(time.DateOnly)
		if len(arguments) > 0 {
			snapshotDate = arguments[0]
		}
		written, err := application.Ledger.WriteDailySnapshots(requestContext, snapshotDate)
		if err != nil {
			return err
		}
		fmt.Printf("wrote %d snapshots for %s\n", written, snapshotDate)
		return nil

	case "ledger:verify":
		application, err := bootstrap.New(settings)
		if err != nil {
			return err
		}
		defer application.Close()
		// Full by default; --incremental checks only what changed since
		// the last clean run (what the nightly job does).
		mode := ledger.VerifyFull
		if len(arguments) > 0 && arguments[0] == "--incremental" {
			mode = ledger.VerifyIncremental
		}
		report, err := application.Ledger.RunVerify(requestContext, mode)
		if err != nil {
			return err
		}
		fmt.Printf("%s check: %d entries and %d snapshots checked\n", report.Mode, report.EntriesChecked, report.SnapshotsChecked)
		printVerifyReport(report.Problems)
		if report.ProblemCount > 0 {
			return fmt.Errorf("%d ledger problems found", report.ProblemCount)
		}
		return nil

	case "admin:create":
		if len(arguments) < 3 {
			return fmt.Errorf("usage: artisan admin:create <email> <name> <password>")
		}
		application, err := bootstrap.New(settings)
		if err != nil {
			return err
		}
		defer application.Close()
		admin, err := application.AdminAuth.CreateAdmin(requestContext, adminauth.NewAdminInput{
			Email:       arguments[0],
			Name:        arguments[1],
			Password:    arguments[2],
			Permissions: adminauth.AllPermissions(),
		})
		if err != nil {
			return err
		}
		fmt.Printf("created admin %s (%s)\n", admin.Email, admin.ID)
		return nil

	case "route:list":
		application, err := bootstrap.New(settings)
		if err != nil {
			return err
		}
		defer application.Close()
		router := routes.Build(application)
		for _, route := range router.Routes() {
			fmt.Printf("%-7s %s\n", route.Method, route.Path)
		}
		return nil
	}
	return fmt.Errorf("unknown command %q", command)
}

// printVerifyReport prints what ledger:verify found.
func printVerifyReport(problems []ledger.VerifyProblem) {
	if len(problems) == 0 {
		fmt.Println("ledger OK: every entry balances and every snapshot matches the journal")
		return
	}
	for _, problem := range problems {
		fmt.Printf("PROBLEM: %s\n", problem.Description)
	}
}

package storage

import "context"

var ReserveWaitingInvestigationsForTest = reserveWaitingInvestigations

func MigrateBeforeAutomaticIncidentForTest(ctx context.Context, database *Database) error {
	return migrateBeforeForTest(ctx, database, "0011_automatic_incident_investigation")
}

func MigrateBeforeAlertBackfillForTest(ctx context.Context, database *Database) error {
	return migrateBeforeForTest(ctx, database, "0012_retire_alert_webhook_jobs")
}

func migrateBeforeForTest(ctx context.Context, database *Database, version string) error {
	migrations, err := loadMigrations()
	if err != nil {
		return err
	}
	var previous []migration
	for _, migration := range migrations {
		if migration.version < version {
			previous = append(previous, migration)
		}
	}
	_, err = migrateDatabase(ctx, database.pool, previous)
	return err
}

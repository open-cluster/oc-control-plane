package storage

import "context"

var ReserveWaitingInvestigationsForTest = reserveWaitingInvestigations

func MigrateBeforeAutomaticIncidentForTest(ctx context.Context, database *Database) error {
	migrations, err := loadMigrations()
	if err != nil {
		return err
	}
	var previous []migration
	for _, migration := range migrations {
		if migration.version < "0011_automatic_incident_investigation" {
			previous = append(previous, migration)
		}
	}
	_, err = migrateDatabase(ctx, database.pool, previous)
	return err
}

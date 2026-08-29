package common

type DatabaseType string

const (
	DatabaseTypeMySQL      DatabaseType = "mysql"
	DatabaseTypeSQLite     DatabaseType = "sqlite"
	DatabaseTypePostgreSQL DatabaseType = "postgres"
	DatabaseTypeClickHouse DatabaseType = "clickhouse"
)

var mainDatabaseType = DatabaseTypeSQLite
var logDatabaseType = DatabaseTypeSQLite

func MainDatabaseType() DatabaseType {
	return mainDatabaseType
}

func LogDatabaseType() DatabaseType {
	return logDatabaseType
}

func SetMainDatabaseType(databaseType DatabaseType) {
	mainDatabaseType = databaseType
}

func SetLogDatabaseType(databaseType DatabaseType) {
	logDatabaseType = databaseType
}

func SetDatabaseTypes(mainType DatabaseType, logType DatabaseType) {
	mainDatabaseType = mainType
	logDatabaseType = logType
}

func UsingMainDatabase(databaseType DatabaseType) bool {
	return mainDatabaseType == databaseType
}

func UsingLogDatabase(databaseType DatabaseType) bool {
	return logDatabaseType == databaseType
}

// The glebarez (pure Go) driver only understands the _pragma= form; the
// mattn-style _busy_timeout= parameter is silently dropped, which left the
// database on the driver default of 5s and journal_mode=delete. WAL lets
// readers run without blocking the writer, which matters because every relay
// request writes billing rows.
var SQLitePath = "one-api.db?_pragma=busy_timeout(30000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_txlock=immediate"

// Package sqlrun drives SQL load over database/sql with explicit connection acquisition
// so pool wait is timed apart from query time. Postgres, MySQL and SQLite.
package sqlrun

// Package provider import this package to populate the global Registry.
package provider

func init() {
	Registry.Register(PostgresProvider{})
	Registry.Register(MySQLProvider{})
	Registry.Register(MSSQLProvider{})
}

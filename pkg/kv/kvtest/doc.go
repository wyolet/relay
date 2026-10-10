// Package kvtest holds kv fixtures for tests: a Recording store that pins
// the scripts a caller runs, and (integration builds) a Redis/Valkey
// keyspace of its own per test on the server named by RELAY_TEST_REDIS_ADDR,
// so test packages run in parallel against one server despite fixed key
// names and prefix scans.
//
// Isolation is by logical database: a test leases one of the indexes 1..15
// through a key on database 0, and the database is flushed and the lease
// released on cleanup. A killed run's leases expire on their own. Cluster
// and Sentinel topologies are out of scope. Redis tests skip when
// RELAY_TEST_REDIS_ADDR is unset.
package kvtest

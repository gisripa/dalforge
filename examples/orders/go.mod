module github.com/gisripa/dalforge/examples/orders

go 1.26.0

require (
	github.com/brianvoe/gofakeit/v7 v7.17.1
	github.com/gisripa/dalforge/dal v0.0.0
	github.com/google/uuid v1.6.0
	github.com/jackc/pgx/v5 v5.11.0
)

require (
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	golang.org/x/sync v0.17.0 // indirect
	golang.org/x/text v0.29.0 // indirect
)

// The example uses this repository's runtime module (dal, dalpg). A real project
// would require a released version instead.
replace github.com/gisripa/dalforge/dal => ../../dal

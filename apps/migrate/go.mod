module github.com/blackhorseya/x-monorepo/apps/migrate

go 1.27

require (
	github.com/blackhorseya/x-monorepo/pkgs/ordering v0.0.0
	github.com/blackhorseya/x-monorepo/pkgs/platform v0.0.0
)

require (
	github.com/blackhorseya/x-monorepo/pkgs/kernel v0.0.0 // indirect
	github.com/golang-migrate/migrate/v4 v4.20.1 // indirect
	github.com/jackc/pgerrcode v0.0.0-20220416144525-469b46aa5efa // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/pgx/v5 v5.11.0 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/jinzhu/inflection v1.0.0 // indirect
	github.com/jinzhu/now v1.1.5 // indirect
	github.com/stretchr/testify v1.12.1 // indirect
	golang.org/x/sync v0.23.0 // indirect
	golang.org/x/text v0.42.0 // indirect
	gorm.io/driver/postgres v1.6.3 // indirect
	gorm.io/gorm v1.31.2 // indirect
)

replace (
	github.com/blackhorseya/x-monorepo/pkgs/kernel => ../../pkgs/kernel
	github.com/blackhorseya/x-monorepo/pkgs/ordering => ../../pkgs/ordering
	github.com/blackhorseya/x-monorepo/pkgs/platform => ../../pkgs/platform
)

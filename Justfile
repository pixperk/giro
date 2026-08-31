# giro
#
# DATABASE_URL and GIRO_TEST_DATABASE_URL come from .env, which is gitignored.
# copy .env.example to get started.

set dotenv-load := true
set dotenv-required := true

_default:
    @just --list --unsorted

# --- migrations -------------------------------------------------------------

# apply every pending migration
migrate: (_migrate "up")

# show what has run and what has not
status: (_migrate "status")

# create an empty migration, e.g. just new "add metadata tables"
new NAME:
    @go run ./cmd/giro migrate new {{ NAME }}

_migrate CMD:
    @go run ./cmd/giro migrate {{ CMD }}

# apply migrations to the test database
migrate-test:
    @DATABASE_URL="$GIRO_TEST_DATABASE_URL" go run ./cmd/giro migrate up

# --- development ------------------------------------------------------------

# run every test with the race detector
test:
    go test -race ./...

# run tests matching a pattern, e.g. just test-one VolumeUpdates
test-one PATTERN:
    go test -race -run {{ PATTERN }} -v ./...

# statement coverage per package
cover:
    go test -cover ./...

# open the coverage report in a browser
cover-html:
    @go test -coverprofile=coverage.out ./... > /dev/null
    @go tool cover -html=coverage.out

# everything ci runs, in the same order. run before committing
check: fmt vet lint test

fmt:
    gofmt -l -w .

vet:
    go vet ./...

# needs golangci-lint, brew install golangci-lint
lint:
    golangci-lint run ./...

# known cves in anything reachable from our code, including transitively
vuln:
    go run golang.org/x/vuln/cmd/govulncheck@latest ./...

# build the binary into ./bin
build:
    @mkdir -p bin
    go build -o bin/giro ./cmd/giro

# --- database ---------------------------------------------------------------

# psql against the dev database
db:
    @psql "$DATABASE_URL"

# psql against the test database
db-test:
    @psql "$GIRO_TEST_DATABASE_URL"

# what the ledger currently holds, per asset
[no-exit-message]
balances:
    @psql "$DATABASE_URL" -c "select asset, sum(input) - sum(output) as drift from accounts_volumes group by asset" 2>/dev/null || echo "no accounts_volumes table yet"

# advisory locks currently held, useful when a migration seems stuck
locks:
    @psql "$DATABASE_URL" -c "select database, classid, objid, mode, granted, pid from pg_locks where locktype = 'advisory'"

# drop every table in the dev database and re-migrate. destructive
[confirm("this drops every table in DATABASE_URL. continue?")]
db-reset:
    @psql "$DATABASE_URL" -qc "drop schema public cascade; create schema public;"
    @just migrate

# same for the test database. destructive
[confirm("this drops every table in GIRO_TEST_DATABASE_URL. continue?")]
db-reset-test:
    @psql "$GIRO_TEST_DATABASE_URL" -qc "drop schema public cascade; create schema public;"

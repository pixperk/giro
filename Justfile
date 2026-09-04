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

# --- api ---

# run with a version suffix rather than a tool directive in go.mod, so the
# generator and its dependency tree stay out of this module. the generated file
# is committed and ci checks it is not stale.
#
# regenerate the go types from the openapi contract
generate:
    @go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.8.0 \
        -config api/codegen.yaml api/openapi.yaml
    @gofmt -w internal/api/gen.go
    @echo "regenerated internal/api/gen.go"

# serve the api, with docs at /docs
serve:
    @go run ./cmd/giro serve

# --- development ------------------------------------------------------------

# sustained load, with latency percentiles and the invariants checked after.
#
# not part of "just check": these take minutes. pass a duration to soak, for
# example "just load 60s", which also enables the leak check.
load DURATION="5s":
    @GIRO_LOAD={{ DURATION }} go test -run TestLoad -v -timeout 30m ./storage/

# the per-operation benchmarks, which answer a different question to the above:
# what one commit costs with nothing else happening.
bench COUNT="200x":
    @go test -bench . -benchtime {{ COUNT }} -run '^$' ./storage/

# run every test with the race detector.
#
# obs is a separate module, so ./... does not reach it. a nested module that
# nothing runs is one that compiles against a version of the engine it has not
# seen in months.
test:
    go test -race ./...
    cd obs && go test -race ./...

# the suite as the restricted application role, which is how it runs in
# production. everything that is an application path must pass. the ones that
# skip are those that have to damage the book to prove the damage is noticed,
# and under this role they cannot stage their attack.
test-restricted:
    GIRO_TEST_ROLE=giro_app go test -race ./storage/

# replay a property test failure. the seed is printed by every run, and CI
# prints it too, so a failure carries its own reproduction.
#
#   just replay 1788499781666073000
replay SEED:
    GIRO_TEST_SEED={{ SEED }} go test -race -v -run 'Conserve|Random|Ordering' ./...

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
check: fmt vet lint test test-restricted

fmt:
    gofmt -l -w .

vet:
    go vet ./...
    @cd obs && go vet ./...

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

# pre-commit runs fmt, vet and lint. pre-push runs the tests and checks
# migrations still apply to an empty database.
#
# install the git hooks
hooks:
    @git config core.hooksPath .githooks
    @echo "hooks installed, skip any of them with --no-verify"

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
    @just db-sweep

# create the local login role the service should connect as: no privileges of
# its own, and a member of giro_app.
#
# membership rather than "set role" on the application's own connection,
# because postgres roles inherit by default and a role with nothing of its own
# has nothing to fall back to. "reset role" is then not a way out of the
# restriction, it is a way back to holding even less.
#
# no password: local connections here are trusted. a real deployment issues a
# credential and keeps it wherever the rest of them live, not in the schema.
db-app-role:
    @psql "$DATABASE_URL" -q -c "do \$\$ begin       if not exists (select 1 from pg_roles where rolname = 'giro_service') then         create role giro_service login;       end if; end \$\$;" -c "grant giro_app to giro_service"
    @echo "created giro_service, a member of giro_app."
    @echo "point the serving DATABASE_URL at it. migrations still need the owner."

# run every invariant check and record that it ran. exits 1 on a finding.
verify:
    @go run ./cmd/giro verify --stale-after=4h

# when each check last ran, and whether anything has stopped. the other half of
# alerting: a detector that stopped running looks exactly like a book with
# nothing wrong. see deploy/README.md for scheduling it.
verify-last MAXAGE="":
    @go run ./cmd/giro verify --last {{ if MAXAGE != "" { "--max-age=" + MAXAGE } else { "" } }}

# what the serving connection can actually do, which is the only version of
# this that counts
privileges:
    @psql "$DATABASE_URL" -qc "select current_user,       current_setting('is_superuser')::bool as superuser,       pg_has_role(current_user, (select relowner from pg_class where oid = to_regclass('logs')), 'USAGE') as owns_tables,       has_table_privilege('logs','INSERT') as can_append,       has_table_privilege('logs','TRUNCATE') as can_erase,       has_table_privilege('logs','UPDATE') as can_rewrite"

# every test makes its own schema and drops it on cleanup, but an interrupted
# run leaves one behind and they accumulate invisibly. this sweeps them.
db-sweep:
    @psql "$GIRO_TEST_DATABASE_URL" -q -c "do \$\$ declare s record; begin       for s in select schema_name from information_schema.schemata       where schema_name like 't\\_%' or schema_name like 'api\\_%'       loop execute format('drop schema %I cascade', s.schema_name); end loop; end \$\$;" 2>/dev/null
    @psql "$GIRO_TEST_DATABASE_URL" -qtc "select count(*) || ' test schemas left' from information_schema.schemata where schema_name like 't\\_%' or schema_name like 'api\\_%'"

package migrate

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// migrations are named <utc timestamp>_<slug>.sql, so lexical order is
// chronological order. timestamps rather than sequence numbers because two
// people on different branches never collide.
const TimestampLayout = "20060102150405"

var filenamePattern = regexp.MustCompile(`^(\d{14})_([a-z0-9]+(?:_[a-z0-9]+)*)\.sql$`)

// a file carrying this directive runs outside a transaction. needed for
// statements postgres refuses to run in one, create index concurrently being
// the one that matters on a live ledger.
const noTransactionDirective = "-- giro:no-transaction"

type Migration struct {
	Version  int64  // 20260831142530
	Name     string // init_schema
	Filename string
	SQL      string
	Checksum string // sha256 of the file bytes
	NoTx     bool
}

// reads every .sql file in the root of fsys and returns them ordered by
// version. anything that is not a .sql file is ignored, so a .gitkeep or a
// README can sit alongside them.
func Load(fsys fs.FS) ([]Migration, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("read migrations dir: %w", err)
	}

	migrations := make([]Migration, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}

		version, name, err := parseFilename(e.Name())
		if err != nil {
			return nil, err
		}

		body, err := fs.ReadFile(fsys, e.Name())
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", e.Name(), err)
		}

		sum := sha256.Sum256(body)
		migrations = append(migrations, Migration{
			Version:  version,
			Name:     name,
			Filename: e.Name(),
			SQL:      string(body),
			Checksum: hex.EncodeToString(sum[:]),
			NoTx:     hasDirective(string(body), noTransactionDirective),
		})
	}

	slices.SortFunc(migrations, func(a, b Migration) int {
		return int(a.Version - b.Version)
	})

	for i := 1; i < len(migrations); i++ {
		if migrations[i].Version == migrations[i-1].Version {
			return nil, fmt.Errorf("duplicate migration version %d: %s and %s",
				migrations[i].Version, migrations[i-1].Filename, migrations[i].Filename)
		}
	}

	return migrations, nil
}

func parseFilename(filename string) (version int64, name string, err error) {
	m := filenamePattern.FindStringSubmatch(filename)
	if m == nil {
		return 0, "", fmt.Errorf("migration %q does not match <14 digit timestamp>_<lower_snake_case>.sql", filename)
	}

	version, err = strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0, "", fmt.Errorf("migration %q has an unparseable version: %w", filename, err)
	}

	return version, m[2], nil
}

// looks for the directive on its own line, so it cannot be triggered by the
// text appearing inside a string literal or a longer comment.
func hasDirective(sql, directive string) bool {
	for line := range strings.Lines(sql) {
		if strings.EqualFold(strings.TrimSpace(line), directive) {
			return true
		}
	}
	return false
}

package migrate

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

const stub = `-- %s
--
-- forward only. to undo something, write a new migration.
-- add "%s" on its own line if this cannot run inside a transaction,
-- for example create index concurrently.

`

// Create writes an empty migration into dir, named for the current utc time, and
// returns its path.
func Create(dir, name string) (string, error) {
	return newAt(dir, name, time.Now())
}

// the clock is a parameter so the collision guard below can be tested without
// racing a real second.
func newAt(dir, name string, now time.Time) (string, error) {
	slug := Slugify(name)
	if slug == "" {
		return "", fmt.Errorf("migration name %q has no usable characters", name)
	}

	filename := fmt.Sprintf("%s_%s.sql", now.UTC().Format(TimestampLayout), slug)
	path := filepath.Join(dir, filename)

	if _, err := os.Stat(path); err == nil {
		return "", fmt.Errorf("%s already exists", path)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}

	body := fmt.Sprintf(stub, slug, noTransactionDirective)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// "Add metadata tables!" -> "add_metadata_tables"
func Slugify(name string) string {
	s := nonSlug.ReplaceAllString(strings.ToLower(name), "_")
	return strings.Trim(s, "_")
}

package factory

// The migration-safety lint (spec 8.4, milestone 1a): every migration the
// branch adds is named, with whether it uses the repository's gh-ost marker
// (ghost_marker) and for which environments, the tables it changes, those
// that are large by table_stats and large_table_rows, and whether it says an
// online algorithm. A large table changed without the marker and without an
// online algorithm, a gh-ost migration of two tables, and a migration with
// no way back are flagged. The precheck and the ready card show it; neither
// the factory nor an agent ever runs gh-ost or a migration on a shared
// database.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/zhuravel/magnum/internal/config"
)

// MigrationFlag is one migration the branch adds, as the lint reads it: its
// path (relative to the checkout), whether it uses the repository's gh-ost
// marker and the environments it names there, the tables it changes, those
// of them at or over large_table_rows, whether it says an online algorithm
// (ALGORITHM=INPLACE or INSTANT, LOCK=NONE, algorithm: :inplace, :instant
// or :concurrently), whether it has no way back (only an up, a down that
// raises, a change Rails cannot reverse; a gh-ost migration's down refuses
// by the marker's design and is not counted), and whether it is a gh-ost
// migration of more than one table. NoDatabases is its folder when no
// databases are set for it (migrationDatabases): the precheck does not run
// it, and says so.
type MigrationFlag struct {
	Path                                   string
	Ghost                                  bool
	Envs, Tables, Large                    []string
	Online, Irreversible, TwoTablesInGhost bool
	NoDatabases                            string
	// rows are the sizes of the Large tables, for Problems.
	rows map[string]int64
}

// Problems are what the lint flags in f, in plain words; none for a
// migration it lets go.
func (f MigrationFlag) Problems() []string {
	var out []string
	if len(f.Large) > 0 && !f.Ghost && !f.Online {
		var sizes []string
		for _, t := range f.Large {
			sizes = append(sizes, fmt.Sprintf("%s: %d rows", t, f.rows[t]))
		}
		out = append(out, "changes a large table ("+strings.Join(sizes, ", ")+") without gh-ost and without an online algorithm")
	}
	if f.TwoTablesInGhost {
		out = append(out, "a gh-ost migration of two tables or more ("+strings.Join(f.Tables, ", ")+"): gh-ost changes one table")
	}
	if f.Irreversible {
		out = append(out, "has no way back: its down is missing or raises, or Rails cannot reverse its change")
	}
	return out
}

// String is f in one line: its file, its gh-ost environments, its tables,
// its problems and, when the precheck does not run it, why.
func (f MigrationFlag) String() string {
	s := f.Path
	if f.Ghost {
		s += " (gh-ost on " + cmpJoin(f.Envs, "no environment named") + ")"
	}
	if len(f.Tables) > 0 {
		s += ": " + strings.Join(f.Tables, ", ")
	}
	if ps := f.Problems(); len(ps) > 0 {
		s += "; " + strings.Join(ps, "; ")
	}
	if f.NoDatabases != "" {
		s += "; " + notRunWords(f.NoDatabases)
	}
	return s
}

// notRunWords say why the precheck does not run a migration of folder.
func notRunWords(folder string) string { return "not run: no databases are set for " + folder }

// isMigration reports whether path, a file the change adds, is a
// migration (config.MigrationFolder): a file directly in a migration folder
// whose name starts with its version, never a spec, a helper or a nested
// file.
func isMigration(path string) bool {
	_, ok := config.MigrationFolder(path)
	return ok
}

// migrationDatabases are the databases the precheck runs migration path
// on, and its folder: those precheck.migration_databases sets for the
// folder; without the key, one run with no database ("") for a migration of
// config.DefaultMigrationFolder, and none for another folder's. None: the
// precheck does not run it.
func migrationDatabases(pc config.FactoryPrecheck, path string) (folder string, dbs []string) {
	folder, _ = config.MigrationFolder(path)
	switch {
	case len(pc.MigrationDatabases) > 0:
		return folder, pc.MigrationDatabases[folder]
	case folder == config.DefaultMigrationFolder:
		return folder, []string{""}
	}
	return folder, nil
}

// cmpJoin is words joined with ", ", or none when there are none.
func cmpJoin(words []string, none string) string {
	if len(words) == 0 {
		return none
	}
	return strings.Join(words, ", ")
}

// The patterns the lint reads a migration with.
var (
	tableCallRe = regexp.MustCompile(`\b(?:add_column|remove_column|change_column|change_column_null|change_column_default|rename_column|` +
		`add_index|remove_index|add_reference|remove_reference|add_belongs_to|remove_belongs_to|add_timestamps|remove_timestamps|` +
		`change_table|create_table|drop_table|rename_table|add_foreign_key|remove_foreign_key|add_check_constraint|` +
		`remove_check_constraint|remove_columns)\b\s*\(?\s*:?["']?([A-Za-z_][A-Za-z0-9_]*)`)
	tableSQLRe = regexp.MustCompile("(?i)\\b(?:(?:ALTER|DROP|TRUNCATE|CREATE|RENAME)\\s+TABLE(?:\\s+IF\\s+(?:NOT\\s+)?EXISTS)?|UPDATE|DELETE\\s+FROM|INSERT\\s+INTO)\\s+`?([A-Za-z_][A-Za-z0-9_]*)`?")
	tableKeyRe = regexp.MustCompile(`\btable(?:_name)?:\s*:?["']?([A-Za-z_][A-Za-z0-9_]*)`)
	onlineRe   = regexp.MustCompile(`(?i)(?:\balgorithm\s*(?::|=>|=)\s*:?["']?(?:inplace|instant|concurrently)\b|\bLOCK\s*=\s*NONE\b)`)
	raisesRe   = regexp.MustCompile(`\braise\s+(?:ActiveRecord::)?IrreversibleMigration\b`)
	defUpRe    = regexp.MustCompile(`(?m)^\s*def\s+up\b`)
	defDownRe  = regexp.MustCompile(`(?m)^\s*def\s+down\b`)
	defChangRe = regexp.MustCompile(`(?m)^\s*def\s+change\b`)
	// unreversibleRe are the steps of a change that Rails cannot reverse
	// without a reversible block: SQL and a column's type change.
	unreversibleRe = regexp.MustCompile(`(?m)^\s*(?:execute\b|change_column\s)`)
	reversibleRe   = regexp.MustCompile(`\b(?:reversible|up_only)\b`)
	envWordRe      = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_-]*`)
)

// ScanMigrations reads each of added (paths of migrations relative to
// checkout, the task's copy at the head) and names it with its flags, in order. The
// marker comes from repo's ghost_marker (none: no migration is a gh-ost
// one), the sizes from its table_stats file in the checkout and its
// large_table_rows (none: no table is large); a table_stats file that is
// missing or not JSON is an error, since the sizes cannot be told. A
// migration that is no gh-ost one and that the precheck has no databases
// for (migrationDatabases) has its folder as NoDatabases.
func ScanMigrations(checkout string, added []string, repo config.FactoryRepo) ([]MigrationFlag, error) {
	if len(added) == 0 {
		return nil, nil
	}
	var sizes map[string]int64
	if repo.TableStats != "" && repo.LargeTableRows > 0 {
		var err error
		if sizes, err = readTableStats(filepath.Join(checkout, repo.TableStats)); err != nil {
			return nil, fmt.Errorf("table_stats %s: %w", repo.TableStats, err)
		}
	}
	out := make([]MigrationFlag, 0, len(added))
	for _, rel := range added {
		b, err := os.ReadFile(filepath.Join(checkout, rel))
		if err != nil {
			return nil, fmt.Errorf("migration %s: %w", rel, err)
		}
		f := scanMigration(rel, string(b), repo, sizes)
		if folder, dbs := migrationDatabases(repo.Precheck, rel); !f.Ghost && len(dbs) == 0 {
			f.NoDatabases = folder
		}
		out = append(out, f)
	}
	return out, nil
}

// scanMigration is one migration's flags.
func scanMigration(rel, text string, repo config.FactoryRepo, sizes map[string]int64) MigrationFlag {
	f := MigrationFlag{Path: rel}
	if m := repo.GhostMarker; m != "" {
		if _, after, ok := strings.Cut(text, m); ok {
			f.Ghost = true
			f.Envs = ghostEnvs(after)
		}
	}
	add := func(t string) {
		if t != "" && !slices.Contains(f.Tables, t) {
			f.Tables = append(f.Tables, t)
		}
	}
	type hit struct {
		at    int
		table string
	}
	var hits []hit
	for _, re := range []*regexp.Regexp{tableCallRe, tableSQLRe, tableKeyRe} {
		for _, m := range re.FindAllStringSubmatchIndex(text, -1) {
			hits = append(hits, hit{m[2], text[m[2]:m[3]]})
		}
	}
	slices.SortFunc(hits, func(a, b hit) int { return a.at - b.at })
	for _, h := range hits {
		add(h.table)
	}
	for _, t := range f.Tables {
		if n, ok := sizes[t]; ok && n >= repo.LargeTableRows {
			f.Large = append(f.Large, t)
			if f.rows == nil {
				f.rows = map[string]int64{}
			}
			f.rows[t] = n
		}
	}
	f.Online = onlineRe.MatchString(text)
	f.TwoTablesInGhost = f.Ghost && len(f.Tables) > 1
	if !f.Ghost {
		up, down, change := defUpRe.MatchString(text), defDownRe.MatchString(text), defChangRe.MatchString(text)
		f.Irreversible = raisesRe.MatchString(text) || (up && !down) ||
			(change && unreversibleRe.MatchString(text) && !reversibleRe.MatchString(text))
	}
	return f
}

// ghostEnvs are the environments the marker names right after it, in
// brackets or parentheses: [:production, :bastion], %i[production], ("x").
func ghostEnvs(after string) []string {
	after = strings.TrimLeft(after, " \t")
	if after == "" || (after[0] != '[' && after[0] != '(') {
		return nil
	}
	closer := map[byte]byte{'[': ']', '(': ')'}[after[0]]
	body := after[1:]
	body = strings.NewReplacer("%i[", " ", "%I[", " ", "%w[", " ", "%W[", " ").Replace(body)
	if i := strings.IndexByte(body, closer); i >= 0 {
		body = body[:i]
	}
	var envs []string
	for _, w := range envWordRe.FindAllString(body, -1) {
		if !slices.Contains(envs, w) {
			envs = append(envs, w)
		}
	}
	return envs
}

// readTableStats reads the table sizes of a stats file: a JSON object of
// table to rows ({"users": 120}) or to an object with the rows
// ({"users": {"rows": 120}}: rows, row_count, table_rows or count), or a
// list of objects with the table's name (table, name or table_name) and its
// rows.
func readTableStats(path string) (map[string]int64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, fmt.Errorf("not JSON: %w", err)
	}
	out := map[string]int64{}
	switch v := v.(type) {
	case map[string]any:
		for table, x := range v {
			if n, ok := rowsOf(x); ok {
				out[table] = n
			}
		}
	case []any:
		for _, x := range v {
			obj, ok := x.(map[string]any)
			if !ok {
				continue
			}
			name := ""
			for _, k := range []string{"table", "name", "table_name"} {
				if s, ok := obj[k].(string); ok {
					name = s
					break
				}
			}
			if n, ok := rowsOf(obj); ok && name != "" {
				out[name] = n
			}
		}
	default:
		return nil, errors.New("neither an object nor a list of tables")
	}
	return out, nil
}

// rowsOf is the row count of a stats entry: a number, or an object's rows.
func rowsOf(x any) (int64, bool) {
	switch x := x.(type) {
	case float64:
		return int64(x), true
	case map[string]any:
		for _, k := range []string{"rows", "row_count", "table_rows", "count"} {
			if n, ok := x[k].(float64); ok {
				return int64(n), true
			}
		}
	}
	return 0, false
}

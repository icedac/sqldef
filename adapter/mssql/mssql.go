package mssql

import (
	"database/sql"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	_ "github.com/denisenkom/go-mssqldb"
	"github.com/k0kubun/sqldef/adapter"
	"github.com/k0kubun/sqldef/sqlparser"
)

const indent = "    "

var reservedKeywords = make(map[string]bool)

func init() {
	// build reserved keyword map
	keywords := sqlparser.Keywords()
	for keyword := range keywords {
		reservedKeywords[strings.ToLower(keyword)] = true
	}
}

func getSafeColumnName(name string) string {
	if _, ok := reservedKeywords[strings.ToLower(name)]; ok {
		return "[" + name + "]"
	}
	return name
}

type MssqlDatabase struct {
	config adapter.Config
	db     *sql.DB
}

func NewDatabase(config adapter.Config) (adapter.Database, error) {
	db, err := sql.Open("sqlserver", mssqlBuildDSN(config))
	if err != nil {
		return nil, err
	}

	return &MssqlDatabase{
		db:     db,
		config: config,
	}, nil
}

func (d *MssqlDatabase) TableNames() ([]string, error) {
	rows, err := d.db.Query(
		`select schema_name(schema_id) as table_schema, name from sys.objects where type = 'U' ORDER BY sys.objects.name;`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	tables := []string{}
	for rows.Next() {
		var schema, name string
		if err := rows.Scan(&schema, &name); err != nil {
			return nil, err
		}
		tables = append(tables, schema+"."+name)
	}

	// order table by reference dependency
	// it may have cyclic dependency if fk created later
	// TODO: perfect solution is creating fk later but current dumptable interface don't allow it
	// so try our bests and adds cyclic dependency tables later
	foreignTableMap := make(map[string][]string)
	for _, tableName := range tables {
		_, foreignTableNames, err := d.getForeignDefs(tableName)
		if err != nil {
			return nil, err
		}
		foreignTableMap[tableName] = foreignTableNames
	}

	dependencyOrderedTables := []string{}
	addedTables := make(map[string]bool)
	for len(tables) > 0 {
		var lastTableLen = len(tables)
		for i := 0; i < len(tables); {
			tableName := tables[i]
			noDeps := true
			fTableNames := foreignTableMap[tableName]
			for _, fTableName := range fTableNames {
				if _, added := addedTables[fTableName]; !added {
					noDeps = false
					break
				}
			}
			if noDeps {
				addedTables[tableName] = true
				dependencyOrderedTables = append(dependencyOrderedTables, tableName)
				tables[i] = tables[len(tables)-1]
				tables = tables[:len(tables)-1]
			} else {
				i = i + 1
			}
		}
		if lastTableLen == len(tables) {
			break
		}
	}

	// leftover 'tables' have cyclic dependencies
	dependencyOrderedTables = append(dependencyOrderedTables, tables...)

	return dependencyOrderedTables, nil
}

func (d *MssqlDatabase) DumpTableDDL(table string) (string, error) {
	cols, err := d.getColumns(table)
	if err != nil {
		return "", err
	}
	indexDefs, err := d.getIndexDefs(table)
	if err != nil {
		return "", err
	}
	foreignDefs, _, err := d.getForeignDefs(table)
	if err != nil {
		return "", err
	}
	return buildDumpTableDDL(table, cols, indexDefs, foreignDefs), nil
}

func buildDumpTableDDL(table string, columns []column, indexDefs []*indexDef, foreignDefs []string) string {
	var queryBuilder strings.Builder
	fmt.Fprintf(&queryBuilder, "CREATE TABLE %s (", table)
	for i, col := range columns {
		if i > 0 {
			fmt.Fprint(&queryBuilder, ",")
		}
		fmt.Fprint(&queryBuilder, "\n"+indent)

		// col.name check if reserved keywords or not
		var col_name string = getSafeColumnName(col.Name)

		fmt.Fprintf(&queryBuilder, "%s %s", col_name, col.dataType)
		if length, ok := col.getLength(); ok {
			fmt.Fprintf(&queryBuilder, "(%s)", length)
		}
		if !col.Nullable {
			fmt.Fprint(&queryBuilder, " NOT NULL")
		}
		if col.DefaultName != "" {
			fmt.Fprintf(&queryBuilder, " CONSTRAINT %s DEFAULT %s", col.DefaultName, col.DefaultVal)
		}
		if col.Identity != nil {
			fmt.Fprintf(&queryBuilder, " IDENTITY(%s,%s)", col.Identity.SeedValue, col.Identity.IncrementValue)
			if col.Identity.NotForReplication {
				fmt.Fprint(&queryBuilder, " NOT FOR REPLICATION")
			}
		}
		if col.Check != nil {
			fmt.Fprintf(&queryBuilder, " CONSTRAINT [%s] CHECK", col.Check.Name)
			if col.Check.NotForReplication {
				fmt.Fprint(&queryBuilder, " NOT FOR REPLICATION")
			}
			fmt.Fprintf(&queryBuilder, " %s", col.Check.Definition)
		}
	}

	// PRIMARY KEY
	for _, indexDef := range indexDefs {
		if !indexDef.primary {
			continue
		}
		fmt.Fprint(&queryBuilder, ",\n"+indent)
		fmt.Fprintf(&queryBuilder, "CONSTRAINT [%s] PRIMARY KEY", indexDef.name)

		if indexDef.indexType == "CLUSTERED" || indexDef.indexType == "NONCLUSTERED" {
			fmt.Fprintf(&queryBuilder, " %s", indexDef.indexType)
		}
		fmt.Fprintf(&queryBuilder, " (%s)", strings.Join(indexDef.columns, ", "))
		if len(indexDef.options) > 0 {
			fmt.Fprint(&queryBuilder, " WITH (")
			for i, option := range indexDef.options {
				if i > 0 {
					fmt.Fprint(&queryBuilder, ",")
				}
				fmt.Fprintf(&queryBuilder, " %s", fmt.Sprintf("%s = %s", option.name, option.value))
			}
			fmt.Fprint(&queryBuilder, " )")
		}
	}

	for _, v := range foreignDefs {
		fmt.Fprint(&queryBuilder, ",\n"+indent)
		fmt.Fprint(&queryBuilder, v)
	}

	for _, indexDef := range indexDefs {
		if indexDef.primary {
			continue
		}
		if len(indexDef.included) > 0 {
			continue
		}

		fmt.Fprint(&queryBuilder, ",\n"+indent)

		fmt.Fprintf(&queryBuilder, "INDEX [%s]", indexDef.name)
		if indexDef.unique {
			fmt.Fprint(&queryBuilder, " UNIQUE")
		}
		if indexDef.indexType == "CLUSTERED" || indexDef.indexType == "NONCLUSTERED" {
			fmt.Fprintf(&queryBuilder, " %s", indexDef.indexType)
		}

		fmt.Fprintf(&queryBuilder, " (%s)", strings.Join(indexDef.columns, ", "))

		if len(indexDef.options) > 0 {
			fmt.Fprint(&queryBuilder, " WITH (")
			for i, option := range indexDef.options {
				if i > 0 {
					fmt.Fprint(&queryBuilder, ",")
				}

				fmt.Fprintf(&queryBuilder, " %s", fmt.Sprintf("%s = %s", option.name, option.value))
			}
			fmt.Fprint(&queryBuilder, " )")
		}
	}

	fmt.Fprintf(&queryBuilder, "\n);\n")

	// export index with include here
	for _, indexDef := range indexDefs {
		if len(indexDef.included) == 0 {
			continue
		}
		if indexDef.primary {
			continue
		}
		fmt.Fprint(&queryBuilder, "\nCREATE")
		if indexDef.unique {
			fmt.Fprint(&queryBuilder, " UNIQUE")
		}
		if indexDef.indexType == "CLUSTERED" || indexDef.indexType == "NONCLUSTERED" {
			fmt.Fprintf(&queryBuilder, " %s", indexDef.indexType)
		}
		fmt.Fprintf(&queryBuilder, " INDEX [%s] ON %s (%s)", indexDef.name, table, strings.Join(indexDef.columns, ", "))
		if len(indexDef.included) > 0 {
			fmt.Fprintf(&queryBuilder, " INCLUDE (%s)", strings.Join(indexDef.included, ", "))
		}
		if len(indexDef.options) > 0 {
			fmt.Fprint(&queryBuilder, " WITH (")
			for i, option := range indexDef.options {
				if i > 0 {
					fmt.Fprint(&queryBuilder, ",")
				}
				fmt.Fprintf(&queryBuilder, " %s", fmt.Sprintf("%s = %s", option.name, option.value))
			}
			fmt.Fprint(&queryBuilder, " )")
		}
		fmt.Fprint(&queryBuilder, ";")
	}

	return strings.TrimSuffix(queryBuilder.String(), "\n")
}

type column struct {
	Name        string
	dataType    string
	MaxLength   string
	Scale       string
	Nullable    bool
	Identity    *identity
	DefaultName string
	DefaultVal  string
	Check       *check
}

func (c column) getLength() (string, bool) {
	switch c.dataType {
	case "char", "varchar", "binary", "varbinary":
		if c.MaxLength == "-1" {
			return "max", true
		}
		return c.MaxLength, true
	case "nvarchar", "nchar":
		if c.MaxLength == "-1" {
			return "max", true
		}
		maxLength, err := strconv.Atoi(c.MaxLength)
		if err != nil {
			return "", false
		}
		return strconv.Itoa(int(maxLength / 2)), true
	case "datetimeoffset":
		if c.Scale == "7" {
			return "", false
		}
		return c.Scale, true
	}
	return "", false
}

type identity struct {
	SeedValue         string
	IncrementValue    string
	NotForReplication bool
}

type check struct {
	Name              string
	Definition        string
	NotForReplication bool
}

func (d *MssqlDatabase) getColumns(table string) ([]column, error) {
	schema, table := splitTableName(table)
	query := fmt.Sprintf(`SELECT
	c.name,
	[type_name] = tp.name,
	c.max_length,
	c.scale,
	c.is_nullable,
	c.is_identity,
	ic.seed_value,
	ic.increment_value,
	ic.is_not_for_replication,
	c.default_object_id,
	default_name = OBJECT_NAME(c.default_object_id),
	default_definition = OBJECT_DEFINITION(c.default_object_id),
	cc.name,
	cc.definition,
	cc.is_not_for_replication
FROM sys.columns c WITH(NOLOCK)
JOIN sys.types tp WITH(NOLOCK) ON c.user_type_id = tp.user_type_id
LEFT JOIN sys.check_constraints cc WITH(NOLOCK) ON c.[object_id] = cc.parent_object_id AND cc.parent_column_id = c.column_id
LEFT JOIN sys.identity_columns ic WITH(NOLOCK) ON c.[object_id] = ic.[object_id] AND ic.[column_id] = c.[column_id]
WHERE c.[object_id] = OBJECT_ID('%s.%s', 'U')`, schema, table)

	rows, err := d.db.Query(query)
	if err != nil {
		fmt.Println(err)
	}
	defer rows.Close()

	cols := []column{}
	for rows.Next() {
		col := column{}
		var colName, dataType, maxLen, scale, defaultId string
		var seedValue, incrementValue, defaultName, defaultVal, checkName, checkDefinition *string
		var isNullable, isIdentity bool
		var identityNotForReplication, checkNotForReplication *bool
		err = rows.Scan(&colName, &dataType, &maxLen, &scale, &isNullable, &isIdentity, &seedValue, &incrementValue, &identityNotForReplication, &defaultId, &defaultName, &defaultVal, &checkName, &checkDefinition, &checkNotForReplication)
		if err != nil {
			return nil, err
		}
		col.Name = colName
		col.MaxLength = maxLen
		col.Scale = scale
		if defaultId != "0" {
			col.DefaultName = *defaultName
			col.DefaultVal = *defaultVal
		}
		col.Nullable = isNullable
		col.dataType = dataType
		if isIdentity {
			col.Identity = &identity{
				SeedValue:         *seedValue,
				IncrementValue:    *incrementValue,
				NotForReplication: *identityNotForReplication,
			}
		}
		if checkName != nil {
			col.Check = &check{
				Name:              *checkName,
				Definition:        *checkDefinition,
				NotForReplication: *checkNotForReplication,
			}
		}
		cols = append(cols, col)
	}
	return cols, nil
}

type indexDef struct {
	name      string
	columns   []string
	primary   bool
	unique    bool
	indexType string
	included  []string
	options   []indexOption
}

type indexOption struct {
	name  string
	value string
}

func (d *MssqlDatabase) getIndexDefs(table string) ([]*indexDef, error) {
	schema, table := splitTableName(table)
	query := fmt.Sprintf(`SELECT
	ind.name AS index_name,
	ind.is_primary_key,
	ind.is_unique,
	ind.type_desc,
	ind.is_padded,
	ind.fill_factor,
	ind.ignore_dup_key,
	st.no_recompute,
	st.is_incremental,
	ind.allow_row_locks,
	ind.allow_page_locks
FROM sys.indexes ind
INNER JOIN sys.stats st ON ind.object_id = st.object_id AND ind.index_id = st.stats_id
WHERE ind.object_id = OBJECT_ID('[%s].[%s]')`, schema, table)

	rows, err := d.db.Query(query)
	if err != nil {
		return nil, err
	}

	indexDefMap := make(map[string]*indexDef)
	var indexName, typeDesc, fillfactor string
	var isPrimary, isUnique, padIndex, ignoreDupKey, noRecompute, incremental, rowLocks, pageLocks bool
	for rows.Next() {
		err = rows.Scan(&indexName, &isPrimary, &isUnique, &typeDesc, &padIndex, &fillfactor, &ignoreDupKey, &noRecompute, &incremental, &rowLocks, &pageLocks)
		if err != nil {
			return nil, err
		}

		options := []indexOption{
			{name: "PAD_INDEX", value: boolToOnOff(padIndex)},
			{name: "IGNORE_DUP_KEY", value: boolToOnOff(ignoreDupKey)},
			{name: "STATISTICS_NORECOMPUTE", value: boolToOnOff(noRecompute)},
			{name: "STATISTICS_INCREMENTAL", value: boolToOnOff(incremental)},
			{name: "ALLOW_ROW_LOCKS", value: boolToOnOff(rowLocks)},
			{name: "ALLOW_PAGE_LOCKS", value: boolToOnOff(pageLocks)},
		}

		// add FILLFACTOR only if it's not 0
		if fillfactor != "0" {
			options = append(options, indexOption{name: "FILLFACTOR", value: fillfactor})
		}

		definition := &indexDef{name: indexName, columns: []string{}, primary: isPrimary, unique: isUnique, indexType: typeDesc, included: []string{}, options: options}
		indexDefMap[indexName] = definition
	}

	rows.Close()

	query = fmt.Sprintf(`SELECT
	ind.name AS index_name,
	COL_NAME(ic.object_id, ic.column_id) AS column_name,
	ic.is_descending_key,
	ic.is_included_column
FROM sys.indexes ind
INNER JOIN sys.index_columns ic ON ind.object_id = ic.object_id AND ind.index_id = ic.index_id
WHERE ind.object_id = OBJECT_ID('[%s].[%s]')`, schema, table)

	rows, err = d.db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var columnName string
	var isDescending, isIncluded bool
	for rows.Next() {
		err = rows.Scan(&indexName, &columnName, &isDescending, &isIncluded)
		if err != nil {
			return nil, err
		}

		columnDefinition := fmt.Sprintf("[%s]", columnName)

		if isIncluded {
			indexDefMap[indexName].included = append(indexDefMap[indexName].included, columnDefinition)
		} else {
			if isDescending {
				columnDefinition += " DESC"
			}
			indexDefMap[indexName].columns = append(indexDefMap[indexName].columns, columnDefinition)
		}
	}

	indexDefs := make([]*indexDef, 0)
	for _, definition := range indexDefMap {
		indexDefs = append(indexDefs, definition)
	}

	// sort index for deterministic test
	sort.Slice(indexDefs, func(i, j int) bool {
		return indexDefs[i].name < indexDefs[j].name
	})

	return indexDefs, nil
}

func (d *MssqlDatabase) getForeignDefs(table string) ([]string, []string, error) {
	schema, table := splitTableName(table)
	query := fmt.Sprintf(`SELECT
	f.name,
	COL_NAME(f.parent_object_id, fc.parent_column_id),
	OBJECT_NAME(f.referenced_object_id)_id,
	COL_NAME(f.referenced_object_id, fc.referenced_column_id),
	f.update_referential_action_desc,
	f.delete_referential_action_desc,
	f.is_not_for_replication
FROM sys.foreign_keys f INNER JOIN sys.foreign_key_columns fc ON f.OBJECT_ID = fc.constraint_object_id
WHERE f.parent_object_id = OBJECT_ID('[%s].[%s]')`, schema, table)

	rows, err := d.db.Query(query)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	foreignTableNames := make([]string, 0)

	defs := make([]string, 0)
	for rows.Next() {
		var constraintName, columnName, foreignTableName, foreignColumnName, foreignUpdateRule, foreignDeleteRule string
		var notForReplication bool
		err = rows.Scan(&constraintName, &columnName, &foreignTableName, &foreignColumnName, &foreignUpdateRule, &foreignDeleteRule, &notForReplication)
		if err != nil {
			return nil, nil, err
		}
		foreignTableName = schema + "." + foreignTableName
		foreignUpdateRule = strings.Replace(foreignUpdateRule, "_", " ", -1)
		foreignDeleteRule = strings.Replace(foreignDeleteRule, "_", " ", -1)
		def := fmt.Sprintf("CONSTRAINT [%s] FOREIGN KEY ([%s]) REFERENCES %s ([%s]) ON UPDATE %s ON DELETE %s", constraintName, columnName, foreignTableName, foreignColumnName, foreignUpdateRule, foreignDeleteRule)
		if notForReplication {
			def += " NOT FOR REPLICATION"
		}
		foreignTableNames = append(foreignTableNames, foreignTableName)
		defs = append(defs, def)
	}

	return defs, foreignTableNames, nil
}

func boolToOnOff(in bool) string {
	if in {
		return "ON"
	} else {
		return "OFF"
	}
}

var (
	suffixSemicolon = regexp.MustCompile(`;$`)
	spaces          = regexp.MustCompile(`[ ]+`)
)

func (d *MssqlDatabase) Views() ([]string, error) {
	// azure sql server has some system view only distinguished by 'is_ms_shipped = 0' check
	const sql = `SELECT
	sys.views.name as name,
	sys.sql_modules.definition as definition
FROM sys.views
INNER JOIN sys.objects ON
	sys.objects.object_id = sys.views.object_id and sys.objects.is_ms_shipped = 0
INNER JOIN sys.schemas ON
	sys.schemas.schema_id = sys.objects.schema_id
INNER JOIN sys.sql_modules
	ON sys.sql_modules.object_id = sys.objects.object_id
`

	rows, err := d.db.Query(sql)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ddls []string
	for rows.Next() {
		var name, definition string
		if err := rows.Scan(&name, &definition); err != nil {
			return nil, err
		}
		definition = strings.TrimSpace(definition)
		definition = strings.ReplaceAll(definition, "\n", "")
		definition = suffixSemicolon.ReplaceAllString(definition, "")
		definition = spaces.ReplaceAllString(definition, " ")
		ddls = append(ddls, definition+";")
	}
	return ddls, nil
}

func (d *MssqlDatabase) Triggers() ([]string, error) {
	query := `SELECT
	s.definition
FROM sys.triggers tr
INNER JOIN sys.all_sql_modules s ON s.object_id = tr.object_id`

	rows, err := d.db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	triggers := make([]string, 0)
	for rows.Next() {
		var definition string
		err = rows.Scan(&definition)
		if err != nil {
			return nil, err
		}
		triggers = append(triggers, definition)
	}

	return triggers, nil
}

func (d *MssqlDatabase) Types() ([]string, error) {
	return nil, nil
}

func (d *MssqlDatabase) DB() *sql.DB {
	return d.db
}

func (d *MssqlDatabase) Close() error {
	return d.db.Close()
}

func mssqlBuildDSN(config adapter.Config) string {
	query := url.Values{}
	query.Add("database", config.DbName)

	u := &url.URL{
		Scheme:   "sqlserver",
		User:     url.UserPassword(config.User, config.Password),
		Host:     fmt.Sprintf("%s:%d", config.Host, config.Port),
		RawQuery: query.Encode(),
	}
	return u.String()
}

func splitTableName(table string) (string, string) {
	schema := "dbo"
	schemaTable := strings.SplitN(table, ".", 2)
	if len(schemaTable) == 2 {
		schema = schemaTable[0]
		table = schemaTable[1]
	}
	return schema, table
}

package sql

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	_ "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/jitsucom/bulker/base/errorj"
	"github.com/jitsucom/bulker/base/logging"
	"github.com/jitsucom/bulker/base/utils"
	"github.com/jitsucom/bulker/bulker"
	"github.com/jitsucom/bulker/types"
	"os"
	"strconv"
	"strings"
	"time"
)

func init() {
	bulker.RegisterBulker(ClickHouseBulkerTypeId, NewClickHouse)
}

// TODO tmp table
const (
	ClickHouseBulkerTypeId = "clickhouse"

	chDistributedPrefix      = "dist_"
	chTableSchemaQuery       = `SELECT name, type, is_in_primary_key FROM system.columns WHERE database = ? and table = ? and default_kind not in ('MATERIALIZED', 'ALIAS', 'EPHEMERAL')`
	chCreateDatabaseTemplate = `CREATE DATABASE IF NOT EXISTS "%s" %s`

	chOnClusterClauseTemplate = ` ON CLUSTER "%s" `
	chNullableColumnTemplate  = ` Nullable(%s) `

	chCreateDistributedTableTemplate = `CREATE TABLE %s %s AS %s ENGINE = Distributed(%s,%s,%s,rand())`
	chAlterTableTemplate             = `ALTER TABLE %s %s %s`
	//chDeleteQueryTemplate            = `DELETE FROM %s %s WHERE %s`
	chDeleteQueryTemplate = `ALTER TABLE %s %s DELETE WHERE %s`

	chCreateTableTemplate   = `CREATE TABLE %s %s (%s) %s %s %s %s`
	chDropTableTemplate     = `DROP TABLE %s%s %s`
	chTruncateTableTemplate = `TRUNCATE TABLE IF EXISTS %s %s`
	chExchangeTableTemplate = `EXCHANGE TABLES %s AND %s %s`
	chRenameTableTemplate   = `RENAME TABLE %s TO %s %s`

	chSelectFinalStatement = `SELECT %s FROM %s FINAL %s%s`
	chLoadStatement        = `INSERT INTO %s (%s) VALUES (%s)`

	chDefaultPartition  = ``
	chDefaultOrderBy    = `ORDER BY (id)`
	chDefaultPrimaryKey = ``
)

var (
	SchemaToClickhouse = map[types.DataType]string{
		types.STRING:    "String",
		types.INT64:     "Int64",
		types.FLOAT64:   "Float64",
		types.TIMESTAMP: "DateTime",
		types.BOOL:      "UInt8",
		types.UNKNOWN:   "String",
	}

	defaultValues = map[string]interface{}{
		"int8":                     0,
		"int16":                    0,
		"int32":                    0,
		"int64":                    0,
		"int128":                   0,
		"int256":                   0,
		"float32":                  0.0,
		"float64":                  0.0,
		"decimal":                  0.0,
		"numeric":                  0.0,
		"datetime":                 time.Time{},
		"uint8":                    false,
		"uint16":                   0,
		"uint32":                   0,
		"uint64":                   0,
		"uint128":                  0,
		"uint256":                  0,
		"string":                   "",
		"lowcardinality(int8)":     0,
		"lowcardinality(int16)":    0,
		"lowcardinality(int32)":    0,
		"lowcardinality(int64)":    0,
		"lowcardinality(int128)":   0,
		"lowcardinality(int256)":   0,
		"lowcardinality(float32)":  0,
		"lowcardinality(float64)":  0,
		"lowcardinality(datetime)": time.Time{},
		"lowcardinality(uint8)":    false,
		"lowcardinality(uint16)":   0,
		"lowcardinality(uint32)":   0,
		"lowcardinality(uint64)":   0,
		"lowcardinality(uint128)":  0,
		"lowcardinality(uint256)":  0,
		"lowcardinality(string)":   "",
		"uuid":                     "00000000-0000-0000-0000-000000000000",
	}
)

// ClickHouseConfig dto for deserialized clickhouse config
type ClickHouseConfig struct {
	Dsns     []string          `mapstructure:"dsns,omitempty" json:"dsns,omitempty" yaml:"dsns,omitempty"`
	Database string            `mapstructure:"db,omitempty" json:"db,omitempty" yaml:"db,omitempty"`
	TLS      map[string]string `mapstructure:"tls,omitempty" json:"tls,omitempty" yaml:"tls,omitempty"`
	Cluster  string            `mapstructure:"cluster,omitempty" json:"cluster,omitempty" yaml:"cluster,omitempty"`
	Engine   *EngineConfig     `mapstructure:"engine,omitempty" json:"engine,omitempty" yaml:"engine,omitempty"`
}

// EngineConfig dto for deserialized clickhouse engine config
type EngineConfig struct {
	RawStatement    string        `mapstructure:"raw_statement,omitempty" json:"raw_statement,omitempty" yaml:"raw_statement,omitempty"`
	NullableFields  []string      `mapstructure:"nullable_fields,omitempty" json:"nullable_fields,omitempty" yaml:"nullable_fields,omitempty"`
	PartitionFields []FieldConfig `mapstructure:"partition_fields,omitempty" json:"partition_fields,omitempty" yaml:"partition_fields,omitempty"`
	OrderFields     []FieldConfig `mapstructure:"order_fields,omitempty" json:"order_fields,omitempty" yaml:"order_fields,omitempty"`
	PrimaryKeys     []string      `mapstructure:"primary_keys,omitempty" json:"primary_keys,omitempty" yaml:"primary_keys,omitempty"`
}

// FieldConfig dto for deserialized clickhouse engine fields
type FieldConfig struct {
	Function string `mapstructure:"function,omitempty" json:"function,omitempty" yaml:"function,omitempty"`
	Field    string `mapstructure:"field,omitempty" json:"field,omitempty" yaml:"field,omitempty"`
}

// ClickHouse is adapter for creating,patching (schema or table), inserting data to clickhouse
type ClickHouse struct {
	SQLAdapterBase[ClickHouseConfig]
	httpMode              bool
	tableStatementFactory *TableStatementFactory
}

// NewClickHouse returns configured ClickHouse adapter instance
func NewClickHouse(bulkerConfig bulker.Config) (bulker.Bulker, error) {
	config := &ClickHouseConfig{}
	if err := utils.ParseObject(bulkerConfig.DestinationConfig, config); err != nil {
		return nil, fmt.Errorf("failed to parse destination config: %w", err)
	}
	httpMode := false
	if strings.HasPrefix(config.Dsns[0], "http") {
		httpMode = true
	}
	dataSource, err := sql.Open("clickhouse", config.Dsns[0])
	if err != nil {
		return nil, err
	}
	//dataSource := clickhouse.OpenDB(&clickhouse.Options{
	//
	//	Addr: config.Dsns,
	//	Auth: clickhouse.Auth{
	//		Database: config.Database,
	//	},
	//	TLS: &tls.Config{
	//		InsecureSkipVerify: false,
	//	},
	//	Settings: clickhouse.Settings{
	//		"max_execution_time": 60,
	//		"wait_end_of_query":  1,
	//	},
	//	Protocol:    clickhouse.HTTP,
	//	DialTimeout: 15 * time.Second,
	//	Compression: &clickhouse.Compression{Method: clickhouse.CompressionLZ4},
	//	Debug:       true,
	//})
	//dataSource.SetMaxIdleConns(5)
	//dataSource.SetMaxOpenConns(10)
	//dataSource.SetConnMaxLifetime(time.Hour)

	//keep select 1 and don't use Ping() because chproxy doesn't support /ping endpoint.
	if _, err := dataSource.Exec("SELECT 1"); err != nil {
		dataSource.Close()
		return nil, err
	}

	tableStatementFactory, err := NewTableStatementFactory(config)
	if err != nil {
		return nil, err
	}

	tableNameFunc := func(config *ClickHouseConfig, tableName string) string {
		return fmt.Sprintf("%s", tableName)
	}
	var nullableFields []string
	if config.Engine != nil {
		nullableFields = config.Engine.NullableFields
	}
	columnDDlFunc := func(name string, column SQLColumn, pkFields utils.Set[string]) string {
		return chColumnDDL(name, column, pkFields, nullableFields)
	}
	queryLogger := logging.NewQueryLogger(bulkerConfig.Id, os.Stderr, os.Stderr)
	sqlAdapterBase := newSQLAdapterBase(ClickHouseBulkerTypeId, config, dataSource,
		queryLogger, chTypecastFunc, QuestionMarkParameterPlaceholder, tableNameFunc,
		originalColumnName, columnDDlFunc, chReformatValue, checkErr)
	sqlAdapterBase.batchFileFormat = JSON
	c := &ClickHouse{
		SQLAdapterBase:        sqlAdapterBase,
		tableStatementFactory: tableStatementFactory,
		httpMode:              httpMode,
	}

	return c, nil
}

func (ch *ClickHouse) CreateStream(id, tableName string, mode bulker.BulkMode, streamOptions ...bulker.StreamOption) (bulker.BulkerStream, error) {
	streamOptions = append(streamOptions, withLocalBatchFile(fmt.Sprintf("bulker_%s_stream_%s_%s", mode, tableName, utils.SanitizeString(id))))

	switch mode {
	case bulker.AutoCommit:
		return newAutoCommitStream(id, ch, tableName, streamOptions...)
	case bulker.Transactional:
		return newTransactionalStream(id, ch, tableName, streamOptions...)
	case bulker.ReplaceTable:
		return newReplaceTableStream(id, ch, tableName, streamOptions...)
	case bulker.ReplacePartition:
		return newReplacePartitionStream(id, ch, tableName, streamOptions...)
	}
	return nil, fmt.Errorf("unsupported bulk mode: %s", mode)
}

func (ch *ClickHouse) Type() string {
	return ClickHouseBulkerTypeId
}

func (ch *ClickHouse) GetTypesMapping() map[types.DataType]string {
	return SchemaToClickhouse
}

// OpenTx opens underline sql transaction and return wrapped instance
func (ch *ClickHouse) OpenTx(ctx context.Context) (*TxSQLAdapter, error) {
	return ch.openTx(ctx, ch)
	//return &TxSQLAdapter{sqlAdapter: ch, tx: NewDbWrapper(ch.Type(), ch.dataSource, ch.queryLogger, ch.checkErrFunc)}, nil
}

// InitDatabase create database instance if doesn't exist
func (ch *ClickHouse) InitDatabase(ctx context.Context) error {
	query := fmt.Sprintf(chCreateDatabaseTemplate, ch.config.Database, ch.getOnClusterClause())

	if _, err := ch.txOrDb(ctx).ExecContext(ctx, query); err != nil {
		return errorj.CreateSchemaError.Wrap(err, "failed to create db schema").
			WithProperty(errorj.DBInfo, &types.ErrorPayload{
				Database:  ch.config.Database,
				Cluster:   ch.config.Cluster,
				Statement: query,
			})
	}

	return nil
}

// CreateTable create database table with name,columns provided in Table representation
// New tables will have MergeTree() or ReplicatedMergeTree() engine depends on config.cluster empty or not
func (ch *ClickHouse) CreateTable(ctx context.Context, table *Table) error {
	columns := table.SortedColumnNames()
	columnsDDL := make([]string, len(columns))
	for i, columnName := range table.SortedColumnNames() {
		column := table.Columns[columnName]
		columnsDDL[i] = ch.columnDDL(columnName, column, table.PKFields)
	}

	statementStr := ch.tableStatementFactory.CreateTableStatement(table.Name, strings.Join(columnsDDL, ","))

	if _, err := ch.txOrDb(ctx).ExecContext(ctx, statementStr); err != nil {
		return errorj.CreateTableError.Wrap(err, "failed to create table").
			WithProperty(errorj.DBInfo, &types.ErrorPayload{
				Database:    ch.config.Database,
				Cluster:     ch.config.Cluster,
				Table:       table.Name,
				PrimaryKeys: table.GetPKFields(),
				Statement:   statementStr,
			})
	}

	//create distributed table if ReplicatedMergeTree engine
	if ch.config.Cluster != "" {
		return ch.createDistributedTableInTransaction(ctx, table.Name)
	}

	return nil
}

// GetTableSchema return table (name,columns with name and types) representation wrapped in Table struct
func (ch *ClickHouse) GetTableSchema(ctx context.Context, tableName string) (*Table, error) {
	table := &Table{Name: tableName, Columns: Columns{}, PKFields: utils.NewSet[string]()}
	rows, err := ch.txOrDb(ctx).QueryContext(ctx, chTableSchemaQuery, ch.config.Database, tableName)
	if err != nil {
		return nil, errorj.GetTableError.Wrap(err, "failed to get table columns").
			WithProperty(errorj.DBInfo, &types.ErrorPayload{
				Database:    ch.config.Database,
				Cluster:     ch.config.Cluster,
				Table:       tableName,
				PrimaryKeys: table.GetPKFields(),
				Statement:   chTableSchemaQuery,
				Values:      []interface{}{ch.config.Database, tableName},
			})
	}

	defer rows.Close()
	for rows.Next() {
		var columnName, columnClickhouseType string
		var isPk bool
		if err := rows.Scan(&columnName, &columnClickhouseType, &isPk); err != nil {
			return nil, errorj.GetTableError.Wrap(err, "failed to scan result").
				WithProperty(errorj.DBInfo, &types.ErrorPayload{
					Database:    ch.config.Database,
					Cluster:     ch.config.Cluster,
					Table:       tableName,
					PrimaryKeys: table.GetPKFields(),
					Statement:   chTableSchemaQuery,
					Values:      []interface{}{ch.config.Database, tableName},
				})
		}
		table.Columns[columnName] = SQLColumn{Type: columnClickhouseType}
	}
	if err := rows.Err(); err != nil {
		return nil, errorj.GetTableError.Wrap(err, "failed read last row").
			WithProperty(errorj.DBInfo, &types.ErrorPayload{
				Database:    ch.config.Database,
				Cluster:     ch.config.Cluster,
				Table:       tableName,
				PrimaryKeys: table.GetPKFields(),
				Statement:   chTableSchemaQuery,
				Values:      []interface{}{ch.config.Database, tableName},
			})
	}

	return table, nil
}

// PatchTableSchema add new columns(from provided Table) to existing table
// drop and create distributed table
func (ch *ClickHouse) PatchTableSchema(ctx context.Context, patchSchema *Table) error {
	if len(patchSchema.Columns) == 0 {
		return nil
	}
	columns := patchSchema.SortedColumnNames()
	addedColumnsDDL := make([]string, len(patchSchema.Columns))
	for i, columnName := range columns {
		column := patchSchema.Columns[columnName]
		columnDDL := ch.columnDDL(columnName, column, patchSchema.PKFields)
		addedColumnsDDL[i] = "ADD COLUMN " + columnDDL
	}

	query := fmt.Sprintf(chAlterTableTemplate, ch.fullTableName(patchSchema.Name), ch.getOnClusterClause(), strings.Join(addedColumnsDDL, ", "))

	if _, err := ch.txOrDb(ctx).ExecContext(ctx, query); err != nil {
		return errorj.PatchTableError.Wrap(err, "failed to patch table").
			WithProperty(errorj.DBInfo, &types.ErrorPayload{
				Database:    ch.config.Database,
				Cluster:     ch.config.Cluster,
				Table:       patchSchema.Name,
				PrimaryKeys: patchSchema.GetPKFields(),
				Statement:   query,
			})
	}

	if ch.config.Cluster != "" {
		query := fmt.Sprintf(chAlterTableTemplate, ch.fullDistTableName(patchSchema.Name), ch.getOnClusterClause(), strings.Join(addedColumnsDDL, ", "))

		_, err := ch.txOrDb(ctx).ExecContext(ctx, query)
		if err != nil {
			// fallback for older clickhouse versions: drop and create distributed table if ReplicatedMergeTree engine
			ch.dropTable(ctx, ch.fullDistTableName(patchSchema.Name), true)
			return ch.createDistributedTableInTransaction(ctx, patchSchema.Name)
		}

		logging.Errorf("Error altering distributed table for [%s] with statement [%s]: %v", patchSchema.Name, query, err)
	}

	return nil
}

func (ch *ClickHouse) Select(ctx context.Context, tableName string, whenConditions *WhenConditions, orderBy string) ([]map[string]any, error) {
	return ch.selectFrom(ctx, chSelectFinalStatement, tableName, "*", whenConditions, orderBy)
}

func (ch *ClickHouse) Count(ctx context.Context, tableName string, whenConditions *WhenConditions) (int, error) {
	res, err := ch.selectFrom(ctx, chSelectFinalStatement, tableName, "count(*) as jitsu_count", whenConditions, "")
	if err != nil {
		return -1, err
	}
	if len(res) == 0 {
		return -1, fmt.Errorf("select count * gave no result")
	}
	scnt := res[0]["jitsu_count"]
	return strconv.Atoi(fmt.Sprint(scnt))
}

func (ch *ClickHouse) Insert(ctx context.Context, targetTable *Table, merge bool, objects []types.Object) (err error) {
	if ch.httpMode {
		return ch.insert(ctx, targetTable, objects)
	}
	tx, err := ch.dataSource.BeginTx(ctx, nil)
	if err != nil {
		err = errorj.LoadError.Wrap(err, "failed to open transaction to load table").
			WithProperty(errorj.DBInfo, &types.ErrorPayload{
				Database:    ch.config.Database,
				Cluster:     ch.config.Cluster,
				Table:       targetTable.Name,
				PrimaryKeys: targetTable.GetPKFields(),
			})
	}

	columns := targetTable.SortedColumnNames()
	columnNames := make([]string, len(columns))
	placeHolders := make([]string, len(columns))

	for i, name := range columns {
		column := targetTable.Columns[name]
		columnNames[i] = ch.columnName(name)
		placeHolders[i] = ch.typecastFunc(ch.parameterPlaceholder(i, ch.columnName(name)), column)

	}
	copyStatement := fmt.Sprintf(chLoadStatement, ch.fullTableName(targetTable.Name), strings.Join(columnNames, ", "), strings.Join(placeHolders, ", "))
	defer func() {
		if err != nil {
			_ = tx.Rollback()
			err = errorj.ExecuteInsertError.Wrap(err, "failed to insert to table").
				WithProperty(errorj.DBInfo, &types.ErrorPayload{
					Database:    ch.config.Database,
					Cluster:     ch.config.Cluster,
					Table:       targetTable.Name,
					PrimaryKeys: targetTable.GetPKFields(),
					Statement:   copyStatement,
				})
		}
	}()

	stmt, err := tx.PrepareContext(ctx, copyStatement)
	if err != nil {
		return err
	}
	defer func() {
		_ = stmt.Close()
	}()

	for _, object := range objects {
		args := make([]any, len(columns))
		for i, v := range columns {
			l, err := convertType(object[v], targetTable.Columns[v])
			if err != nil {
				return err
			}
			//logging.Infof("%s: %v (%T) was %v", v, l, l, object[v])
			args[i] = l
		}
		if _, err := stmt.ExecContext(ctx, args...); err != nil {
			return checkErr(err)
		}
	}

	return tx.Commit()
}

// LoadTable transfer data from local file to ClickHouse table
func (ch *ClickHouse) LoadTable(ctx context.Context, targetTable *Table, loadSource *LoadSource) (err error) {
	if loadSource.Type != LocalFile {
		return fmt.Errorf("LoadTable: only local file is supported")
	}
	if loadSource.Format != ch.batchFileFormat {
		return fmt.Errorf("LoadTable: only %s format is supported", ch.batchFileFormat)
	}
	tx, err := ch.dataSource.BeginTx(ctx, nil)
	if err != nil {
		err = errorj.LoadError.Wrap(err, "failed to open transaction to load table").
			WithProperty(errorj.DBInfo, &types.ErrorPayload{
				Database:    ch.config.Database,
				Cluster:     ch.config.Cluster,
				Table:       targetTable.Name,
				PrimaryKeys: targetTable.GetPKFields(),
			})
	}

	columns := targetTable.SortedColumnNames()
	columnNames := make([]string, len(columns))
	placeHolders := make([]string, len(columns))

	for i, name := range columns {
		column := targetTable.Columns[name]
		columnNames[i] = ch.columnName(name)
		placeHolders[i] = ch.typecastFunc(ch.parameterPlaceholder(i, ch.columnName(name)), column)

	}
	copyStatement := fmt.Sprintf(chLoadStatement, ch.fullTableName(targetTable.Name), strings.Join(columnNames, ", "), strings.Join(placeHolders, ", "))
	defer func() {
		if err != nil {
			_ = tx.Rollback()
			err = errorj.LoadError.Wrap(err, "failed to load table").
				WithProperty(errorj.DBInfo, &types.ErrorPayload{
					Database:    ch.config.Database,
					Cluster:     ch.config.Cluster,
					Table:       targetTable.Name,
					PrimaryKeys: targetTable.GetPKFields(),
					Statement:   copyStatement,
				})
		}
	}()

	stmt, err := tx.PrepareContext(ctx, copyStatement)
	if err != nil {
		return err
	}
	defer func() {
		_ = stmt.Close()
	}()
	//f, err := os.ReadFile(loadSource.Path)
	//logging.Infof("FILE: %s", f)

	file, err := os.Open(loadSource.Path)
	if err != nil {
		return err
	}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		object := map[string]any{}
		decoder := json.NewDecoder(bytes.NewReader(scanner.Bytes()))
		decoder.UseNumber()
		err = decoder.Decode(&object)
		if err != nil {
			return err
		}
		args := make([]any, len(columns))
		for i, v := range columns {
			l, err := convertType(object[v], targetTable.Columns[v])
			if err != nil {
				return err
			}
			//logging.Infof("%s: %v (%T) was %v", v, l, l, object[v])
			args[i] = l
		}
		if _, err := stmt.ExecContext(ctx, args...); err != nil {
			return checkErr(err)
		}
	}

	return tx.Commit()
	//if err != nil {
	//	return err
	//}
	//_, err = ch.txOrDb(ctx).ExecContext(ctx, fmt.Sprintf("OPTIMIZE TABLE %s", ch.fullTableName(targetTable.Name)))
	//if err != nil {
	//	return err
	//}
	//return nil
}

func (ch *ClickHouse) CopyTables(ctx context.Context, targetTable *Table, sourceTable *Table, merge bool) error {
	return ch.copy(ctx, targetTable, sourceTable)
}

func (ch *ClickHouse) Delete(ctx context.Context, tableName string, deleteConditions *WhenConditions) error {
	deleteCondition, values := ToWhenConditions(deleteConditions, ch.parameterPlaceholder, 0)
	deleteQuery := fmt.Sprintf(chDeleteQueryTemplate, ch.fullTableName(tableName), ch.getOnClusterClause(), deleteCondition)

	if _, err := ch.txOrDb(ctx).ExecContext(ctx, deleteQuery, values...); err != nil {
		return errorj.DeleteFromTableError.Wrap(err, "failed to delete data").
			WithProperty(errorj.DBInfo, &types.ErrorPayload{
				Database:  ch.config.Database,
				Cluster:   ch.config.Cluster,
				Table:     tableName,
				Statement: deleteQuery,
				Values:    values,
			})
	}
	return nil
}

// TruncateTable deletes all records in tableName table
func (ch *ClickHouse) TruncateTable(ctx context.Context, tableName string) error {
	statement := fmt.Sprintf(chTruncateTableTemplate, ch.fullTableName(tableName), ch.getOnClusterClause())
	if _, err := ch.txOrDb(ctx).ExecContext(ctx, statement); err != nil {
		return errorj.TruncateError.Wrap(err, "failed to truncate table").
			WithProperty(errorj.DBInfo, &types.ErrorPayload{
				Table:     tableName,
				Statement: statement,
			})
	}

	return nil
}

func (ch *ClickHouse) DropTable(ctx context.Context, tableName string, ifExists bool) error {
	err := ch.dropTable(ctx, ch.fullTableName(tableName), ifExists)
	if err != nil {
		return err
	}
	if ch.config.Cluster != "" {
		return ch.dropTable(ctx, ch.fullDistTableName(tableName), true)
	}
	return nil
}

func (ch *ClickHouse) dropTable(ctx context.Context, fullTableName string, ifExists bool) error {
	ifExs := ""
	if ifExists {
		ifExs = "IF EXISTS "
	}
	query := fmt.Sprintf(chDropTableTemplate, ifExs, fullTableName, ch.getOnClusterClause())

	if _, err := ch.txOrDb(ctx).ExecContext(ctx, query); err != nil {

		return errorj.DropError.Wrap(err, "failed to drop table").
			WithProperty(errorj.DBInfo, &types.ErrorPayload{
				Schema:    ch.config.Database,
				Cluster:   ch.config.Cluster,
				Table:     fullTableName,
				Statement: query,
			})
	}

	return nil
}

func (ch *ClickHouse) ReplaceTable(ctx context.Context, originalTable, replacementTable string, dropOldTable bool) (err error) {
	query := fmt.Sprintf(chExchangeTableTemplate, ch.fullTableName(originalTable), ch.fullTableName(replacementTable), ch.getOnClusterClause())

	if _, err := ch.txOrDb(ctx).ExecContext(ctx, query); err != nil {
		if checkNotExistErr(err) == ErrTableNotExist {
			query = fmt.Sprintf(chRenameTableTemplate, ch.fullTableName(replacementTable), ch.fullTableName(originalTable), ch.getOnClusterClause())

			if _, err := ch.txOrDb(ctx).ExecContext(ctx, query); err != nil {
				return fmt.Errorf("error renaming [%s] table: %v", replacementTable, err)
			}
			if ch.config.Cluster != "" {
				query := fmt.Sprintf(chRenameTableTemplate, ch.fullDistTableName(replacementTable), ch.fullDistTableName(originalTable), ch.getOnClusterClause())
				if _, err := ch.txOrDb(ctx).ExecContext(ctx, query); err != nil {
					return fmt.Errorf("error renaming [%s] distributed table: %v", originalTable, err)
				}
			}
			return nil
		} else {
			return fmt.Errorf("error replacing [%s] table: %v", originalTable, err)
		}
	}

	if ch.config.Cluster != "" {
		query := fmt.Sprintf(chExchangeTableTemplate, ch.fullDistTableName(originalTable), ch.fullDistTableName(replacementTable), ch.getOnClusterClause())

		if _, err := ch.txOrDb(ctx).ExecContext(ctx, query); err != nil {
			return fmt.Errorf("error replacing [%s] distributed table: %v", originalTable, err)
		}
	}
	if dropOldTable {
		return ch.DropTable(ctx, replacementTable, true)
	} else {
		return nil
	}

}

// Close underlying sql.DB
func (ch *ClickHouse) Close() error {
	return ch.dataSource.Close()
}

// return ON CLUSTER name clause or "" if config.cluster is empty
func (ch *ClickHouse) getOnClusterClause() string {
	if ch.config.Cluster == "" {
		return ""
	}

	return fmt.Sprintf(chOnClusterClauseTemplate, ch.config.Cluster)
}

// create distributed table, ignore errors
func (ch *ClickHouse) createDistributedTableInTransaction(ctx context.Context, originTableName string) error {
	statement := fmt.Sprintf(chCreateDistributedTableTemplate,
		ch.fullDistTableName(originTableName), ch.getOnClusterClause(), ch.fullTableName(originTableName), ch.config.Cluster, ch.config.Database, originTableName)

	if _, err := ch.txOrDb(ctx).ExecContext(ctx, statement); err != nil {
		return fmt.Errorf("error creating distributed table statement with statement [%s] for [%s] : %w", statement, originTableName, err)
	}
	return nil
}

func (ch *ClickHouse) fullDistTableName(tableName string) string {
	return ch._tableNameFunc(ch.config, chDistributedPrefix+tableName)
}

func convertType(value any, column SQLColumn) (any, error) {
	v := types.ReformatValue(value)
	//logging.Infof("%v (%T) was %v (%T)", v, v, value, value)

	switch strings.ToLower(column.Type) {
	case "float64":
		switch n := v.(type) {
		case int64:
			return float64(n), nil
		case int:
			return float64(n), nil
		case string:
			f, err := strconv.ParseFloat(n, 64)
			if err != nil {
				return v, fmt.Errorf("error converting string to float64: %w", err)
			}
			return f, nil
		}
	case "int64":
		switch n := v.(type) {
		case int:
			return int64(n), nil
		case float64:
			if n == float64(int64(n)) {
				return int64(n), nil
			} else {
				return v, fmt.Errorf("error converting float to int64: %f", n)
			}
		case string:
			f, err := strconv.Atoi(n)
			if err != nil {
				return v, fmt.Errorf("error converting string to int: %w", err)
			}
			return int64(f), nil
		}
	case "bool":
		switch n := v.(type) {
		case string:
			f, err := strconv.ParseBool(n)
			if err != nil {
				return v, fmt.Errorf("error converting string to bool: %w", err)
			}
			return f, nil
		}
	case "uint8":
		switch n := v.(type) {
		case string:
			f, err := strconv.ParseBool(n)
			if err == nil {
				return f, nil
			}
		}
	case "string":
		switch n := v.(type) {
		case time.Time:
			return n.Format("2006-01-02 15:04:05Z"), nil
		case int64:
			return strconv.FormatInt(n, 10), nil
		case float64:
			return fmt.Sprint(n), nil
		case bool:
			return strconv.FormatBool(n), nil
		}
	}
	return v, nil
}

// chColumnDDL returns column DDL (column name, mapped sql type)
func chColumnDDL(name string, column SQLColumn, pkFields utils.Set[string], nullableFields []string) string {
	//get sql type
	columnSQLType := column.GetDDLType()

	//get nullable or plain
	var columnTypeDDL string
	if utils.ArrayContains(nullableFields, name) {
		columnTypeDDL = fmt.Sprintf(chNullableColumnTemplate, columnSQLType)
	} else {
		columnTypeDDL = columnSQLType
	}

	return fmt.Sprintf(`"%s" %s`, name, columnTypeDDL)
}

// chTypecastFunc returns "?" placeholder or with typecast
func chTypecastFunc(placeholder string, column SQLColumn) string {
	if column.Override {
		return fmt.Sprintf("cast(%s, '%s')", placeholder, column.Type)
	}
	return placeholder
}

// return nil if column type is nullable or default value for input type
func chGetDefaultValue(sqlType string) any {
	if !strings.Contains(strings.ToLower(sqlType), "nullable") {
		//get default value based on type
		dv, ok := defaultValues[strings.ToLower(sqlType)]
		if ok {
			return dv
		}

		logging.SystemErrorf("Unknown clickhouse default value for %s", sqlType)
	}

	return nil
}

// if value is boolean - reformat it [true = 1; false = 0] ClickHouse supports UInt8 instead of boolean
// otherwise return value as is
func chReformatValue(value any, valuePresent bool, sqlColumn SQLColumn) any {
	if !valuePresent {
		return chGetDefaultValue(sqlColumn.Type)
	}
	//reformat boolean
	booleanValue, ok := value.(bool)
	if ok {
		if booleanValue {
			return 1
		}

		return 0
	}
	return value
}

func extractStatement(fieldConfigs []FieldConfig) string {
	var parameters []string
	for _, fieldConfig := range fieldConfigs {
		if fieldConfig.Function != "" {
			parameters = append(parameters, fieldConfig.Function+"("+fieldConfig.Field+")")
			continue
		}
		parameters = append(parameters, fieldConfig.Field)
	}
	return strings.Join(parameters, ",")
}

// Validate required fields in ClickHouseConfig
func (chc *ClickHouseConfig) Validate() error {
	if chc == nil {
		return errors.New("ClickHouse config is required")
	}

	if len(chc.Dsns) == 0 {
		return errors.New("dsn is required parameter")
	}

	for _, dsn := range chc.Dsns {
		if dsn == "" {
			return errors.New("DSNs values can't be empty")
		}

		if !strings.HasPrefix(strings.TrimSpace(dsn), "http") {
			return errors.New("DSNs must have http:// or https:// prefix")
		}
	}

	if chc.Cluster == "" && len(chc.Dsns) > 1 {
		return errors.New("cluster is required parameter when dsns count > 1")
	}

	if chc.Database == "" {
		return errors.New("db is required parameter")
	}

	return nil
}

// TableStatementFactory is used for creating CREATE TABLE statements depends on config
type TableStatementFactory struct {
	engineStatement string
	database        string
	onClusterClause string

	partitionClause  string
	orderByClause    string
	primaryKeyClause string

	engineStatementFormat bool
}

func NewTableStatementFactory(config *ClickHouseConfig) (*TableStatementFactory, error) {
	if config == nil {
		return nil, errors.New("Clickhouse config can't be nil")
	}
	var onClusterClause string
	if config.Cluster != "" {
		onClusterClause = fmt.Sprintf(chOnClusterClauseTemplate, config.Cluster)
	}

	partitionClause := chDefaultPartition
	orderByClause := chDefaultOrderBy
	primaryKeyClause := chDefaultPrimaryKey
	if config.Engine != nil {
		//raw statement overrides all provided config parameters
		if config.Engine.RawStatement != "" {
			return &TableStatementFactory{
				engineStatement: config.Engine.RawStatement,
				database:        config.Database,
				onClusterClause: onClusterClause,
			}, nil
		}

		if len(config.Engine.PartitionFields) > 0 {
			partitionClause = "PARTITION BY (" + extractStatement(config.Engine.PartitionFields) + ")"
		}
		if len(config.Engine.OrderFields) > 0 {
			orderByClause = "ORDER BY (" + extractStatement(config.Engine.OrderFields) + ")"
		}
		if len(config.Engine.PrimaryKeys) > 0 {
			primaryKeyClause = "PRIMARY KEY (" + strings.Join(config.Engine.PrimaryKeys, ", ") + ")"
		}
	}

	var engineStatement string
	var engineStatementFormat bool
	if config.Cluster != "" {
		//create engine statement with ReplicatedReplacingMergeTree() engine. We need to replace %s with tableName on creating statement
		engineStatement = `ENGINE = ReplicatedReplacingMergeTree('/clickhouse/tables/{shard}/` + config.Database + `/%s', '{replica}')`
		engineStatementFormat = true
	} else {
		//create table template with ReplacingMergeTree() engine
		engineStatement = `ENGINE = ReplacingMergeTree()`
	}

	return &TableStatementFactory{
		engineStatement:       engineStatement,
		database:              config.Database,
		onClusterClause:       onClusterClause,
		partitionClause:       partitionClause,
		orderByClause:         orderByClause,
		primaryKeyClause:      primaryKeyClause,
		engineStatementFormat: engineStatementFormat,
	}, nil
}

// CreateTableStatement return clickhouse DDL for creating table statement
func (tsf TableStatementFactory) CreateTableStatement(tableName, columnsClause string) string {
	engineStatement := tsf.engineStatement
	if tsf.engineStatementFormat {
		engineStatement = fmt.Sprintf(engineStatement, tableName)
	}
	return fmt.Sprintf(chCreateTableTemplate, tableName, tsf.onClusterClause, columnsClause, engineStatement,
		tsf.partitionClause, tsf.orderByClause, tsf.primaryKeyClause)
}

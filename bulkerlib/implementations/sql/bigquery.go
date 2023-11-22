package sql

import (
	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/civil"
	"context"
	"errors"
	"fmt"
	"github.com/hashicorp/go-multierror"
	bulker "github.com/jitsucom/bulker/bulkerlib"
	"github.com/jitsucom/bulker/bulkerlib/implementations"
	types2 "github.com/jitsucom/bulker/bulkerlib/types"
	"github.com/jitsucom/bulker/jitsubase/appbase"
	"github.com/jitsucom/bulker/jitsubase/errorj"
	"github.com/jitsucom/bulker/jitsubase/logging"
	"github.com/jitsucom/bulker/jitsubase/utils"
	jsoniter "github.com/json-iterator/go"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"math"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

func init() {
	bulker.RegisterBulker(BigqueryBulkerTypeId, NewBigquery)
}

const (
	BigQueryAutocommitUnsupported = "BigQuery bulker doesn't support auto commit mode as not efficient"
	BigqueryBulkerTypeId          = "bigquery"

	bigqueryMergeTemplate  = "MERGE INTO %s T USING %s S ON %s WHEN MATCHED THEN UPDATE SET %s WHEN NOT MATCHED THEN INSERT (%s) VALUES (%s)"
	bigqueryDeleteTemplate = "DELETE FROM %s WHERE %s"
	bigqueryUpdateTemplate = "UPDATE %s SET %s WHERE %s"

	bigqueryTruncateTemplate = "TRUNCATE TABLE %s"
	bigquerySelectTemplate   = "SELECT %s FROM %s%s%s"

	bigqueryRowsLimitPerInsertOperation = 500
)

var (
	bigqueryReservedWords    = []string{"all", "and", "any", "array", "as", "asc", "assert_rows_modified", "at", "between", "by", "case", "cast", "collate", "contains", "create", "cross", "cube", "current", "default", "define", "desc", "distinct", "else", "end", "enum", "escape", "except", "exclude", "exists", "extract", "false", "fetch", "following", "for", "from", "full", "group", "grouping", "groups", "hash", "having", "if", "ignore", "in", "inner", "intersect", "interval", "into", "is", "join", "lateral", "left", "like", "limit", "lookup", "merge", "natural", "new", "no", "not", "null", "nulls", "of", "on", "or", "order", "outer", "over", "partition", "preceding", "proto", "qualify", "range", "recursive", "respect", "right", "rollup", "rows", "select", "set", "some", "struct", "tablesample", "then", "to", "treat", "true", "unbounded", "union", "unnest", "using", "when", "where", "window", "with", "within"}
	bigqueryReservedWordsSet = utils.NewSet(bigqueryReservedWords...)
	bigqueryReservedPrefixes = [...]string{"_table_", "_file_", "_partition", "_row_timestamp", "__root__", "_colidentifier"}

	bigqueryColumnUnsupportedCharacters = regexp.MustCompile(`[^0-9A-Za-z_]`)
	bigqueryColumnUndescores            = regexp.MustCompile(`_{2,}`)

	//SchemaToBigQueryString is mapping between JSON types and BigQuery types
	bigqueryTypes = map[types2.DataType][]string{
		types2.STRING:    {string(bigquery.StringFieldType)},
		types2.INT64:     {string(bigquery.IntegerFieldType)},
		types2.FLOAT64:   {string(bigquery.FloatFieldType)},
		types2.TIMESTAMP: {string(bigquery.TimestampFieldType)},
		types2.BOOL:      {string(bigquery.BooleanFieldType)},
		types2.JSON:      {string(bigquery.JSONFieldType)},
		types2.UNKNOWN:   {string(bigquery.StringFieldType)},
	}
	bigqueryTypeMapping        map[types2.DataType]string
	bigqueryReverseTypeMapping map[string]types2.DataType
)

func init() {
	bigqueryTypeMapping = make(map[types2.DataType]string, len(bigqueryTypes))
	bigqueryReverseTypeMapping = make(map[string]types2.DataType, len(bigqueryTypes)+3)
	for dataType, postgresTypes := range bigqueryTypes {
		for i, postgresType := range postgresTypes {
			if i == 0 {
				bigqueryTypeMapping[dataType] = postgresType
			}
			if dataType != types2.UNKNOWN {
				bigqueryReverseTypeMapping[postgresType] = dataType
			}
		}
	}
}

// BigQuery adapter for creating,patching (schema or table), inserting and copying data from gcs to BigQuery
type BigQuery struct {
	appbase.Service
	client      *bigquery.Client
	config      *implementations.GoogleConfig
	queryLogger *logging.QueryLogger
	tableHelper TableHelper
}

// NewBigquery return configured BigQuery bulker.Bulker instance
func NewBigquery(bulkerConfig bulker.Config) (bulker.Bulker, error) {
	config := &implementations.GoogleConfig{}
	if err := utils.ParseObject(bulkerConfig.DestinationConfig, config); err != nil {
		return nil, fmt.Errorf("failed to parse destination config: %v", err)
	}
	var err error
	err = config.Validate()
	if err != nil {
		return nil, fmt.Errorf("failed to validate config: %v", err)
	}
	var client *bigquery.Client
	ctx := context.Background()
	if config.Credentials == nil {
		client, err = bigquery.NewClient(ctx, config.Project)
	} else {
		client, err = bigquery.NewClient(ctx, config.Project, config.Credentials)
	}

	if err != nil {
		err = fmt.Errorf("error creating BigQuery client: %v", err)
	}
	var queryLogger *logging.QueryLogger
	if bulkerConfig.LogLevel == bulker.Verbose {
		queryLogger = logging.NewQueryLogger(bulkerConfig.Id, os.Stderr, os.Stderr)
	}
	b := &BigQuery{
		Service: appbase.NewServiceBase(bulkerConfig.Id),
		client:  client, config: config, queryLogger: queryLogger}
	b.tableHelper = NewTableHelper(1024, '`')
	b.tableHelper.columnNameFunc = columnNameFunc
	b.tableHelper.tableNameFunc = tableNameFunc
	return b, err
}

func (bq *BigQuery) CreateStream(id, tableName string, mode bulker.BulkMode, streamOptions ...bulker.StreamOption) (bulker.BulkerStream, error) {
	bq.validateOptions(streamOptions)
	streamOptions = append(streamOptions, withLocalBatchFile(fmt.Sprintf("bulker_%s", utils.SanitizeString(id))))
	switch mode {
	case bulker.Stream:
		return nil, errors.New(BigQueryAutocommitUnsupported)
		//return newAutoCommitStream(id, bq, tableName, streamOptions...)
	case bulker.Batch:
		return newTransactionalStream(id, bq, tableName, streamOptions...)
	case bulker.ReplaceTable:
		return newReplaceTableStream(id, bq, tableName, streamOptions...)
	case bulker.ReplacePartition:
		return newReplacePartitionStream(id, bq, tableName, streamOptions...)
	}
	return nil, fmt.Errorf("unsupported bulk mode: %s", mode)
}

func (bq *BigQuery) GetBatchFileFormat() types2.FileFormat {
	return types2.FileFormatCSV
}

func (bq *BigQuery) GetBatchFileCompression() types2.FileCompression {
	return types2.FileCompressionNONE
}

func (bq *BigQuery) validateOptions(streamOptions []bulker.StreamOption) error {
	options := &bulker.StreamOptions{}
	for _, option := range streamOptions {
		options.Add(option)
	}
	return nil
}

func (bq *BigQuery) CopyTables(ctx context.Context, targetTable *Table, sourceTable *Table, merge bool) (err error) {
	if !merge {
		defer func() {
			if err != nil {
				err = errorj.CopyError.Wrap(err, "failed to run BQ copier").
					WithProperty(errorj.DBInfo, &types2.ErrorPayload{
						Dataset: bq.config.Dataset,
						Bucket:  bq.config.Bucket,
						Project: bq.config.Project,
						Table:   targetTable.Name,
					})
			}
		}()
		dataset := bq.client.Dataset(bq.config.Dataset)
		copier := dataset.Table(targetTable.Name).CopierFrom(dataset.Table(sourceTable.Name))
		copier.WriteDisposition = bigquery.WriteAppend
		copier.CreateDisposition = bigquery.CreateIfNeeded
		bq.logQuery(fmt.Sprintf("Copy tables values to table %s from: %s ", targetTable.Name, sourceTable.Name), nil, err)
		_, err = RunJob(ctx, copier, fmt.Sprintf("copy data from '%s' to '%s'", sourceTable.Name, targetTable.Name))
		return err
	} else {
		defer func() {
			if err != nil {
				err = errorj.BulkMergeError.Wrap(err, "failed to run merge").
					WithProperty(errorj.DBInfo, &types2.ErrorPayload{
						Dataset: bq.config.Dataset,
						Bucket:  bq.config.Bucket,
						Project: bq.config.Project,
						Table:   targetTable.Name,
					})
			}
		}()

		columns := sourceTable.SortedColumnNames()
		updateSet := make([]string, len(columns))
		for i, name := range columns {
			updateSet[i] = fmt.Sprintf("T.%s = S.%s", bq.quotedColumnName(name), bq.quotedColumnName(name))
		}
		var joinConditions []string
		for pkField := range targetTable.PKFields {
			joinConditions = append(joinConditions, fmt.Sprintf("T.%s = S.%s", bq.quotedColumnName(pkField), bq.quotedColumnName(pkField)))
		}
		quotedColumns := make([]string, len(columns))
		for i, name := range columns {
			quotedColumns[i] = bq.quotedColumnName(name)
		}
		columnsString := strings.Join(quotedColumns, ",")
		insertFromSelectStatement := fmt.Sprintf(bigqueryMergeTemplate, bq.fullTableName(targetTable.Name), bq.fullTableName(sourceTable.Name),
			strings.Join(joinConditions, " AND "), strings.Join(updateSet, ", "), columnsString, columnsString)

		query := bq.client.Query(insertFromSelectStatement)
		_, err = RunJob(ctx, query, fmt.Sprintf("copy data from '%s' to '%s'", sourceTable.Name, targetTable.Name))
		return err
	}
}

func (bq *BigQuery) Ping(ctx context.Context) error {
	if bq.client == nil {
		ctx := context.Background()
		var err error
		if bq.config.Credentials == nil {
			bq.client, err = bigquery.NewClient(ctx, bq.config.Project)
		} else {
			bq.client, err = bigquery.NewClient(ctx, bq.config.Project, bq.config.Credentials)
		}
		return err
	}
	_, err := bq.client.Query("SELECT 1;").Read(context.Background())
	if err != nil {
		if bq.config.Credentials == nil {
			bq.client, err = bigquery.NewClient(ctx, bq.config.Project)
		} else {
			bq.client, err = bigquery.NewClient(ctx, bq.config.Project, bq.config.Credentials)
		}
	}
	return err
}

// GetTableSchema return google BigQuery table (name,columns) representation wrapped in Table struct
func (bq *BigQuery) GetTableSchema(ctx context.Context, tableName string) (*Table, error) {
	tableName = bq.TableName(tableName)
	table := &Table{Name: tableName, Columns: Columns{}, PKFields: utils.Set[string]{}}

	bqTable := bq.client.Dataset(bq.config.Dataset).Table(tableName)

	meta, err := bqTable.Metadata(ctx)
	if err != nil {
		if isNotFoundErr(err) {
			return table, nil
		}

		return nil, errorj.GetTableError.Wrap(err, "failed to get table").
			WithProperty(errorj.DBInfo, &types2.ErrorPayload{
				Dataset: bq.config.Dataset,
				Bucket:  bq.config.Bucket,
				Project: bq.config.Project,
				Table:   tableName,
			})
	}
	for _, field := range meta.Schema {
		dt, _ := bq.GetDataType(string(field.Type))
		table.Columns[field.Name] = types2.SQLColumn{Type: string(field.Type), DataType: dt}
	}
	for k, v := range meta.Labels {
		if strings.HasPrefix(k, BulkerManagedPkConstraintPrefix) {
			pkFields := strings.Split(v, "---")
			for _, pkField := range pkFields {
				table.PKFields.Put(pkField)
			}
			table.PrimaryKeyName = k
			break
		}
	}

	return table, nil
}

// CreateTable creates google BigQuery table from Table
func (bq *BigQuery) CreateTable(ctx context.Context, table *Table) (err error) {
	tableName := bq.TableName(table.Name)
	bqTable := bq.client.Dataset(bq.config.Dataset).Table(tableName)

	_, err = bqTable.Metadata(ctx)
	if err == nil {
		bq.Infof("BigQuery table %s already exists", tableName)
		return nil
	}

	if !isNotFoundErr(err) {
		return errorj.GetTableError.Wrap(err, "failed to get table metadata").
			WithProperty(errorj.DBInfo, &types2.ErrorPayload{
				Dataset: bq.config.Dataset,
				Bucket:  bq.config.Bucket,
				Project: bq.config.Project,
				Table:   tableName,
			})
	}

	bqSchema := bigquery.Schema{}
	for _, columnName := range table.SortedColumnNames() {
		column := table.Columns[columnName]
		bigQueryType := bigquery.FieldType(strings.ToUpper(column.GetDDLType()))
		bqSchema = append(bqSchema, &bigquery.FieldSchema{Name: bq.ColumnName(columnName), Type: bigQueryType})
	}
	var labels map[string]string
	if len(table.PKFields) > 0 && table.PrimaryKeyName != "" {
		labels = map[string]string{table.PrimaryKeyName: strings.Join(table.GetPKFields(), "---")}
	}
	tableMetaData := bigquery.TableMetadata{Name: tableName, Schema: bqSchema, Labels: labels}
	if table.Partition.Field == "" && table.TimestampColumn != "" {
		// partition by timestamp column
		table = table.Clone()
		table.Partition.Field = table.TimestampColumn
		table.Partition.Granularity = MONTH
	}
	if table.Partition.Field != "" && table.Partition.Granularity != ALL {
		var partitioningType bigquery.TimePartitioningType
		switch table.Partition.Granularity {
		case DAY, WEEK:
			partitioningType = bigquery.DayPartitioningType
		case MONTH, QUARTER:
			partitioningType = bigquery.MonthPartitioningType
		case YEAR:
			partitioningType = bigquery.YearPartitioningType
		}
		tableMetaData.TimePartitioning = &bigquery.TimePartitioning{Field: table.Partition.Field, Type: partitioningType}
	}
	if table.Temporary {
		tableMetaData.ExpirationTime = time.Now().Add(time.Hour)
	}
	bq.logQuery("CREATE table for schema: ", tableMetaData, nil)
	if err := bqTable.Create(ctx, &tableMetaData); err != nil {
		schemaJson, _ := bqSchema.ToJSONFields()
		return errorj.GetTableError.Wrap(err, "failed to create table").
			WithProperty(errorj.DBInfo, &types2.ErrorPayload{
				Dataset:   bq.config.Dataset,
				Bucket:    bq.config.Bucket,
				Project:   bq.config.Project,
				Table:     tableName,
				Statement: string(schemaJson),
			})
	}

	return nil
}

// InitDatabase creates google BigQuery Dataset if doesn't exist
func (bq *BigQuery) InitDatabase(ctx context.Context) error {
	dataset := bq.config.Dataset
	bqDataset := bq.client.Dataset(dataset)
	if _, err := bqDataset.Metadata(ctx); err != nil {
		if isNotFoundErr(err) {
			datasetMetadata := &bigquery.DatasetMetadata{Name: dataset}
			bq.logQuery("CREATE dataset: ", datasetMetadata, nil)
			if err := bqDataset.Create(ctx, datasetMetadata); err != nil {
				return errorj.CreateSchemaError.Wrap(err, "failed to create dataset").
					WithProperty(errorj.DBInfo, &types2.ErrorPayload{
						Dataset: dataset,
					})
			}
		} else {
			return errorj.CreateSchemaError.Wrap(err, "failed to get dataset metadata").
				WithProperty(errorj.DBInfo, &types2.ErrorPayload{
					Dataset: dataset,
				})
		}
	}

	return nil
}

// PatchTableSchema adds Table columns to google BigQuery table
func (bq *BigQuery) PatchTableSchema(ctx context.Context, patchSchema *Table) error {
	bqTable := bq.client.Dataset(bq.config.Dataset).Table(patchSchema.Name)
	metadata, err := bqTable.Metadata(ctx)
	if err != nil {
		return errorj.PatchTableError.Wrap(err, "failed to get table metadata").
			WithProperty(errorj.DBInfo, &types2.ErrorPayload{
				Dataset: bq.config.Dataset,
				Bucket:  bq.config.Bucket,
				Project: bq.config.Project,
				Table:   patchSchema.Name,
			})
	}
	//patch primary keys - delete old
	if patchSchema.DeletePkFields {
		metadata.Labels = map[string]string{}
	}

	//patch primary keys - create new
	if len(patchSchema.PKFields) > 0 && patchSchema.PrimaryKeyName != "" {
		metadata.Labels = map[string]string{patchSchema.PrimaryKeyName: strings.Join(patchSchema.GetPKFields(), ",")}
	}
	for _, columnName := range patchSchema.SortedColumnNames() {
		column := patchSchema.Columns[columnName]
		bigQueryType := bigquery.FieldType(strings.ToUpper(column.GetDDLType()))
		metadata.Schema = append(metadata.Schema, &bigquery.FieldSchema{Name: bq.ColumnName(columnName), Type: bigQueryType})
	}
	updateReq := bigquery.TableMetadataToUpdate{Schema: metadata.Schema}
	bq.logQuery("PATCH update request: ", updateReq, nil)
	if _, err := bqTable.Update(ctx, updateReq, metadata.ETag); err != nil {
		schemaJson, _ := metadata.Schema.ToJSONFields()
		return errorj.PatchTableError.Wrap(err, "failed to patch table").
			WithProperty(errorj.DBInfo, &types2.ErrorPayload{
				Dataset:   bq.config.Dataset,
				Bucket:    bq.config.Bucket,
				Project:   bq.config.Project,
				Table:     patchSchema.Name,
				Statement: string(schemaJson),
			})
	}

	return nil
}

func (bq *BigQuery) DeletePartition(ctx context.Context, tableName string, datePartiton *DatePartition) error {
	tableName = bq.TableName(tableName)
	partitions := GranularityToPartitionIds(datePartiton.Granularity, datePartiton.Value)
	for _, partition := range partitions {
		bq.logQuery("DELETE partition "+partition+" in table"+tableName, "", nil)
		bq.Infof("Deletion partition %s in table %s", partition, tableName)
		if err := bq.client.Dataset(bq.config.Dataset).Table(tableName + "$" + partition).Delete(ctx); err != nil {
			gerr, ok := err.(*googleapi.Error)
			if ok && gerr.Code == 404 {
				bq.Infof("Partition %s$%s was not found", tableName, partition)
				continue
			}
			return errorj.DeleteFromTableError.Wrap(err, "failed to delete partition").
				WithProperty(errorj.DBInfo, &types2.ErrorPayload{
					Dataset:   bq.config.Dataset,
					Bucket:    bq.config.Bucket,
					Project:   bq.config.Project,
					Table:     tableName,
					Partition: partition,
				})
		}

	}
	return nil
}

func GranularityToPartitionIds(g Granularity, t time.Time) []string {
	t = g.Lower(t)
	switch g {
	case HOUR:
		return []string{t.Format("2006010215")}
	case DAY:
		return []string{t.Format("20060102")}
	case WEEK:
		week := make([]string, 0, 7)
		for i := 0; i < 7; i++ {
			week = append(week, t.AddDate(0, 0, i).Format("20060102"))
		}
		return week
	case MONTH:
		return []string{t.Format("200601")}
	case QUARTER:
		quarter := make([]string, 0, 3)
		for i := 0; i < 3; i++ {
			quarter = append(quarter, t.AddDate(0, i, 0).Format("200601"))
		}
		return quarter
	case YEAR:
		return []string{t.Format("2006")}
	default:
		logging.SystemErrorf("Granularity %s is not mapped to any partition time unit:.", g)
		return []string{}
	}
}

func (bq *BigQuery) Insert(ctx context.Context, table *Table, merge bool, objects ...types2.Object) (err error) {
	inserter := bq.client.Dataset(bq.config.Dataset).Table(table.Name).Inserter()
	bq.logQuery(fmt.Sprintf("Inserting [%d] values to table %s using BigQuery Streaming API with chunks [%d]: ", len(objects), table.Name, bigqueryRowsLimitPerInsertOperation), objects, nil)

	items := make([]*BQItem, 0, bigqueryRowsLimitPerInsertOperation)
	operation := 0
	operations := int(math.Max(1, float64(len(objects))/float64(bigqueryRowsLimitPerInsertOperation)))
	for _, object := range objects {
		if len(items) > bigqueryRowsLimitPerInsertOperation {
			operation++
			if err := bq.insertItems(ctx, inserter, items); err != nil {
				return errorj.ExecuteInsertInBatchError.Wrap(err, "failed to execute middle insert %d of %d in batch", operation, operations).
					WithProperty(errorj.DBInfo, &types2.ErrorPayload{
						Dataset: bq.config.Dataset,
						Bucket:  bq.config.Bucket,
						Project: bq.config.Project,
						Table:   table.Name,
					})
			}

			items = make([]*BQItem, 0, bigqueryRowsLimitPerInsertOperation)
		}

		items = append(items, &BQItem{values: object})
	}

	if len(items) > 0 {
		operation++
		if err := bq.insertItems(ctx, inserter, items); err != nil {
			return errorj.ExecuteInsertInBatchError.Wrap(err, "failed to execute last insert %d of %d in batch", operation, operations).
				WithProperty(errorj.DBInfo, &types2.ErrorPayload{
					Dataset: bq.config.Dataset,
					Bucket:  bq.config.Bucket,
					Project: bq.config.Project,
					Table:   table.Name,
				})
		}
	}

	return nil
}

func (bq *BigQuery) LoadTable(ctx context.Context, targetTable *Table, loadSource *LoadSource) (err error) {
	tableName := bq.TableName(targetTable.Name)
	defer func() {
		if err != nil {
			err = errorj.ExecuteInsertInBatchError.Wrap(err, "failed to execute middle insert batch").
				WithProperty(errorj.DBInfo, &types2.ErrorPayload{
					Dataset: bq.config.Dataset,
					Bucket:  bq.config.Bucket,
					Project: bq.config.Project,
					Table:   tableName,
				})
		}
	}()
	bq.logQuery(fmt.Sprintf("Loading values to table %s from: %s ", targetTable.Name, loadSource.Path), nil, nil)
	//f, err := os.ReadFile(loadSource.Path)
	//bq.Infof("FILE: %s", f)

	file, err := os.Open(loadSource.Path)
	if err != nil {
		return err
	}
	bqTable := bq.client.Dataset(bq.config.Dataset).Table(tableName)
	meta, err := bqTable.Metadata(ctx)

	//sort meta schema field order to match csv file column order
	mp := make(map[string]*bigquery.FieldSchema, len(meta.Schema))
	for _, field := range meta.Schema {
		mp[field.Name] = field
	}
	for i, field := range targetTable.SortedColumnNames() {
		if _, ok := mp[field]; !ok {
			return fmt.Errorf("field %s is not in table schema", field)
		}
		meta.Schema[i] = mp[field]
	}

	source := bigquery.NewReaderSource(file)
	source.Schema = meta.Schema

	if loadSource.Format == types2.FileFormatCSV {
		source.SourceFormat = bigquery.CSV
		source.SkipLeadingRows = 1
		source.CSVOptions.NullMarker = "\\N"
	} else if loadSource.Format == types2.FileFormatNDJSON {
		source.SourceFormat = bigquery.JSON
	}
	loader := bq.client.Dataset(bq.config.Dataset).Table(tableName).LoaderFrom(source)
	loader.CreateDisposition = bigquery.CreateIfNeeded
	loader.WriteDisposition = bigquery.WriteAppend
	_, err = RunJob(ctx, loader, fmt.Sprintf("load into table '%s'", tableName))
	return err
}

// DropTable drops table from BigQuery
func (bq *BigQuery) DropTable(ctx context.Context, tableName string, ifExists bool) error {
	tableName = bq.TableName(tableName)
	bq.logQuery(fmt.Sprintf("DROP table '%s' if exists: %t", tableName, ifExists), nil, nil)

	bqTable := bq.client.Dataset(bq.config.Dataset).Table(tableName)
	_, err := bqTable.Metadata(ctx)
	gerr, ok := err.(*googleapi.Error)
	if ok && gerr.Code == 404 && ifExists {
		return nil
	}
	if err := bqTable.Delete(ctx); err != nil {
		return errorj.DropError.Wrap(err, "failed to drop table").
			WithProperty(errorj.DBInfo, &types2.ErrorPayload{
				Dataset: bq.config.Dataset,
				Bucket:  bq.config.Bucket,
				Project: bq.config.Project,
				Table:   tableName,
			})
	}

	return nil
}

func (bq *BigQuery) Drop(ctx context.Context, table *Table, ifExists bool) error {
	return bq.DropTable(ctx, table.Name, ifExists)
}

func (bq *BigQuery) ReplaceTable(ctx context.Context, targetTableName string, replacementTable *Table, dropOldTable bool) (err error) {
	targetTableName = bq.TableName(targetTableName)
	replacementTableName := bq.TableName(replacementTable.Name)
	defer func() {
		if err != nil {
			err = errorj.CopyError.Wrap(err, "failed to replace table").
				WithProperty(errorj.DBInfo, &types2.ErrorPayload{
					Dataset: bq.config.Dataset,
					Bucket:  bq.config.Bucket,
					Project: bq.config.Project,
					Table:   targetTableName,
				})
		}
	}()
	dataset := bq.client.Dataset(bq.config.Dataset)
	copier := dataset.Table(targetTableName).CopierFrom(dataset.Table(replacementTableName))
	copier.WriteDisposition = bigquery.WriteTruncate
	copier.CreateDisposition = bigquery.CreateIfNeeded
	bq.logQuery(fmt.Sprintf("COPY table '%s' to '%s'", replacementTableName, targetTableName), copier, nil)

	_, err = RunJob(ctx, copier, fmt.Sprintf("replace table '%s' with '%s'", targetTableName, replacementTableName))
	if err != nil {
		return err
	}
	if dropOldTable {
		return bq.DropTable(ctx, replacementTableName, false)
	} else {
		return nil
	}
}

// TruncateTable deletes all records in tableName table
func (bq *BigQuery) TruncateTable(ctx context.Context, tableName string) error {
	tableName = bq.TableName(tableName)
	query := fmt.Sprintf(bigqueryTruncateTemplate, bq.fullTableName(tableName))
	bq.logQuery(query, nil, nil)
	if _, err := bq.client.Query(query).Read(ctx); err != nil {
		extraText := ""
		if strings.Contains(err.Error(), "Not found") {
			extraText = ": " + ErrTableNotExist.Error()
		}
		return errorj.TruncateError.Wrap(err, "failed to truncate table"+extraText).
			WithProperty(errorj.DBInfo, &types2.ErrorPayload{
				Dataset: bq.config.Dataset,
				Bucket:  bq.config.Bucket,
				Project: bq.config.Project,
				Table:   tableName,
			})
	}
	//TODO: temporary workaround for "404: Table is truncated" error until #958 is done
	time.Sleep(time.Minute)
	return nil
}

func (bq *BigQuery) insertItems(ctx context.Context, inserter *bigquery.Inserter, items []*BQItem) error {
	if err := inserter.Put(ctx, items); err != nil {
		var multiErr error
		if putMultiError, ok := err.(bigquery.PutMultiError); ok {
			for _, errUnit := range putMultiError {
				multiErr = multierror.Append(multiErr, errors.New(errUnit.Error()))
			}
		} else {
			multiErr = err
		}

		return multiErr
	}

	return nil
}

func (bq *BigQuery) toDeleteQuery(conditions *WhenConditions) string {
	var queryConditions []string

	for _, condition := range conditions.Conditions {
		conditionString := fmt.Sprintf("%v %v %q", condition.Field, condition.Clause, condition.Value)
		queryConditions = append(queryConditions, conditionString)
	}

	return strings.Join(queryConditions, " "+conditions.JoinCondition+" ")
}

func (bq *BigQuery) logQuery(messageTemplate string, entity any, err error) {
	if bq.queryLogger != nil {
		entityString := ""
		if entity != nil {
			entityJSON, err := jsoniter.Marshal(entity)
			if err != nil {
				entityString = fmt.Sprintf("[failed to marshal query entity: %v]", err)
				bq.Warnf("Failed to serialize entity for logging: %s", fmt.Sprint(entity))
			} else {
				entityString = string(entityJSON)
			}
		}
		bq.queryLogger.LogQuery(messageTemplate+entityString, err)
	}
}

func (bq *BigQuery) Close() error {
	if bq.client != nil {
		return bq.client.Close()
	}
	return nil
}

// Return true if google err is 404
func isNotFoundErr(err error) bool {
	e, ok := err.(*googleapi.Error)
	return ok && e.Code == http.StatusNotFound
}

// BQItem struct for streaming inserts to BigQuery
type BQItem struct {
	values map[string]any
}

func (bqi *BQItem) Save() (row map[string]bigquery.Value, insertID string, err error) {
	row = map[string]bigquery.Value{}

	for k, v := range bqi.values {
		row[k] = v
	}

	return
}

func (bq *BigQuery) Update(ctx context.Context, tableName string, object types2.Object, whenConditions *WhenConditions) (err error) {
	tableName = bq.TableName(tableName)
	updateCondition, updateValues := bq.toWhenConditions(whenConditions)

	columns := make([]string, len(object), len(object))
	values := make([]bigquery.QueryParameter, len(object)+len(updateValues), len(object)+len(updateValues))
	i := 0
	for name, value := range object {
		columns[i] = bq.quotedColumnName(name) + "= @" + name
		values[i] = bigquery.QueryParameter{Name: name, Value: value}
		i++
	}
	for a := 0; a < len(updateValues); a++ {
		values[i+a] = updateValues[a]
	}
	updateQuery := fmt.Sprintf(bigqueryUpdateTemplate, bq.fullTableName(tableName), strings.Join(columns, ", "), updateCondition)
	defer func() {
		v := make([]any, len(values))
		for i, value := range values {
			v[i] = value.Value
		}
		if err != nil {
			err = errorj.UpdateError.Wrap(err, "failed execute update").
				WithProperty(errorj.DBInfo, &types2.ErrorPayload{
					Dataset:   bq.config.Dataset,
					Table:     tableName,
					Statement: updateQuery,
					Values:    v,
				})
		}
	}()

	query := bq.client.Query(updateQuery)
	query.Parameters = values
	_, err = RunJob(ctx, query, fmt.Sprintf("update table '%s'", tableName))
	return err
}

func (bq *BigQuery) Select(ctx context.Context, tableName string, whenConditions *WhenConditions, orderBy []string) ([]map[string]any, error) {
	return bq.selectFrom(ctx, tableName, "*", whenConditions, orderBy)
}
func (bq *BigQuery) selectFrom(ctx context.Context, tableName string, selectExpression string, whenConditions *WhenConditions, orderBy []string) (res []map[string]any, err error) {
	tableName = bq.TableName(tableName)
	whenCondition, values := bq.toWhenConditions(whenConditions)
	if whenCondition != "" {
		whenCondition = " WHERE " + whenCondition
	}
	orderByClause := ""
	if len(orderBy) > 0 {
		quotedOrderByColumns := make([]string, len(orderBy))
		for i, col := range orderBy {
			quotedOrderByColumns[i] = fmt.Sprintf("%s asc", bq.quotedColumnName(col))
		}
		orderByClause = " ORDER BY " + strings.Join(quotedOrderByColumns, ", ")
	}
	selectQuery := fmt.Sprintf(bigquerySelectTemplate, selectExpression, bq.fullTableName(tableName), whenCondition, orderByClause)
	bq.logQuery(selectQuery, nil, nil)

	defer func() {
		v := make([]any, len(values))
		for i, value := range values {
			v[i] = value.Value
		}
		if err != nil {
			err = errorj.SelectFromTableError.Wrap(err, "failed execute select").
				WithProperty(errorj.DBInfo, &types2.ErrorPayload{
					Dataset:   bq.config.Dataset,
					Table:     tableName,
					Statement: selectQuery,
					Values:    v,
				})
		}
	}()

	query := bq.client.Query(selectQuery)
	query.Parameters = values
	job, err := RunJob(ctx, query, fmt.Sprintf("select from table '%s'", tableName))
	if err != nil {
		return nil, err
	}
	it, err := job.Read(ctx)
	if err != nil {
		return nil, err
	}
	var result []map[string]any
	for {
		var row = map[string]bigquery.Value{}
		err := it.Next(&row)
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, err
		}
		var resRow = map[string]any{}
		for k, v := range row {
			switch i := v.(type) {
			case civil.Date:
				v = i.In(time.UTC)
			case int64:
				v = int(i)
			}
			resRow[k] = v
		}
		result = append(result, resRow)
	}
	return result, nil
}

func (bq *BigQuery) Count(ctx context.Context, tableName string, whenConditions *WhenConditions) (int, error) {
	res, err := bq.selectFrom(ctx, tableName, "count(*) as jitsu_count", whenConditions, nil)
	if err != nil {
		return -1, err
	}
	if len(res) == 0 {
		return -1, fmt.Errorf("select count * gave no result")
	}
	return strconv.Atoi(fmt.Sprint(res[0]["jitsu_count"]))
}

func (bq *BigQuery) toWhenConditions(conditions *WhenConditions) (string, []bigquery.QueryParameter) {
	if conditions == nil {
		return "", []bigquery.QueryParameter{}
	}
	var queryConditions []string
	var values []bigquery.QueryParameter

	for _, condition := range conditions.Conditions {
		switch strings.ToLower(condition.Clause) {
		case "is null":
		case "is not null":
			queryConditions = append(queryConditions, bq.quotedColumnName(condition.Field)+" "+condition.Clause)
		default:
			queryConditions = append(queryConditions, bq.quotedColumnName(condition.Field)+" "+condition.Clause+" @when_"+condition.Field)
			values = append(values, bigquery.QueryParameter{Name: "when_" + condition.Field, Value: types2.ReformatValue(condition.Value)})
		}
	}

	return strings.Join(queryConditions, " "+conditions.JoinCondition+" "), values
}
func (bq *BigQuery) Delete(ctx context.Context, tableName string, deleteConditions *WhenConditions) (err error) {
	tableName = bq.TableName(tableName)
	whenCondition, values := bq.toWhenConditions(deleteConditions)
	if len(whenCondition) == 0 {
		return errors.New("delete conditions are empty")
	}
	deleteQuery := fmt.Sprintf(bigqueryDeleteTemplate, bq.fullTableName(tableName), whenCondition)
	defer func() {
		v := make([]any, len(values))
		for i, value := range values {
			v[i] = value.Value
		}
		if err != nil {
			err = errorj.DeleteFromTableError.Wrap(err, "failed execute delete").
				WithProperty(errorj.DBInfo, &types2.ErrorPayload{
					Dataset:   bq.config.Dataset,
					Table:     tableName,
					Statement: deleteQuery,
					Values:    v,
				})
		}
	}()
	query := bq.client.Query(deleteQuery)
	query.Parameters = values
	_, err = RunJob(ctx, query, fmt.Sprintf("delete from table '%s'", tableName))
	return err
}
func (bq *BigQuery) Type() string {
	return BigqueryBulkerTypeId
}

func (bq *BigQuery) OpenTx(ctx context.Context) (*TxSQLAdapter, error) {
	return &TxSQLAdapter{sqlAdapter: bq, tx: NewDummyTxWrapper(bq.Type())}, nil
}

func (bq *BigQuery) fullTableName(tableName string) string {
	return bq.config.Project + "." + bq.config.Dataset + "." + bq.tableHelper.quotedTableName(tableName)
}

func tableNameFunc(identifier string) (adapted string, needQuote bool) {
	return identifier, !sqlUnquotedIdentifierPattern.MatchString(identifier)
}

func columnNameFunc(identifier string) (adapted string, needQuote bool) {
	cleanIdentifier := bigqueryColumnUnsupportedCharacters.ReplaceAllString(identifier, "")
	cleanIdentifier = bigqueryColumnUndescores.ReplaceAllString(cleanIdentifier, "_")
	if cleanIdentifier == "" || cleanIdentifier == "_" {
		return fmt.Sprintf("column_%x", utils.HashString(identifier)), false
	}
	identifier = strings.ReplaceAll(identifier, " ", "_")
	result := bigqueryColumnUnsupportedCharacters.ReplaceAllString(identifier, "")
	if utils.IsNumber(int32(result[0])) {
		result = "_" + result
	}
	lc := strings.ToLower(result)
	if bigqueryReservedWordsSet.Contains(lc) {
		return result, true
	}
	for _, reserved := range bigqueryReservedPrefixes {
		if strings.HasPrefix(lc, reserved) {
			return utils.ShortenString("_"+result, 300), false
		}
	}
	return utils.ShortenString(result, 300), false
}

func (bq *BigQuery) TableName(identifier string) string {
	return bq.tableHelper.TableName(identifier)
}

func (bq *BigQuery) ColumnName(identifier string) string {
	return bq.tableHelper.ColumnName(identifier)
}

func (bq *BigQuery) quotedColumnName(identifier string) string {
	return bq.tableHelper.quotedColumnName(identifier)
}

func (bq *BigQuery) GetSQLType(dataType types2.DataType) (string, bool) {
	v, ok := bigqueryTypeMapping[dataType]
	return v, ok
}

func (bq *BigQuery) GetDataType(sqlType string) (types2.DataType, bool) {
	v, ok := bigqueryReverseTypeMapping[sqlType]
	return v, ok
}

func (bq *BigQuery) TableHelper() *TableHelper {
	return &bq.tableHelper
}

type JobRunner interface {
	Run(ctx context.Context) (*bigquery.Job, error)
}

func RunJob(ctx context.Context, runner JobRunner, jobDescription string) (job *bigquery.Job, err error) {
	var status *bigquery.JobStatus
	job, err = runner.Run(ctx)
	if err == nil {
		status, err = job.Wait(ctx)
		if err == nil {
			err = status.Err()
			if err == nil {
				return job, nil
			}
		}
	}

	if err != nil {
		builder := strings.Builder{}
		builder.WriteString(fmt.Sprintf("Failed to %s. Job ID: %s Completed with error: %s", jobDescription, job.ID(), err.Error()))
		if status != nil && len(status.Errors) > 0 {
			builder.WriteString("\nDetailed errors:")
			for _, statusError := range status.Errors {
				builder.WriteString(fmt.Sprintf("\n%s", statusError.Error()))
			}
		}
		return job, errors.New(builder.String())
	} else {
		return job, nil
	}
}

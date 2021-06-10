package bun

import (
	"context"
	"database/sql"
	"sort"
	"strconv"

	"github.com/uptrace/bun/dialect/feature"
	"github.com/uptrace/bun/dialect/sqltype"
	"github.com/uptrace/bun/internal"
	"github.com/uptrace/bun/schema"
)

type CreateTableQuery struct {
	baseQuery

	temp        bool
	ifNotExists bool
	varchar     int

	fks         []schema.QueryWithArgs
	partitionBy schema.QueryWithArgs
	tablespace  schema.QueryWithArgs
}

func NewCreateTableQuery(db *DB) *CreateTableQuery {
	q := &CreateTableQuery{
		baseQuery: baseQuery{
			db:  db,
			dbi: db.DB,
		},
	}
	return q
}

func (q *CreateTableQuery) Conn(db DBI) *CreateTableQuery {
	q.setDBI(db)
	return q
}

func (q *CreateTableQuery) Model(model interface{}) *CreateTableQuery {
	q.setTableModel(model)
	return q
}

//------------------------------------------------------------------------------

func (q *CreateTableQuery) Table(tables ...string) *CreateTableQuery {
	for _, table := range tables {
		q.addTable(schema.UnsafeIdent(table))
	}
	return q
}

func (q *CreateTableQuery) TableExpr(query string, args ...interface{}) *CreateTableQuery {
	q.addTable(schema.SafeQuery(query, args))
	return q
}

func (q *CreateTableQuery) ModelTableExpr(query string, args ...interface{}) *CreateTableQuery {
	q.modelTable = schema.SafeQuery(query, args)
	return q
}

//------------------------------------------------------------------------------

func (q *CreateTableQuery) Temp() *CreateTableQuery {
	q.temp = true
	return q
}

func (q *CreateTableQuery) IfNotExists() *CreateTableQuery {
	q.ifNotExists = true
	return q
}

func (q *CreateTableQuery) Varchar(n int) *CreateTableQuery {
	q.varchar = n
	return q
}

func (q *CreateTableQuery) ForeignKey(query string, args ...interface{}) *CreateTableQuery {
	q.fks = append(q.fks, schema.SafeQuery(query, args))
	return q
}

func (q *CreateTableQuery) AppendQuery(fmter schema.Formatter, b []byte) (_ []byte, err error) {
	if q.err != nil {
		return nil, q.err
	}
	if q.table == nil {
		return nil, errNilModel
	}

	b = append(b, "CREATE "...)
	if q.temp {
		b = append(b, "TEMP "...)
	}
	b = append(b, "TABLE "...)
	if q.ifNotExists {
		b = append(b, "IF NOT EXISTS "...)
	}
	b, err = q.appendFirstTable(fmter, b)
	if err != nil {
		return nil, err
	}

	b = append(b, " ("...)

	for i, field := range q.table.Fields {
		if i > 0 {
			b = append(b, ", "...)
		}

		b = append(b, field.SQLName...)
		b = append(b, " "...)
		b = q.appendSQLType(b, field)
		if field.NotNull {
			b = append(b, " NOT NULL"...)
		}
		if q.db.features.Has(feature.AutoIncrement) && field.AutoIncrement {
			b = append(b, " AUTO_INCREMENT"...)
		}
		if field.SQLDefault != "" {
			b = append(b, " DEFAULT "...)
			b = append(b, field.SQLDefault...)
		}
	}

	b = q.appendPKConstraint(b, q.table.PKs)
	b = q.appendUniqueConstraints(fmter, b)
	b, err = q.appenFKConstraints(fmter, b)
	if err != nil {
		return nil, err
	}

	b = append(b, ")"...)

	if !q.partitionBy.IsZero() {
		b = append(b, " PARTITION BY "...)
		b, err = q.partitionBy.AppendQuery(fmter, b)
		if err != nil {
			return nil, err
		}
	}

	if !q.tablespace.IsZero() {
		b = append(b, " TABLESPACE "...)
		b, err = q.tablespace.AppendQuery(fmter, b)
		if err != nil {
			return nil, err
		}
	}

	return b, nil
}

func (q *CreateTableQuery) appendSQLType(b []byte, field *schema.Field) []byte {
	if field.CreateTableSQLType != field.DiscoveredSQLType {
		return append(b, field.CreateTableSQLType...)
	}

	if q.varchar > 0 &&
		field.CreateTableSQLType == sqltype.VarChar {
		b = append(b, "varchar("...)
		b = strconv.AppendInt(b, int64(q.varchar), 10)
		b = append(b, ")"...)
		return b
	}

	return append(b, field.CreateTableSQLType...)
}

func (q *CreateTableQuery) appendUniqueConstraints(fmter schema.Formatter, b []byte) []byte {
	unique := q.table.Unique

	keys := make([]string, 0, len(unique))
	for key := range unique {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for _, key := range keys {
		b = q.appendUniqueConstraint(fmter, b, key, unique[key])
	}

	return b
}

func (q *CreateTableQuery) appendUniqueConstraint(
	fmter schema.Formatter, b []byte, name string, fields []*schema.Field,
) []byte {
	if name != "" {
		b = append(b, ", CONSTRAINT "...)
		b = fmter.AppendIdent(b, name)
	} else {
		b = append(b, ","...)
	}
	b = append(b, " UNIQUE ("...)
	b = appendColumns(b, "", fields)
	b = append(b, ")"...)

	return b
}

func (q *CreateTableQuery) appenFKConstraints(
	fmter schema.Formatter, b []byte,
) (_ []byte, err error) {
	for _, fk := range q.fks {
		b = append(b, ", FOREIGN KEY "...)
		b, err = fk.AppendQuery(fmter, b)
		if err != nil {
			return nil, err
		}
	}
	return b, nil
}

func (q *CreateTableQuery) appendPKConstraint(b []byte, pks []*schema.Field) []byte {
	if len(pks) == 0 {
		return b
	}

	b = append(b, ", PRIMARY KEY ("...)
	b = appendColumns(b, "", pks)
	b = append(b, ")"...)
	return b
}

//------------------------------------------------------------------------------

func (q *CreateTableQuery) Exec(ctx context.Context, dest ...interface{}) (res sql.Result, _ error) {
	if err := q.beforeCreateTableHook(ctx); err != nil {
		return res, err
	}

	bs := getByteSlice()
	defer putByteSlice(bs)

	queryBytes, err := q.AppendQuery(q.db.fmter, bs.b)
	if err != nil {
		return res, err
	}

	bs.update(queryBytes)
	query := internal.String(queryBytes)

	res, err = q.exec(ctx, q, query)
	if err != nil {
		return res, err
	}

	if q.table != nil {
		if err := q.afterCreateTableHook(ctx); err != nil {
			return res, err
		}
	}

	return res, nil
}

func (q *CreateTableQuery) beforeCreateTableHook(ctx context.Context) error {
	if hook, ok := q.table.ZeroIface.(BeforeCreateTableHook); ok {
		if err := hook.BeforeCreateTable(ctx, q); err != nil {
			return err
		}
	}
	return nil
}

func (q *CreateTableQuery) afterCreateTableHook(ctx context.Context) error {
	if hook, ok := q.table.ZeroIface.(AfterCreateTableHook); ok {
		if err := hook.AfterCreateTable(ctx, q); err != nil {
			return err
		}
	}
	return nil
}

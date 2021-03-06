package sqlf

import (
	"database/sql"
	"errors"
	"fmt"
	"reflect"

	"github.com/jmoiron/sqlx"
	"github.com/jmoiron/sqlx/reflectx"
)

// InsertRowCommand contains all the information required to insert
// a single row into a database table based on the contents of a Go struct..
type InsertRowCommand interface {
	// Command returns the SQL insert statement with placeholders for arguments..
	Command() string

	// Args returns an array of arguments for the SQL command
	// based on the contents of the row.
	Args(row interface{}) ([]interface{}, error)

	// Exec executes the SQL insert statement with the arguments
	// appropriate for the contents of the row. If the row has
	// an auto-increment column, it will be populated with the value
	// generated by the database server.
	Exec(db sqlx.Execer, row interface{}) error
}

// UpdateRowCommand contains all the information required to update
// or delete a single row in a database table based on the contents of a
// associated Go struct.
type UpdateRowCommand interface {
	// Command returns the SQL update/delete statement with placeholders for arguments..
	Command() string

	// Args returns an array of arguments for the SQL update/delete command
	// based on the contents of the row.
	Args(row interface{}) ([]interface{}, error)

	// Exec executes the SQL update/delete statement with the arguments
	// appropriate for the contents of the row. Returns the number
	// of rows updated, which should be zero or one. The contents of the
	// row struct are unchanged.
	Exec(db sqlx.Execer, row interface{}) (rowCount int, err error)
}

// ExecCommand contains all the information required to perform an
// operation against the database that does not return rows. (For example,
// insert, update or delete). Commands accept an arbitrary number of
// arguments, each of which should be a scalar value.
type ExecCommand interface {
	// Command returns the SQL statement with placeholders for arguments..
	Command() string

	// Exec executes the SQL statement with the arguments given.
	Exec(db sqlx.Execer, args ...interface{}) (sql.Result, error)
}

// QueryCommand contains all the information required to perform an
// operation against the database that returns rows. (ie select statements).
// Commands accept an arbitrary number of arguments, each of which should be
// a scalar value.
type QueryCommand interface {
	// Command returns the SQL select statement with placeholders for arguments..
	Command() string

	// Query executes the query with the arguments given.
	Query(db sqlx.Queryer, args ...interface{}) (*sqlx.Rows, error)

	// QueryRow executes the query, which is expected to return at most one row.
	// QueryRow always returns a non-nil value. Errors are deferred until the Scan
	// method is called on the Row.
	QueryRow(db sqlx.Queryer, args ...interface{}) *sqlx.Row

	// Select executes a query using the provided Queryer, and StructScans each
	// row into dest, which must be a slice. If the slice elements are scannable,
	// then the result set must have only one column. Otherwise StructScan is
	// used. The *sql.Rows are closed automatically.
	Select(db sqlx.Queryer, dest interface{}, args ...interface{}) error
}

// cloneArgs takes a deep copy of all arguments so that they can be
// modified before preparing the SQL statement.
func cloneArgs(args []interface{}) []interface{} {
	args2 := make([]interface{}, len(args))
	tableClones := map[*TableInfo]*TableInfo{}
	tableClone := func(ti *TableInfo) *TableInfo {
		ti2 := tableClones[ti]
		if ti2 == nil {
			ti2 = ti.clone()
			tableClones[ti] = ti2
		}
		return ti2
	}

	for i, arg := range args {
		if tn, ok := arg.(TableName); ok {
			args2[i] = tn.clone(tableClone(tn.table))
		} else if cil, ok := arg.(ColumnList); ok {
			args2[i] = cil.clone(tableClone(cil.table))
		} else if ph, ok := arg.(*Placeholder); ok {
			args2[i] = ph.clone(tableClone(ph.table))
		} else {
			args2[i] = arg
		}
	}
	return args2
}

type execRowCommand struct {
	command string
	table   *TableInfo
	inputs  []*columnInfo
}

func (cmd execRowCommand) Command() string {
	return cmd.command
}

func (cmd execRowCommand) Args(row interface{}) ([]interface{}, error) {
	if cmd.table == nil {
		return nil, errors.New("table not specified")
	}
	var args []interface{}

	rowVal := reflect.ValueOf(row)
	for rowVal.Type().Kind() == reflect.Ptr {
		rowVal = rowVal.Elem()
	}
	if rowVal.Type() != cmd.table.rowType {
		return nil, fmt.Errorf("Args: expected type %s.%s or pointer", cmd.table.rowType.PkgPath(), cmd.table.rowType.Name())
	}

	for _, ci := range cmd.inputs {
		args = append(args, reflectx.FieldByIndexesReadOnly(rowVal, ci.fields).Interface())
	}

	return args, nil
}

func (cmd execRowCommand) doExec(db sqlx.Execer, row interface{}) (sql.Result, error) {
	args, err := cmd.Args(row)
	if err != nil {
		return nil, err
	}
	return db.Exec(cmd.Command(), args...)
}

func (cmd execRowCommand) getRowValue(row interface{}) (reflect.Value, error) {
	rowVal := reflect.ValueOf(row)
	for rowVal.Type().Kind() == reflect.Ptr {
		rowVal = rowVal.Elem()
	}
	if rowVal.Type() != cmd.table.rowType {
		return reflect.Value{}, fmt.Errorf("Args: expected type %s.%s or pointer", cmd.table.rowType.PkgPath(), cmd.table.rowType.Name())
	}
	return rowVal, nil
}

// insertRowCommand handles inserting a single table at a time.
type insertRowCommand struct {
	execRowCommand
}

func (cmd insertRowCommand) Exec(db sqlx.Execer, row interface{}) error {
	// find the auto-increment column, if any
	var autoInc *columnInfo
	for _, ci := range cmd.table.columns {
		if ci.autoIncrement {
			autoInc = ci
			break
		}
	}

	// field for setting the auto-increment value
	var field reflect.Value
	if autoInc != nil {
		// Some DBs allow the auto-increment column to be specified.
		// Work out if this statment is doing this.
		autoIncInserted := false
		for _, ci := range cmd.inputs {
			if ci == autoInc {
				// this statement is setting the auto-increment column explicitly
				autoIncInserted = true
				break
			}
		}

		if !autoIncInserted {
			rowVal := reflect.ValueOf(row)
			field = reflectx.FieldByIndexes(rowVal, autoInc.fields)
			if !field.CanSet() {
				return fmt.Errorf("Cannot set auto-increment value for type %s", rowVal.Type().Name())
			}
		}
	}

	result, err := cmd.doExec(db, row)
	if err != nil {
		return err
	}

	if field.IsValid() {
		n, err := result.LastInsertId()
		if err != nil {
			return nil
		}
		// TODO: could catch a panic here if the type is not int8, 1nt16, int32, int64
		field.SetInt(n)
	}
	return nil
}

// InsertRowf builds up a command for inserting a single row in the database
// using a familiar "printf" style syntax.
//
// TODO: need an example
func InsertRowf(format string, args ...interface{}) InsertRowCommand {
	// take a clone of the args so that we can modify them
	args = cloneArgs(args)
	cmd := insertRowCommand{}

	for _, arg := range args {
		if tn, ok := arg.(TableName); ok {
			if tn.clause == clauseInsertInto {
				cmd.table = tn.table
			}
		}
		if cil, ok := arg.(ColumnList); ok {
			if cil.clause.isInput() {
				// input parameters for the INSERT statement
				cmd.inputs = append(cmd.inputs, cil.filtered()...)
			}
		}
	}

	// apply placeholders to each of the input parameters
	for i, ci := range cmd.inputs {
		ci.setPosition(i + 1)
	}

	// generate the SQL statement
	cmd.command = fmt.Sprintf(format, args...)

	return cmd
}

// updateRowCommand handles inserting a single table at a time.
type updateRowCommand struct {
	execRowCommand
}

func (cmd updateRowCommand) Exec(db sqlx.Execer, row interface{}) (rowsUpdated int, err error) {
	result, err := cmd.doExec(db, row)
	if err != nil {
		return 0, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

// UpdateRowf builds a command to update a single row in the database
// using a familiar "printf"-style syntax.
//
// TODO: example needed.
func UpdateRowf(format string, args ...interface{}) UpdateRowCommand {
	// take a clone of the args so that we can modify them
	args = cloneArgs(args)
	cmd := updateRowCommand{}

	for _, arg := range args {
		if tn, ok := arg.(TableName); ok {
			if tn.clause == clauseUpdateTable {
				cmd.table = tn.table
			}
		}
		if cil, ok := arg.(ColumnList); ok {
			if cil.clause.isInput() {
				// input parameters for the UPDATE statement
				cmd.inputs = append(cmd.inputs, cil.filtered()...)
			}
		}
	}

	// apply placeholders to each of the input parameters
	for i, ci := range cmd.inputs {
		ci.setPosition(i + 1)
	}

	// generate the SQL statement
	cmd.command = fmt.Sprintf(format, args...)

	return cmd
}

type execCommand struct {
	command string
}

func (cmd execCommand) Command() string {
	return cmd.command
}

func (cmd execCommand) Exec(db sqlx.Execer, args ...interface{}) (sql.Result, error) {
	return db.Exec(cmd.Command(), args...)
}

// Execf formats an SQL command that does not return any rows.
func Execf(format string, args ...interface{}) ExecCommand {
	args = cloneArgs(args)
	cmd := execCommand{}
	var inputs []interface {
		setPosition(n int)
	}

	for _, arg := range args {
		if cil, ok := arg.(ColumnList); ok {
			if cil.clause.isInput() {
				for _, ci := range cil.filtered() {
					inputs = append(inputs, ci)
				}
			}
		} else if ph, ok := arg.(*Placeholder); ok {
			inputs = append(inputs, ph)
		}
	}

	// apply placeholders to each of the input parameters
	for i, input := range inputs {
		input.setPosition(i + 1)
	}

	// generate the SQL statement
	cmd.command = fmt.Sprintf(format, args...)

	return cmd
}

// updateRowCommand handles inserting a single table at a time.
type queryCommand struct {
	command string
	columns []*columnInfo
	inputs  []*columnInfo
	mapper  *reflectx.Mapper
}

func (cmd *queryCommand) getMapper() (*reflectx.Mapper, error) {
	m := make(map[string]*columnInfo)
	if cmd.mapper == nil {
		for _, ci := range cmd.columns {
			if _, ok := m[ci.fieldName]; ok {
				// TODO: need to modify the github.com/jmoiron/sqlx/reflectx package
				// to solve this problem.
				return nil, fmt.Errorf("Cannot process query: multiple fields named %s", ci.fieldName)
			}
			m[ci.fieldName] = ci
		}
		mapFunc := func(name string) string {
			if ci, ok := m[name]; ok {
				if ci.hasColumnAlias() {
					return ci.columnAlias()
				}
				return ci.columnName
			}
			return name
		}
		cmd.mapper = reflectx.NewMapperFunc("", mapFunc)
	}
	return cmd.mapper, nil

}

func (cmd *queryCommand) Command() string {
	return cmd.command
}

func (cmd *queryCommand) Query(db sqlx.Queryer, args ...interface{}) (*sqlx.Rows, error) {
	mapper, err := cmd.getMapper()
	if err != nil {
		return nil, err
	}

	rows, err := db.Query(cmd.Command(), args...)
	if err != nil {
		return nil, err
	}
	return &sqlx.Rows{
		Rows:   rows,
		Mapper: mapper,
	}, nil

}

func (cmd *queryCommand) QueryRow(db sqlx.Queryer, args ...interface{}) *sqlx.Row {
	mapper, err := cmd.getMapper()
	if err != nil {
		// TODO
		panic(err.Error())
	}
	row := db.QueryRowx(cmd.Command(), args...)
	row.Mapper = mapper
	return row
}

func (cmd *queryCommand) Select(db sqlx.Queryer, dest interface{}, args ...interface{}) error {
	q := queryer{
		cmd: cmd,
		db:  db,
	}
	return sqlx.Select(q, dest, "unused", args...)
}

// queryer implements the sqlx.Queryer interface. In all methods, the
// query string is ignored and the actual query is taken from the query command.
type queryer struct {
	cmd *queryCommand
	db  sqlx.Queryer
}

func (q queryer) Query(query string, args ...interface{}) (*sql.Rows, error) {
	return q.db.Query(q.cmd.Command(), args...)
}

func (q queryer) Queryx(query string, args ...interface{}) (*sqlx.Rows, error) {
	return q.cmd.Query(q.db, args...)
}

func (q queryer) QueryRowx(query string, args ...interface{}) *sqlx.Row {
	return q.cmd.QueryRow(q.db, args)
}

// Queryf builds a command to query one or more rows from the database
// using a familiar "printf"-style syntax.
//
// TODO: example needed.
func Queryf(format string, args ...interface{}) QueryCommand {
	// take a clone of the args so that we can modify them
	args = cloneArgs(args)
	cmd := queryCommand{}

	for _, arg := range args {
		if cil, ok := arg.(ColumnList); ok {
			if cil.clause.isInput() {
				// input parameters for the SELECT statement
				cmd.inputs = append(cmd.inputs, cil.filtered()...)
			}
			if cil.clause == clauseSelectColumns {
				cmd.columns = append(cmd.columns, cil.filtered()...)
			}
		}
	}

	// apply placeholders to each of the input parameters
	for i, ci := range cmd.inputs {
		ci.setPosition(i + 1)
	}

	// generate the SQL statement
	cmd.command = fmt.Sprintf(format, args...)

	return &cmd
}

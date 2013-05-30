// Copyright 2012 James Cooper. All rights reserved.
// Use of this source code is governed by a MIT-style
// license that can be found in the LICENSE file.

// Package gorp provides a simple way to marshal Go structs to and from
// SQL databases.  It uses the database/sql package, and should work with any
// compliant database/sql driver.
//
// Source code and project home:
// https://github.com/coopernurse/gorp
//
package gorp

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"github.com/jmoiron/sqlx"
	"reflect"
	"strings"
)

var zeroVal reflect.Value
var versFieldConst = "[gorp_ver_field]"

// OptimisticLockError is returned by Update() or Delete() if the
// struct being modified has a Version field and the value is not equal to
// the current value in the database
type OptimisticLockError struct {
	// Table name where the lock error occurred
	TableName string

	// Primary key values of the row being updated/deleted
	Keys []interface{}

	// true if a row was found with those keys, indicating the
	// LocalVersion is stale.  false if no value was found with those
	// keys, suggesting the row has been deleted since loaded, or
	// was never inserted to begin with
	RowExists bool

	// Version value on the struct passed to Update/Delete. This value is
	// out of sync with the database.
	LocalVersion int64
}

// Error returns a description of the cause of the lock error
func (e OptimisticLockError) Error() string {
	if e.RowExists {
		return fmt.Sprintf("gorp: OptimisticLockError table=%s keys=%v out of date version=%d", e.TableName, e.Keys, e.LocalVersion)
	}

	return fmt.Sprintf("gorp: OptimisticLockError no row found for table=%s keys=%v", e.TableName, e.Keys)
}

// CustomScanner binds a database column value to a Go type
type CustomScanner struct {
	// After a row is scanned, Holder will contain the value from the database column.
	// Initialize the CustomScanner with the concrete Go type you wish the database
	// driver to scan the raw column into.
	Holder interface{}
	// Target typically holds a pointer to the target struct field to bind the Holder
	// value to.
	Target interface{}
	// Binder is a custom function that converts the holder value to the target type
	// and sets target accordingly.  This function should return error if a problem
	// occurs converting the holder to the target.
	Binder func(holder interface{}, target interface{}) error
}

// Bind is called automatically by gorp after Scan()
func (me CustomScanner) Bind() error {
	return me.Binder(me.Holder, me.Target)
}

// TableMap represents a mapping between a Go struct and a database table
// Use dbmap.AddTable() or dbmap.AddTableWithName() to create these
type TableMap struct {
	// Name of database table.
	TableName  string
	gotype     reflect.Type
	columns    []*ColumnMap
	keys       []*ColumnMap
	version    *ColumnMap
	insertPlan bindPlan
	updatePlan bindPlan
	deletePlan bindPlan
	getPlan    bindPlan
	dbmap      *DbMap
	// Cached capabilities for the struct mapped to this table
	CanPreInsert  bool
	CanPostInsert bool
	CanPostGet    bool
	CanPreUpdate  bool
	CanPostUpdate bool
	CanPreDelete  bool
	CanPostDelete bool
}

// ResetSql removes cached insert/update/select/delete SQL strings
// associated with this TableMap.  Call this if you've modified
// any column names or the table name itself.
func (t *TableMap) ResetSql() {
	t.insertPlan = bindPlan{}
	t.updatePlan = bindPlan{}
	t.deletePlan = bindPlan{}
	t.getPlan = bindPlan{}
}

// SetKeys lets you specify the fields on a struct that map to primary
// key columns on the table.  If isAutoIncr is set, result.LastInsertId()
// will be used after INSERT to bind the generated id to the Go struct.
//
// Automatically calls ResetSql() to ensure SQL statements are regenerated.
func (t *TableMap) SetKeys(isAutoIncr bool, fieldNames ...string) *TableMap {
	t.keys = make([]*ColumnMap, 0)
	for _, name := range fieldNames {
		colmap := t.ColMap(strings.ToLower(name))
		colmap.isPK = true
		colmap.isAutoIncr = isAutoIncr
		t.keys = append(t.keys, colmap)
	}
	t.ResetSql()

	return t
}

// ColMap returns the ColumnMap pointer matching the given struct field
// name.  It panics if the struct does not contain a field matching this
// name.
func (t *TableMap) ColMap(field string) *ColumnMap {
	col := colMapOrNil(t, field)
	if col == nil {
		e := fmt.Sprintf("No ColumnMap in table %s type %s with field %s",
			t.TableName, t.gotype.Name(), field)

		panic(e)
	}
	return col
}

func colMapOrNil(t *TableMap, field string) *ColumnMap {
	for _, col := range t.columns {
		if col.fieldName == field || col.ColumnName == field {
			return col
		}
	}
	return nil
}

// SetVersionCol sets the column to use as the Version field.  By default
// the "Version" field is used.  Returns the column found, or panics
// if the struct does not contain a field matching this name.
//
// Automatically calls ResetSql() to ensure SQL statements are regenerated.
func (t *TableMap) SetVersionCol(field string) *ColumnMap {
	c := t.ColMap(field)
	t.version = c
	t.ResetSql()
	return c
}

// A bindPlan saves a query type (insert, get, updated, delete) so it doesn't
// have to be re-created every time it's executed.
type bindPlan struct {
	query       string
	argFields   []string
	keyFields   []string
	versField   string
	autoIncrIdx int
}

func (plan bindPlan) createBindInstance(elem reflect.Value) bindInstance {
	bi := bindInstance{query: plan.query, autoIncrIdx: plan.autoIncrIdx, versField: plan.versField}
	if plan.versField != "" {
		bi.existingVersion = elem.FieldByName(plan.versField).Int()
	}

	for i := 0; i < len(plan.argFields); i++ {
		k := plan.argFields[i]
		if k == versFieldConst {
			newVer := bi.existingVersion + 1
			bi.args = append(bi.args, newVer)
			if bi.existingVersion == 0 {
				elem.FieldByName(plan.versField).SetInt(int64(newVer))
			}
		} else {
			val := elem.FieldByName(k).Interface()
			bi.args = append(bi.args, val)
		}
	}

	for i := 0; i < len(plan.keyFields); i++ {
		k := plan.keyFields[i]
		val := elem.FieldByName(k).Interface()
		bi.keys = append(bi.keys, val)
	}

	return bi
}

type bindInstance struct {
	query           string
	args            []interface{}
	keys            []interface{}
	existingVersion int64
	versField       string
	autoIncrIdx     int
}

func (t *TableMap) bindInsert(elem reflect.Value) bindInstance {
	plan := t.insertPlan
	if plan.query == "" {
		plan.autoIncrIdx = -1

		s := bytes.Buffer{}
		s2 := bytes.Buffer{}
		s.WriteString(fmt.Sprintf("insert into %s (", t.dbmap.Dialect.QuoteField(t.TableName)))

		x := 0
		first := true
		for y := range t.columns {
			col := t.columns[y]

			if !col.Transient {
				if !first {
					s.WriteString(",")
					s2.WriteString(",")
				}
				s.WriteString(t.dbmap.Dialect.QuoteField(col.ColumnName))

				if col.isAutoIncr {
					s2.WriteString(t.dbmap.Dialect.AutoIncrBindValue())
					plan.autoIncrIdx = y
				} else {
					s2.WriteString(t.dbmap.Dialect.BindVar(x))
					if col == t.version {
						plan.versField = col.fieldName
						plan.argFields = append(plan.argFields, versFieldConst)
					} else {
						plan.argFields = append(plan.argFields, col.fieldName)
					}

					x++
				}

				first = false
			}
		}
		s.WriteString(") values (")
		s.WriteString(s2.String())
		s.WriteString(")")
		if plan.autoIncrIdx > -1 {
			s.WriteString(t.dbmap.Dialect.AutoIncrInsertSuffix(t.columns[plan.autoIncrIdx]))
		}
		s.WriteString(";")

		plan.query = s.String()
		t.insertPlan = plan
	}

	return plan.createBindInstance(elem)
}

func (t *TableMap) bindUpdate(elem reflect.Value) bindInstance {
	plan := t.updatePlan
	if plan.query == "" {

		s := bytes.Buffer{}
		s.WriteString(fmt.Sprintf("update %s set ", t.dbmap.Dialect.QuoteField(t.TableName)))
		x := 0

		for y := range t.columns {
			col := t.columns[y]
			if !col.isPK && !col.Transient {
				if x > 0 {
					s.WriteString(", ")
				}
				s.WriteString(t.dbmap.Dialect.QuoteField(col.ColumnName))
				s.WriteString("=")
				s.WriteString(t.dbmap.Dialect.BindVar(x))

				if col == t.version {
					plan.versField = col.fieldName
					plan.argFields = append(plan.argFields, versFieldConst)
				} else {
					plan.argFields = append(plan.argFields, col.fieldName)
				}
				x++
			}
		}

		s.WriteString(" where ")
		for y := range t.keys {
			col := t.keys[y]
			if y > 0 {
				s.WriteString(" and ")
			}
			s.WriteString(t.dbmap.Dialect.QuoteField(col.ColumnName))
			s.WriteString("=")
			s.WriteString(t.dbmap.Dialect.BindVar(x))

			plan.argFields = append(plan.argFields, col.fieldName)
			plan.keyFields = append(plan.keyFields, col.fieldName)
			x++
		}
		if plan.versField != "" {
			s.WriteString(" and ")
			s.WriteString(t.dbmap.Dialect.QuoteField(t.version.ColumnName))
			s.WriteString("=")
			s.WriteString(t.dbmap.Dialect.BindVar(x))
			plan.argFields = append(plan.argFields, plan.versField)
		}
		s.WriteString(";")

		plan.query = s.String()
		t.updatePlan = plan
	}

	return plan.createBindInstance(elem)
}

func (t *TableMap) bindDelete(elem reflect.Value) bindInstance {
	plan := t.deletePlan
	if plan.query == "" {

		s := bytes.Buffer{}
		s.WriteString(fmt.Sprintf("delete from %s", t.dbmap.Dialect.QuoteField(t.TableName)))

		for y := range t.columns {
			col := t.columns[y]
			if !col.Transient {
				if col == t.version {
					plan.versField = col.fieldName
				}
			}
		}

		s.WriteString(" where ")
		for x := range t.keys {
			k := t.keys[x]
			if x > 0 {
				s.WriteString(" and ")
			}
			s.WriteString(t.dbmap.Dialect.QuoteField(k.ColumnName))
			s.WriteString("=")
			s.WriteString(t.dbmap.Dialect.BindVar(x))

			plan.keyFields = append(plan.keyFields, k.fieldName)
			plan.argFields = append(plan.argFields, k.fieldName)
		}
		if plan.versField != "" {
			s.WriteString(" and ")
			s.WriteString(t.dbmap.Dialect.QuoteField(t.version.ColumnName))
			s.WriteString("=")
			s.WriteString(t.dbmap.Dialect.BindVar(len(plan.argFields)))

			plan.argFields = append(plan.argFields, plan.versField)
		}
		s.WriteString(";")

		plan.query = s.String()
		t.deletePlan = plan
	}

	return plan.createBindInstance(elem)
}

func (t *TableMap) bindGet() bindPlan {
	plan := t.getPlan
	if plan.query == "" {

		s := bytes.Buffer{}
		s.WriteString("select ")

		x := 0
		for _, col := range t.columns {
			if !col.Transient {
				if x > 0 {
					s.WriteString(",")
				}
				s.WriteString(t.dbmap.Dialect.QuoteField(col.ColumnName))
				plan.argFields = append(plan.argFields, col.fieldName)
				x++
			}
		}
		s.WriteString(" from ")
		s.WriteString(t.TableName)
		s.WriteString(" where ")
		for x := range t.keys {
			col := t.keys[x]
			if x > 0 {
				s.WriteString(" and ")
			}
			s.WriteString(t.dbmap.Dialect.QuoteField(col.ColumnName))
			s.WriteString("=")
			s.WriteString(t.dbmap.Dialect.BindVar(x))

			plan.keyFields = append(plan.keyFields, col.fieldName)
		}
		s.WriteString(";")

		plan.query = s.String()
		t.getPlan = plan
	}

	return plan
}

// ColumnMap represents a mapping between a Go struct field and a single
// column in a table.
// Unique and MaxSize only inform the
// CreateTables() function and are not used by Insert/Update/Delete/Get.
type ColumnMap struct {
	// Column name in db table
	ColumnName string

	// If true, this column is skipped in generated SQL statements
	Transient bool

	// If true, " unique" is added to create table statements.
	// Not used elsewhere
	Unique bool

	// Passed to Dialect.ToSqlType() to assist in informing the
	// correct column type to map to in CreateTables()
	// Not used elsewhere
	MaxSize int

	fieldName  string
	gotype     reflect.Type
	isPK       bool
	isAutoIncr bool
}

// This mapping should be known ahead of time, and this is the one case where
// I think I want things to actually be done in the struct tags instead of
// being changed at runtime where other systems then do not have access to them

// Rename allows you to specify the column name in the table
//
// Example:  table.ColMap("Updated").Rename("date_updated")
//
//func (c *ColumnMap) Rename(colname string) *ColumnMap {
//	c.ColumnName = colname
//	return c
//}

// SetTransient allows you to mark the column as transient. If true
// this column will be skipped when SQL statements are generated
func (c *ColumnMap) SetTransient(b bool) *ColumnMap {
	c.Transient = b
	return c
}

// If true " unique" will be added to create table statements for this
// column
func (c *ColumnMap) SetUnique(b bool) *ColumnMap {
	c.Unique = b
	return c
}

// SetMaxSize specifies the max length of values of this column. This is
// passed to the dialect.ToSqlType() function, which can use the value
// to alter the generated type for "create table" statements
func (c *ColumnMap) SetMaxSize(size int) *ColumnMap {
	c.MaxSize = size
	return c
}

// Transaction represents a database transaction.
// Insert/Update/Delete/Get/Exec operations will be run in the context
// of that transaction.  Transactions should be terminated with
// a call to Commit() or Rollback()
type Transaction struct {
	dbmap *DbMap
	tx    *sqlx.Tx
}

// SqlExecutor exposes gorp operations that can be run from Pre/Post
// hooks.  This hides whether the current operation that triggered the
// hook is in a transaction.
//
// See the DbMap function docs for each of the functions below for more
// information.
type SqlExecutor interface {
	Get(dest interface{}, keys ...interface{}) error
	Insert(list ...interface{}) error
	Update(list ...interface{}) (int64, error)
	Delete(list ...interface{}) (int64, error)
	Exec(query string, args ...interface{}) (sql.Result, error)
	Select(i interface{}, query string, args ...interface{}) ([]interface{}, error)
	query(query string, args ...interface{}) (*sql.Rows, error)
	queryRow(query string, args ...interface{}) *sql.Row
	queryRowx(query string, args ...interface{}) *sqlx.Row
}

// Insert runs a SQL INSERT statement for each element in list.  List
// items must be pointers.
//
// Any interface whose TableMap has an auto-increment primary key will
// have its last insert id bound to the PK field on the struct.
//
// Hook functions PreInsert() and/or PostInsert() will be executed
// before/after the INSERT statement if the interface defines them.
//
// Panics if any interface in the list has not been registered with AddTable
func (m *DbMap) Insert(list ...interface{}) error {
	return insert(m, m, list...)
}

// Update runs a SQL UPDATE statement for each element in list.  List
// items must be pointers.
//
// Hook functions PreUpdate() and/or PostUpdate() will be executed
// before/after the UPDATE statement if the interface defines them.
//
// Returns number of rows updated
//
// Returns an error if SetKeys has not been called on the TableMap
// Panics if any interface in the list has not been registered with AddTable
func (m *DbMap) Update(list ...interface{}) (int64, error) {
	return update(m, m, list...)
}

// Delete runs a SQL DELETE statement for each element in list.  List
// items must be pointers.
//
// Hook functions PreDelete() and/or PostDelete() will be executed
// before/after the DELETE statement if the interface defines them.
//
// Returns number of rows deleted
//
// Returns an error if SetKeys has not been called on the TableMap
// Panics if any interface in the list has not been registered with AddTable
func (m *DbMap) Delete(list ...interface{}) (int64, error) {
	return delete(m, m, list...)
}

// Get runs a SQL SELECT to fetch a single row from the table based on the
// primary key(s)
//
//  i should be an empty value for the struct to load
//  keys should be the primary key value(s) for the row to load.  If
//  multiple keys exist on the table, the order should match the column
//  order specified in SetKeys() when the table mapping was defined.
//
// Hook function PostGet() will be executed
// after the SELECT statement if the interface defines them.
//
// Returns a pointer to a struct that matches or nil if no row is found
//
// Returns an error if SetKeys has not been called on the TableMap
// Panics if any interface in the list has not been registered with AddTable
func (m *DbMap) Get(dest interface{}, keys ...interface{}) error {
	return get(m, m, dest, keys...)
}

// Select runs an arbitrary SQL query, binding the columns in the result
// to fields on the struct specified by i.  args represent the bind
// parameters for the SQL statement.
//
// Column names on the SELECT statement should be aliased to the field names
// on the struct i. Returns an error if one or more columns in the result
// do not match.  It is OK if fields on i are not part of the SQL
// statement.
//
// Hook function PostGet() will be executed
// after the SELECT statement if the interface defines them.
//
// Values are returned in one of two ways:
// 1. If i is a struct or a pointer to a struct, returns a slice of pointers to
//    matching rows of type i.
// 2. If i is a pointer to a slice, the results will be appended to that slice
//    and nil returned.
//
// i does NOT need to be registered with AddTable()
func (m *DbMap) Select(i interface{}, query string, args ...interface{}) ([]interface{}, error) {
	return hookedselect(m, m, i, query, args...)
}

// FIXME: this comment is wrong, query preparation is commented out

// Exec runs an arbitrary SQL statement.  args represent the bind parameters.
// This is equivalent to running:  Prepare(), Exec() using database/sql
func (m *DbMap) Exec(query string, args ...interface{}) (sql.Result, error) {
	m.trace(query, args)
	//stmt, err := m.Db.Prepare(query)
	//if err != nil {
	//	return nil, err
	//}
	//fmt.Println("Exec", query, args)
	return m.Db.Exec(query, args...)
}

// Begin starts a gorp Transaction
func (m *DbMap) Begin() (*Transaction, error) {
	tx, err := m.Db.Begin()
	if err != nil {
		return nil, err
	}
	return &Transaction{m, &sqlx.Tx{*tx}}, nil
}

func (m *DbMap) tableFor(t reflect.Type, checkPK bool) (*TableMap, error) {
	table := tableOrNil(m, t)
	if table == nil {
		panic(fmt.Sprintf("No table found for type: %v", t.Name()))
	}

	if checkPK && len(table.keys) < 1 {
		e := fmt.Sprintf("gorp: No keys defined for table: %s",
			table.TableName)
		return nil, errors.New(e)
	}

	return table, nil
}

func tableOrNil(m *DbMap, t reflect.Type) *TableMap {
	for i := range m.tables {
		table := m.tables[i]
		if table.gotype == t {
			return table
		}
	}
	return nil
}

func (m *DbMap) tableForPointer(ptr interface{}, checkPK bool) (*TableMap, reflect.Value, error) {
	ptrv := reflect.ValueOf(ptr)
	if ptrv.Kind() != reflect.Ptr {
		e := fmt.Sprintf("gorp: passed non-pointer: %v (kind=%v)", ptr,
			ptrv.Kind())
		return nil, reflect.Value{}, errors.New(e)
	}
	elem := ptrv.Elem()
	etype := reflect.TypeOf(elem.Interface())
	t, err := m.tableFor(etype, checkPK)
	if err != nil {
		return nil, reflect.Value{}, err
	}

	return t, elem, nil
}

func (m *DbMap) queryRow(query string, args ...interface{}) *sql.Row {
	m.trace(query, args)
	return m.Db.QueryRow(query, args...)
}

func (m *DbMap) queryRowx(query string, args ...interface{}) *sqlx.Row {
	m.trace(query, args)
	return m.Dbx.QueryRowx(query, args...)
}

func (m *DbMap) query(query string, args ...interface{}) (*sql.Rows, error) {
	m.trace(query, args)
	return m.Db.Query(query, args...)
}

func (m *DbMap) trace(query string, args ...interface{}) {
	if m.logger != nil {
		m.logger.Printf("%s%s %v", m.logPrefix, query, args)
	}
}

///////////////

// Same behavior as DbMap.Insert(), but runs in a transaction
func (t *Transaction) Insert(list ...interface{}) error {
	return insert(t.dbmap, t, list...)
}

// Same behavior as DbMap.Update(), but runs in a transaction
func (t *Transaction) Update(list ...interface{}) (int64, error) {
	return update(t.dbmap, t, list...)
}

// Same behavior as DbMap.Delete(), but runs in a transaction
func (t *Transaction) Delete(list ...interface{}) (int64, error) {
	return delete(t.dbmap, t, list...)
}

// Same behavior as DbMap.Get(), but runs in a transaction
func (t *Transaction) Get(dest interface{}, keys ...interface{}) error {
	return get(t.dbmap, t, dest, keys...)
}

// Same behavior as DbMap.Select(), but runs in a transaction
func (t *Transaction) Select(i interface{}, query string, args ...interface{}) ([]interface{}, error) {
	return hookedselect(t.dbmap, t, i, query, args...)
}

// Same behavior as DbMap.Exec(), but runs in a transaction
func (t *Transaction) Exec(query string, args ...interface{}) (sql.Result, error) {
	t.dbmap.trace(query, args)
	stmt, err := t.tx.Prepare(query)
	if err != nil {
		return nil, err
	}
	return stmt.Exec(args...)
}

// Commits the underlying database transaction
func (t *Transaction) Commit() error {
	return t.tx.Commit()
}

// Rolls back the underlying database transaction
func (t *Transaction) Rollback() error {
	return t.tx.Rollback()
}

func (t *Transaction) queryRow(query string, args ...interface{}) *sql.Row {
	t.dbmap.trace(query, args)
	return t.tx.QueryRow(query, args...)
}

func (t *Transaction) queryRowx(query string, args ...interface{}) *sqlx.Row {
	t.dbmap.trace(query, args)
	return t.tx.QueryRowx(query, args...)
}

func (t *Transaction) query(query string, args ...interface{}) (*sql.Rows, error) {
	t.dbmap.trace(query, args)
	return t.tx.Query(query, args...)
}

///////////////

func hookedselect(m *DbMap, exec SqlExecutor, i interface{}, query string,
	args ...interface{}) ([]interface{}, error) {

	list, err := rawselect(m, exec, i, query, args...)
	if err != nil {
		return nil, err
	}

	// FIXME: should PostGet hooks be run on regular selects?  a PostGet
	// hook has access to the object and the database, and I'd hate for
	// a query to execute SQL on every row of a queryset.

	// Determine where the results are: written to i, or returned in list
	if t := toSliceType(i); t == nil {
		for _, v := range list {
			err = runHook("PostGet", reflect.ValueOf(v), hookArg(exec))
			if err != nil {
				return nil, err
			}
		}
	} else {
		resultsValue := reflect.Indirect(reflect.ValueOf(i))
		for i := 0; i < resultsValue.Len(); i++ {
			err = runHook("PostGet", resultsValue.Index(i), hookArg(exec))
			if err != nil {
				return nil, err
			}
		}
	}
	return list, nil
}

func toType(i interface{}) (reflect.Type, error) {
	t := reflect.TypeOf(i)

	// If a Pointer to a type, follow
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}

	if t.Kind() != reflect.Struct {
		return nil, errors.New("Cannot select into non-struct type.")
	}
	return t, nil

}

func rawselect(m *DbMap, exec SqlExecutor, i interface{}, query string, args ...interface{}) ([]interface{}, error) {
	appendToSlice := false // Write results to i directly?

	// get type for i, verifying it's a struct or a pointer-to-slice
	t, err := toType(i)
	if err != nil {
		if t = toSliceType(i); t == nil {
			return nil, err
		}
		appendToSlice = true
	}

	// Run the query
	rows, err := exec.query(query, args...)
	if err != nil {
		return nil, err
	}

	defer rows.Close()

	// Fetch the column names as returned from db
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}

	// check if type t is a mapped table - if so we'll
	// check the table for column aliasing below
	tableMapped := false
	table := tableOrNil(m, t)
	if table != nil {
		tableMapped = true
	}

	colToFieldOffset := make([]int, len(cols))

	numField := t.NumField()

	// Loop over column names and find field in i to bind to
	// based on column name. all returned columns must match
	// a field in the i struct
	for x := range cols {
		colToFieldOffset[x] = -1
		colName := strings.ToLower(cols[x])
		for y := 0; y < numField; y++ {
			field := t.Field(y)
			fieldName := field.Tag.Get("db")

			if fieldName == "-" {
				continue
			} else if fieldName == "" {
				fieldName = strings.ToLower(field.Name)
			}
			if tableMapped {
				colMap := colMapOrNil(table, fieldName)
				if colMap != nil {
					fieldName = colMap.ColumnName
				}
			}
			fieldName = strings.ToLower(fieldName)

			if fieldName == colName {
				colToFieldOffset[x] = y
				break
			}
		}
		if colToFieldOffset[x] == -1 {
			e := fmt.Sprintf("gorp: No field %s in type %s (query: %s)",
				colName, t.Name(), query)
			return nil, errors.New(e)
		}
	}

	// Add results to one of these two slices.
	var (
		list       = make([]interface{}, 0)
		sliceValue = reflect.Indirect(reflect.ValueOf(i))
	)

	for {
		if !rows.Next() {
			// if error occured return rawselect
			if rows.Err() != nil {
				return nil, rows.Err()
			}
			// time to exit from outer "for" loop
			break
		}
		v := reflect.New(t)
		dest := make([]interface{}, len(cols))

		custScan := make([]CustomScanner, 0)

		for x := range cols {
			f := v.Elem().Field(colToFieldOffset[x])
			target := f.Addr().Interface()
			dest[x] = target
		}

		err = rows.Scan(dest...)
		if err != nil {
			return nil, err
		}

		for _, c := range custScan {
			err = c.Bind()
			if err != nil {
				return nil, err
			}
		}

		if appendToSlice {
			sliceValue.Set(reflect.Append(sliceValue, v))
		} else {
			list = append(list, v.Interface())
		}
	}

	if appendToSlice && sliceValue.IsNil() {
		sliceValue.Set(reflect.MakeSlice(sliceValue.Type(), 0, 0))
	}

	return list, nil
}

func fieldByName(val reflect.Value, fieldName string) *reflect.Value {
	// try to find field by exact match
	f := val.FieldByName(fieldName)

	if f != zeroVal {
		return &f
	}

	// try to find by case insensitive match - only the Postgres driver
	// seems to require this - in the case where columns are aliased in the sql
	fieldNameL := strings.ToLower(fieldName)
	fieldCount := val.NumField()
	t := val.Type()
	for i := 0; i < fieldCount; i++ {
		sf := t.Field(i)
		if strings.ToLower(sf.Name) == fieldNameL {
			f := val.Field(i)
			return &f
		}
	}

	return nil
}

// toSliceType returns the element type of the given object, if the object is a
// "*[]*Element". If not, returns nil.
func toSliceType(i interface{}) reflect.Type {
	t := reflect.TypeOf(i)
	if t.Kind() != reflect.Ptr {
		return nil
	}
	if t = t.Elem(); t.Kind() != reflect.Slice {
		return nil
	}
	if t = t.Elem(); t.Kind() != reflect.Ptr {
		return nil
	}
	if t = t.Elem(); t.Kind() != reflect.Struct {
		return nil
	}
	return t
}

func get(m *DbMap, exec SqlExecutor, dest interface{}, keys ...interface{}) error {

	t, err := sqlx.BaseStructType(reflect.TypeOf(dest))
	if err != nil {
		return err
	}

	table, err := m.tableFor(t, true)
	if err != nil {
		return err
	}

	plan := table.bindGet()
	row := exec.queryRowx(plan.query, keys...)
	err = row.StructScan(dest)

	if err != nil {
		return err
	}

	if table.CanPostGet {
		err = dest.(PostGetter).PostGet(exec)
		if err != nil {
			return err
		}
	}

	return nil
}

func delete(m *DbMap, exec SqlExecutor, list ...interface{}) (int64, error) {
	var err error
	var table *TableMap
	var elem reflect.Value
	var count int64

	for _, ptr := range list {
		table, elem, err = m.tableForPointer(ptr, true)
		if err != nil {
			return -1, err
		}

		if table.CanPreDelete {
			err = ptr.(PreDeleter).PreDelete(exec)
			if err != nil {
				return -1, err
			}
		}

		bi := table.bindDelete(elem)

		res, err := exec.Exec(bi.query, bi.args...)
		if err != nil {
			return -1, err
		}

		rows, err := res.RowsAffected()
		if err != nil {
			return -1, err
		}

		if rows == 0 && bi.existingVersion > 0 {
			return lockError(m, exec, table.TableName, bi.existingVersion, elem, bi.keys...)
		}

		count += rows

		if table.CanPostDelete {
			err = ptr.(PostDeleter).PostDelete(exec)
			if err != nil {
				return -1, err
			}
		}
	}

	return count, nil
}

func update(m *DbMap, exec SqlExecutor, list ...interface{}) (int64, error) {
	var err error
	var table *TableMap
	var elem reflect.Value
	var count int64

	for _, ptr := range list {
		table, elem, err = m.tableForPointer(ptr, true)
		if err != nil {
			return -1, err
		}

		if table.CanPreUpdate {
			err = ptr.(PreUpdater).PreUpdate(exec)
			if err != nil {
				return -1, err
			}
		}

		bi := table.bindUpdate(elem)
		if err != nil {
			return -1, err
		}

		res, err := exec.Exec(bi.query, bi.args...)
		if err != nil {
			return -1, err
		}

		rows, err := res.RowsAffected()
		if err != nil {
			return -1, err
		}

		if rows == 0 && bi.existingVersion > 0 {
			return lockError(m, exec, table.TableName,
				bi.existingVersion, elem, bi.keys...)
		}

		if bi.versField != "" {
			elem.FieldByName(bi.versField).SetInt(bi.existingVersion + 1)
		}

		count += rows

		if table.CanPostUpdate {
			err = ptr.(PostUpdater).PostUpdate(exec)

			if err != nil {
				return -1, err
			}
		}
	}
	return count, nil
}

func insert(m *DbMap, exec SqlExecutor, list ...interface{}) error {
	var err error
	var table *TableMap
	var elem reflect.Value

	for _, ptr := range list {
		table, elem, err = m.tableForPointer(ptr, false)
		if err != nil {
			return err
		}

		if table.CanPreInsert {
			err = ptr.(PreInserter).PreInsert(exec)
			if err != nil {
				return err
			}
		}

		bi := table.bindInsert(elem)
		if err != nil {
			return err
		}

		if bi.autoIncrIdx > -1 {
			id, err := m.Dialect.InsertAutoIncr(exec, bi.query, bi.args...)
			if err != nil {
				return err
			}
			f := elem.Field(bi.autoIncrIdx)
			k := f.Kind()
			if (k == reflect.Int) || (k == reflect.Int16) || (k == reflect.Int32) || (k == reflect.Int64) {
				f.SetInt(id)
			} else {
				return errors.New(fmt.Sprintf("gorp: Cannot set autoincrement value on non-Int field. SQL=%s  autoIncrIdx=%d", bi.query, bi.autoIncrIdx))
			}
		} else {
			_, err := exec.Exec(bi.query, bi.args...)
			if err != nil {
				return err
			}
		}

		if table.CanPostInsert {
			err = ptr.(PostInserter).PostInsert(exec)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func hookArg(exec SqlExecutor) []reflect.Value {
	execval := reflect.ValueOf(exec)
	return []reflect.Value{execval}
}

func runHook(name string, eptr reflect.Value, arg []reflect.Value) error {
	hook := eptr.MethodByName(name)
	if hook != zeroVal {
		ret := hook.Call(arg)
		if len(ret) > 0 && !ret[0].IsNil() {
			return ret[0].Interface().(error)
		}
	}
	return nil
}

func lockError(m *DbMap, exec SqlExecutor, tableName string, existingVer int64, elem reflect.Value, keys ...interface{}) (int64, error) {

	dest := reflect.New(elem.Type()).Interface()
	err := get(m, exec, dest, keys...)
	if err != nil {
		return -1, err
	}

	ole := OptimisticLockError{tableName, keys, true, existingVer}
	if dest == nil {
		ole.RowExists = false
	}
	return -1, ole
}

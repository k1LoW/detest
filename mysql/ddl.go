package mysql

import (
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/k1LoW/detest/internal/sqlir"
	"github.com/pingcap/tidb/pkg/parser/ast"
	"github.com/pingcap/tidb/pkg/parser/mysql"
	"github.com/pingcap/tidb/pkg/parser/types"
)

func (c *conv) createTable(s *ast.CreateTableStmt) (sqlir.SchemaChange, error) {
	if s.ReferTable != nil {
		return sqlir.SchemaChange{}, c.unsupported("CREATE TABLE ... LIKE")
	}
	ch := sqlir.SchemaChange{Table: tableName(s.Table), Create: true, IfNotExists: s.IfNotExists}
	if err := c.tableOptions(&ch, s.Options); err != nil {
		return ch, err
	}
	for _, col := range s.Cols {
		if err := c.column(&ch, col); err != nil {
			return ch, err
		}
	}
	autoInc := 0
	for _, col := range ch.Columns {
		if col.AutoIncrement {
			autoInc++
		}
	}
	if autoInc > 1 {
		// Refused here, before any of the table is created.
		return ch, c.unsupported("more than one AUTO_INCREMENT column")
	}
	for _, k := range s.Constraints {
		if err := c.constraint(&ch, k); err != nil {
			return ch, err
		}
	}
	return ch, nil
}

// tableOptions takes the table options detest acts on, AUTO_INCREMENT=n, into
// ch, and refuses an engine other than InnoDB, whose semantics detest gives
// every table; the others, such as CHARSET, declare nothing it needs.
func (c *conv) tableOptions(ch *sqlir.SchemaChange, opts []*ast.TableOption) error {
	for _, o := range opts {
		switch o.Tp {
		case ast.TableOptionAutoIncrement:
			ch.AutoIncrement = int64(min(o.UintValue, math.MaxInt64))
		case ast.TableOptionEngine:
			if !strings.EqualFold(o.StrValue, "InnoDB") {
				return c.unsupported("ENGINE=" + o.StrValue)
			}
		}
	}
	return nil
}

// place gives the column ch defined last the position of a MySQL column
// specification.
func place(ch *sqlir.SchemaChange, pos *ast.ColumnPosition) {
	if pos == nil || len(ch.Columns) == 0 {
		return
	}
	col := &ch.Columns[len(ch.Columns)-1]
	switch pos.Tp {
	case ast.ColumnPositionFirst:
		col.First = true
	case ast.ColumnPositionAfter:
		col.After = pos.RelativeColumn.Name.L
	}
}

func (c *conv) createTableAs(s *ast.CreateTableStmt) (sqlir.Statement, error) {
	var opts sqlir.SchemaChange
	if err := c.tableOptions(&opts, s.Options); err != nil {
		return nil, err
	}
	if len(s.Cols) > 0 || len(s.Constraints) > 0 || opts.AutoIncrement > 0 {
		// The table takes its columns from the query; declarations beside
		// it would need the whole of CREATE TABLE merged in.
		return nil, c.unsupported("CREATE TABLE ... SELECT with column definitions, keys or AUTO_INCREMENT")
	}
	sel, err := c.query1(s.Select)
	if err != nil {
		return nil, err
	}
	return &sqlir.CreateTableAsStmt{Table: tableName(s.Table), Select: sel, IfNotExists: s.IfNotExists}, nil
}

// column adds a column definition, with the constraints written on it, to
// ch.
func (c *conv) column(ch *sqlir.SchemaChange, d *ast.ColumnDef) error {
	name := d.Name.Name.L
	col := sqlir.ColumnDef{Name: name, Type: typeName(d.Tp)}
	if d.Tp != nil {
		switch d.Tp.GetType() {
		case mysql.TypeVarchar, mysql.TypeString, mysql.TypeVarString:
			// CHAR(n) and VARCHAR(n) count characters; their BINARY
			// counterparts count bytes, which detest does not check.
			if d.Tp.GetCharset() != "binary" && d.Tp.GetFlen() > 0 {
				col.MaxLen = d.Tp.GetFlen()
			}
		case mysql.TypeEnum, mysql.TypeSet:
			col.Members, col.Set = d.Tp.GetElems(), d.Tp.GetType() == mysql.TypeSet
		case mysql.TypeDatetime, mysql.TypeTimestamp:
			col.FSP = max(d.Tp.GetDecimal(), 0)
		}
	}
	for _, o := range d.Options {
		switch o.Tp {
		case ast.ColumnOptionPrimaryKey:
			ch.Constraints = append(ch.Constraints, sqlir.UniqueDef{Name: "PRIMARY", Primary: true, Elems: []sqlir.Expr{&sqlir.ColumnRef{Column: name}}})
		case ast.ColumnOptionUniqKey:
			ch.Constraints = append(ch.Constraints, sqlir.UniqueDef{Name: name, Elems: []sqlir.Expr{&sqlir.ColumnRef{Column: name}}})
		case ast.ColumnOptionNotNull:
			col.NotNull = true
		case ast.ColumnOptionAutoIncrement:
			col.AutoIncrement = true
		case ast.ColumnOptionDefaultValue:
			col.Default = c.defaultExpr(o.Expr)
		case ast.ColumnOptionOnUpdate:
			e, err := c.expr(o.Expr)
			if err != nil {
				return err
			}
			col.OnUpdate = e
		case ast.ColumnOptionGenerated:
			return c.unsupported("generated column")
		case ast.ColumnOptionCheck:
			if !o.Enforced {
				continue
			}
			ch.Checks = append(ch.Checks, sqlir.CheckDef{Name: o.ConstraintName, Expr: c.checkExpr(o.Expr)})
		case ast.ColumnOptionReference:
			fk, err := c.reference(ch.Table, len(ch.ForeignKeys), "", []string{name}, o.Refer)
			if err != nil {
				return err
			}
			ch.ForeignKeys = append(ch.ForeignKeys, fk)
		}
	}
	ch.Columns = append(ch.Columns, col)
	return nil
}

// checkExpr converts a CHECK. One detest cannot convert becomes Unconverted,
// as Postgres's does, so the schema still loads and a write the check would guard fails
// as unsupported, rather than pass a check that is not there.
func (c *conv) checkExpr(n ast.ExprNode) sqlir.Expr {
	e, err := c.cond(n)
	if err != nil {
		return &sqlir.Unconverted{}
	}
	return e
}

// defaultExpr converts a column default. One detest cannot convert becomes
// Unconverted, so the schema still loads.
func (c *conv) defaultExpr(n ast.ExprNode) sqlir.Expr {
	e, err := c.expr(n)
	if err != nil {
		return &sqlir.Unconverted{}
	}
	return e
}

// typeName is the column type the executor checks writes against: the
// integer types by their width in bytes, with a u for unsigned ones.
func typeName(t *types.FieldType) string {
	if t == nil {
		return ""
	}
	var w string
	switch t.GetType() {
	case mysql.TypeTiny:
		w = "1"
	case mysql.TypeShort:
		w = "2"
	case mysql.TypeInt24:
		w = "3"
	case mysql.TypeLong:
		w = "4"
	case mysql.TypeLonglong:
		w = "8"
	default:
		return strings.ToLower(types.TypeToStr(t.GetType(), t.GetCharset()))
	}
	if mysql.HasUnsignedFlag(t.GetFlag()) {
		return "uint" + w
	}
	return "int" + w
}

// keyColumns converts the parts of an index: the elements the index holds,
// and the leading columns it holds whole, which InnoDB's gap locks follow. A
// prefix part, such as name(3), holds the column's first characters, as an
// expression; the columns stop at the first part that is not a whole column.
// first is the first part's column, which names an unnamed index.
func (c *conv) keyColumns(keys []*ast.IndexPartSpecification) (elems []sqlir.Expr, cols []string, first string, err error) {
	whole := true
	for _, k := range keys {
		if k.Expr != nil {
			e, err := c.expr(k.Expr)
			if err != nil {
				return nil, nil, "", err
			}
			elems = append(elems, e)
			whole = false
			continue
		}
		name := k.Column.Name.L
		if first == "" {
			first = name
		}
		var e sqlir.Expr = &sqlir.ColumnRef{Column: name}
		if k.Length > 0 {
			e = &sqlir.FuncCall{Name: "left", Args: []sqlir.Expr{e, &sqlir.Const{Value: int64(k.Length)}}}
			whole = false
		}
		if whole {
			cols = append(cols, name)
		}
		elems = append(elems, e)
	}
	return elems, cols, first, nil
}

// descending reports an index with a descending part. The gap locks follow
// ascending key order, so a locking search by such an index is refused, and
// a descending unique key, which every search for its point lookups uses,
// with it.
func descending(keys []*ast.IndexPartSpecification) bool {
	return slices.ContainsFunc(keys, func(k *ast.IndexPartSpecification) bool { return k.Desc })
}

// constraint adds a table constraint or index to ch.
func (c *conv) constraint(ch *sqlir.SchemaChange, k *ast.Constraint) error {
	switch k.Tp {
	case ast.ConstraintPrimaryKey:
		if descending(k.Keys) {
			return c.unsupported("a descending primary key")
		}
		elems, _, _, err := c.keyColumns(k.Keys)
		if err != nil {
			return err
		}
		ch.Constraints = append(ch.Constraints, sqlir.UniqueDef{Name: "PRIMARY", Primary: true, Elems: elems})
	case ast.ConstraintUniq, ast.ConstraintUniqKey, ast.ConstraintUniqIndex:
		if descending(k.Keys) {
			return c.unsupported("a descending unique index")
		}
		elems, _, first, err := c.keyColumns(k.Keys)
		if err != nil {
			return err
		}
		name := k.Name
		if name == "" {
			name = first // MySQL names an unnamed index after its first column
		}
		ch.Constraints = append(ch.Constraints, sqlir.UniqueDef{Name: name, Elems: elems})
	case ast.ConstraintKey, ast.ConstraintIndex:
		_, cols, first, err := c.keyColumns(k.Keys)
		if err != nil {
			return err
		}
		name := k.Name
		if name == "" {
			name = first
		}
		ix := sqlir.IndexDef{Name: name, Columns: cols, Desc: descending(k.Keys)}
		if len(cols) == 0 && k.Keys[0].Expr == nil && k.Keys[0].Length > 0 {
			ix.Prefix = first
		}
		ch.Indexes = append(ch.Indexes, ix)
	case ast.ConstraintForeignKey:
		_, cols, _, err := c.keyColumns(k.Keys)
		if err != nil {
			return err
		}
		if len(cols) != len(k.Keys) {
			return c.unsupported("a foreign key on a prefix or an expression, which MySQL refuses")
		}
		fk, err := c.reference(ch.Table, len(ch.ForeignKeys), k.Name, cols, k.Refer)
		if err != nil {
			return err
		}
		ch.ForeignKeys = append(ch.ForeignKeys, fk)
	case ast.ConstraintCheck:
		if !k.Enforced {
			return nil
		}
		ch.Checks = append(ch.Checks, sqlir.CheckDef{Name: k.Name, Expr: c.checkExpr(k.Expr)})
	}
	// FULLTEXT and other index kinds constrain nothing detest checks.
	return nil
}

var referOpts = map[ast.ReferOptionType]string{
	ast.ReferOptionNoOption: "no action", ast.ReferOptionNoAction: "no action", ast.ReferOptionRestrict: "restrict",
	ast.ReferOptionCascade: "cascade", ast.ReferOptionSetNull: "set null", ast.ReferOptionSetDefault: "set default",
}

// reference converts a foreign key. MySQL names an unnamed one table_ibfk_N.
func (c *conv) reference(table string, n int, name string, cols []string, r *ast.ReferenceDef) (sqlir.ForeignKey, error) {
	if r == nil {
		return sqlir.ForeignKey{}, c.unsupported("foreign key without REFERENCES")
	}
	if name == "" {
		name = fmt.Sprintf("%s_ibfk_%d", relname(table), n+1)
	}
	// InnoDB parses MATCH but checks every foreign key as MATCH SIMPLE.
	fk := sqlir.ForeignKey{Name: name, Columns: cols, RefTable: tableName(r.Table), OnDelete: "no action", OnUpdate: "no action"}
	for _, k := range r.IndexPartSpecifications {
		fk.RefColumns = append(fk.RefColumns, k.Column.Name.L)
	}
	if r.OnDelete != nil {
		fk.OnDelete = referOpts[r.OnDelete.ReferOpt]
	}
	if r.OnUpdate != nil {
		fk.OnUpdate = referOpts[r.OnUpdate.ReferOpt]
	}
	if fk.OnDelete == "set default" || fk.OnUpdate == "set default" {
		return fk, c.unsupported("SET DEFAULT, which InnoDB refuses in a foreign key")
	}
	return fk, nil
}

func relname(table string) string {
	if i := strings.LastIndex(table, "."); i >= 0 {
		return table[i+1:]
	}
	return table
}

func (c *conv) alterTable(s *ast.AlterTableStmt) ([]sqlir.SchemaChange, error) {
	table := tableName(s.Table)
	ch := sqlir.SchemaChange{Table: table}
	var renames []sqlir.SchemaChange // column renames, applied before ch's definitions
	out := []sqlir.SchemaChange{}
	for _, spec := range s.Specs {
		switch spec.Tp {
		case ast.AlterTableAddColumns:
			for _, col := range spec.NewColumns {
				if err := c.column(&ch, col); err != nil {
					return nil, err
				}
				place(&ch, spec.Position)
			}
			for _, k := range spec.NewConstraints {
				if err := c.constraint(&ch, k); err != nil {
					return nil, err
				}
			}
		case ast.AlterTableAddConstraint:
			if err := c.constraint(&ch, spec.Constraint); err != nil {
				return nil, err
			}
		case ast.AlterTableDropColumn:
			ch.DropColumns = append(ch.DropColumns, spec.OldColumnName.Name.L)
		case ast.AlterTableDropPrimaryKey:
			ch.DropIndexes = append(ch.DropIndexes, "PRIMARY")
		case ast.AlterTableDropIndex:
			ch.DropIndexes = append(ch.DropIndexes, spec.Name)
		case ast.AlterTableDropForeignKey:
			ch.DropForeignKeys = append(ch.DropForeignKeys, spec.Name)
		case ast.AlterTableDropCheck:
			ch.DropChecks = append(ch.DropChecks, spec.Name)
		case ast.AlterTableModifyColumn, ast.AlterTableChangeColumn:
			if len(spec.NewColumns) != 1 {
				return nil, c.unsupported("MODIFY or CHANGE of several columns")
			}
			nc := spec.NewColumns[0]
			if spec.Tp == ast.AlterTableChangeColumn && spec.OldColumnName.Name.L != nc.Name.Name.L {
				// CHANGE renames first, so the new definition replaces the
				// renamed column's rather than adding a second one.
				renames = append(renames, sqlir.SchemaChange{Table: table, RenameColumn: [2]string{spec.OldColumnName.Name.L, nc.Name.Name.L}})
			}
			// The new definition replaces the column's: its default, and
			// NOT NULL and AUTO_INCREMENT unless it says them again.
			ch.Columns = append(ch.Columns, sqlir.ColumnDef{Name: nc.Name.Name.L, DropNotNull: true, DropAutoIncrement: true, TypeOnly: true})
			if err := c.column(&ch, nc); err != nil {
				return nil, err
			}
			place(&ch, spec.Position)
		case ast.AlterTableRenameColumn:
			out = append(out, sqlir.SchemaChange{Table: table, RenameColumn: [2]string{spec.OldColumnName.Name.L, spec.NewColumnName.Name.L}})
		case ast.AlterTableRenameIndex:
			out = append(out, sqlir.SchemaChange{Table: table, RenameIndex: [2]string{spec.FromKey.O, spec.ToKey.O}})
		case ast.AlterTableRenameTable:
			out = append(out, sqlir.SchemaChange{Table: table, RenameTo: tableName(spec.NewTable)})
		case ast.AlterTableAlterColumn:
			if len(spec.NewColumns) != 1 {
				continue
			}
			col := sqlir.ColumnDef{Name: spec.NewColumns[0].Name.Name.L}
			for _, o := range spec.NewColumns[0].Options {
				if o.Tp == ast.ColumnOptionDefaultValue {
					col.Default = c.defaultExpr(o.Expr)
				}
			}
			ch.Columns = append(ch.Columns, col)
		case ast.AlterTableOption:
			if err := c.tableOptions(&ch, spec.Options); err != nil {
				return nil, err
			}
		case ast.AlterTableDropPartition, ast.AlterTableTruncatePartition, ast.AlterTableExchangePartition,
			ast.AlterTableReorganizePartition, ast.AlterTableCoalescePartitions, ast.AlterTableDiscardPartitionTablespace,
			ast.AlterTableImportPartitionTablespace:
			// These change the rows, which detest keeps without partitions.
			return nil, c.unsupported("a partition operation that changes rows")
		}
		// Partitioning otherwise, and algorithm hints, declare nothing detest
		// needs.
	}
	return append(append(renames, ch), out...), nil
}

func (c *conv) createIndex(s *ast.CreateIndexStmt) (sqlir.SchemaChange, error) {
	ch := sqlir.SchemaChange{Table: tableName(s.Table)}
	switch s.KeyType {
	case ast.IndexKeyTypeFulltext, ast.IndexKeyTypeSpatial, ast.IndexKeyTypeVector, ast.IndexKeyTypeColumnar:
		// Not a B-tree a range search goes along, as in CREATE TABLE.
		return ch, nil
	}
	k := &ast.Constraint{Name: s.IndexName, Keys: s.IndexPartSpecifications, Tp: ast.ConstraintIndex}
	if s.KeyType == ast.IndexKeyTypeUnique {
		k.Tp = ast.ConstraintUniqIndex
	}
	return ch, c.constraint(&ch, k)
}

func (c *conv) createView(s *ast.CreateViewStmt) (sqlir.Statement, error) {
	sel, err := c.query1(s.Select)
	if err != nil {
		return nil, err
	}
	ch := sqlir.SchemaChange{Table: tableName(s.ViewName), Object: "view", View: sel, Replace: s.OrReplace}
	for _, col := range s.Cols {
		ch.ViewColumns = append(ch.ViewColumns, col.L)
	}
	return &sqlir.SchemaStmt{Changes: []sqlir.SchemaChange{ch}}, nil
}

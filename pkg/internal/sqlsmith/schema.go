// Copyright 2019 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

package sqlsmith

import (
	gosql "database/sql"
	"fmt"
	"strings"

	// Import builtins so they are reflected in tree.FunDefs.
	_ "github.com/cockroachdb/cockroach/pkg/sql/sem/builtins"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/types"
	"github.com/lib/pq/oid"
)

// tableRef represents a table and its columns.
type tableRef struct {
	TableName *tree.TableName
	Columns   []*tree.ColumnTableDef
}

type tableRefs []*tableRef

func (t tableRefs) Pop() (*tableRef, tableRefs) {
	return t[0], t[1:]
}

// ReloadSchemas loads tables from the database.
func (s *Smither) ReloadSchemas() error {
	if s.db == nil {
		return nil
	}
	s.lock.Lock()
	defer s.lock.Unlock()
	var err error
	s.tables, err = extractTables(s.db)
	if err != nil {
		return err
	}
	s.indexes, err = extractIndexes(s.db, s.tables)
	return err
}

func (s *Smither) getRandTable() (*tableRef, bool) {
	s.lock.RLock()
	defer s.lock.RUnlock()
	if len(s.tables) == 0 {
		return nil, false
	}
	return s.tables[s.rnd.Intn(len(s.tables))], true
}

func (s *Smither) getIndexes(table tree.TableName) map[tree.Name]*tree.CreateIndex {
	s.lock.RLock()
	defer s.lock.RUnlock()
	return s.indexes[table]
}

func (s *Smither) getRandTableIndex(
	table tree.TableName,
) (*tree.TableIndexName, *tree.CreateIndex, bool) {
	indexes := s.getIndexes(table)
	if len(indexes) == 0 {
		return nil, nil, false
	}
	names := make([]tree.Name, 0, len(indexes))
	for n := range indexes {
		names = append(names, n)
	}
	idx := indexes[names[s.rnd.Intn(len(names))]]
	return &tree.TableIndexName{
		Table: table,
		Index: tree.UnrestrictedName(idx.Name),
	}, idx, true
}

func (s *Smither) getRandIndex() (*tree.TableIndexName, *tree.CreateIndex, bool) {
	tableRef, ok := s.getRandTable()
	if !ok {
		return nil, nil, false
	}
	return s.getRandTableIndex(*tableRef.TableName)
}

func extractTables(db *gosql.DB) ([]*tableRef, error) {
	rows, err := db.Query(`
SELECT
	table_catalog,
	table_schema,
	table_name,
	column_name,
	crdb_sql_type,
	generation_expression != '' AS computed,
	is_nullable = 'YES' AS nullable,
	is_hidden = 'YES' AS hidden
FROM
	information_schema.columns
WHERE
	table_schema = 'public'
ORDER BY
	table_catalog, table_schema, table_name
	`)
	// TODO(justin): have a flag that includes system tables?
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// This is a little gross: we want to operate on each segment of the results
	// that corresponds to a single table. We could maybe json_agg the results
	// or something for a cleaner processing step?

	firstTime := true
	var lastCatalog, lastSchema, lastName tree.Name
	var tables []*tableRef
	var currentCols []*tree.ColumnTableDef
	emit := func() {
		if lastSchema != "public" {
			return
		}
		tables = append(tables, &tableRef{
			TableName: tree.NewTableName(lastCatalog, lastName),
			Columns:   currentCols,
		})
	}
	for rows.Next() {
		var catalog, schema, name, col tree.Name
		var typ string
		var computed, nullable, hidden bool
		if err := rows.Scan(&catalog, &schema, &name, &col, &typ, &computed, &nullable, &hidden); err != nil {
			return nil, err
		}
		if hidden {
			continue
		}

		if firstTime {
			lastCatalog = catalog
			lastSchema = schema
			lastName = name
		}
		firstTime = false

		if lastCatalog != catalog || lastSchema != schema || lastName != name {
			emit()
			currentCols = nil
		}

		coltyp := typeFromName(typ)
		column := tree.ColumnTableDef{
			Name: col,
			Type: coltyp,
		}
		if nullable {
			column.Nullable.Nullability = tree.Null
		}
		if computed {
			column.Computed.Computed = true
		}
		currentCols = append(currentCols, &column)
		lastCatalog = catalog
		lastSchema = schema
		lastName = name
	}
	if !firstTime {
		emit()
	}
	return tables, rows.Err()
}

func extractIndexes(
	db *gosql.DB, tables tableRefs,
) (map[tree.TableName]map[tree.Name]*tree.CreateIndex, error) {
	ret := map[tree.TableName]map[tree.Name]*tree.CreateIndex{}

	for _, t := range tables {
		indexes := map[tree.Name]*tree.CreateIndex{}
		rows, err := db.Query(fmt.Sprintf(`SELECT index_name, column_name, storing, direction = 'ASC' FROM [SHOW INDEXES FROM %s]`, t.TableName))
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var idx, col tree.Name
			var storing, ascending bool
			if err := rows.Scan(&idx, &col, &storing, &ascending); err != nil {
				rows.Close()
				return nil, err
			}
			if _, ok := indexes[idx]; !ok {
				indexes[idx] = &tree.CreateIndex{
					Name:  idx,
					Table: *t.TableName,
				}
			}
			create := indexes[idx]
			if storing {
				create.Storing = append(create.Storing, col)
			} else {
				dir := tree.Ascending
				if !ascending {
					dir = tree.Descending
				}
				create.Columns = append(create.Columns, tree.IndexElem{
					Column:    col,
					Direction: dir,
				})
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		ret[*t.TableName] = indexes
	}
	return ret, nil
}

type operator struct {
	*tree.BinOp
	Operator tree.BinaryOperator
}

var operators = func() map[oid.Oid][]operator {
	m := map[oid.Oid][]operator{}
	for BinaryOperator, overload := range tree.BinOps {
		for _, ov := range overload {
			bo := ov.(*tree.BinOp)
			m[bo.ReturnType.Oid()] = append(m[bo.ReturnType.Oid()], operator{
				BinOp:    bo,
				Operator: BinaryOperator,
			})
		}
	}
	return m
}()

type function struct {
	def      *tree.FunctionDefinition
	overload *tree.Overload
}

var functions = func() map[tree.FunctionClass]map[oid.Oid][]function {
	m := map[tree.FunctionClass]map[oid.Oid][]function{}
	for _, def := range tree.FunDefs {
		switch def.Name {
		case "pg_sleep":
			continue
		}
		if strings.Contains(def.Name, "crdb_internal.force_") {
			continue
		}
		if _, ok := m[def.Class]; !ok {
			m[def.Class] = map[oid.Oid][]function{}
		}
		// Ignore pg compat functions since many are unimplemented.
		if def.Category == "Compatibility" {
			continue
		}
		if def.Private {
			continue
		}
		for _, ov := range def.Definition {
			ov := ov.(*tree.Overload)
			// Ignore documented unusable functions.
			if strings.Contains(ov.Info, "Not usable") {
				continue
			}
			typ := ov.FixedReturnType()
			found := false
			for _, nonArrayTyp := range types.AnyNonArray {
				if typ.SemanticType == nonArrayTyp.SemanticType {
					found = true
				}
			}
			if !found {
				continue
			}
			m[def.Class][typ.Oid()] = append(m[def.Class][typ.Oid()], function{
				def:      def,
				overload: ov,
			})
		}
	}
	return m
}()

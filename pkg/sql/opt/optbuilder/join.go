// Copyright 2018 The Cockroach Authors.
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

package optbuilder

import (
	"fmt"

	"github.com/cockroachdb/cockroach/pkg/sql/opt"
	"github.com/cockroachdb/cockroach/pkg/sql/opt/memo"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgerror"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/types"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlbase"
	"github.com/cockroachdb/cockroach/pkg/util"
)

// buildJoin builds a set of memo groups that represent the given join table
// expression.
//
// See Builder.buildStmt for a description of the remaining input and
// return values.
func (b *Builder) buildJoin(join *tree.JoinTableExpr, inScope *scope) (outScope *scope) {
	leftScope := b.buildDataSource(join.Left, nil /* indexFlags */, inScope)
	rightScope := b.buildDataSource(join.Right, nil /* indexFlags */, inScope)

	// Check that the same table name is not used on both sides.
	b.validateJoinTableNames(leftScope, rightScope)

	joinType := sqlbase.JoinTypeFromAstString(join.Join)

	switch cond := join.Cond.(type) {
	case tree.NaturalJoinCond, *tree.UsingJoinCond:
		var usingColNames tree.NameList

		switch t := cond.(type) {
		case tree.NaturalJoinCond:
			usingColNames = commonColumns(leftScope, rightScope)
		case *tree.UsingJoinCond:
			usingColNames = t.Cols
		}

		return b.buildUsingJoin(joinType, usingColNames, leftScope, rightScope, inScope)

	case *tree.OnJoinCond, nil:
		// Append columns added by the children, as they are visible to the filter.
		outScope = inScope.push()
		outScope.appendColumnsFromScope(leftScope)
		outScope.appendColumnsFromScope(rightScope)

		var filter memo.GroupID
		if on, ok := cond.(*tree.OnJoinCond); ok {
			// Do not allow special functions in the ON clause.
			b.semaCtx.Properties.Require("ON", tree.RejectSpecial)
			outScope.context = "ON"
			filter = b.buildScalar(
				outScope.resolveAndRequireType(on.Expr, types.Bool), outScope, nil, nil, nil,
			)
		} else {
			filter = b.factory.ConstructTrue()
		}

		outScope.group = b.constructJoin(joinType, leftScope.group, rightScope.group, filter)
		return outScope

	default:
		panic(fmt.Sprintf("unsupported join condition %#v", cond))
	}
}

// validateJoinTableNames checks that table names are not repeated between the
// left and right sides of a join. leftTables contains a pre-built map of the
// tables from the left side of the join, and rightScope contains the
// scopeColumns (and corresponding table names) from the right side of the
// join.
func (b *Builder) validateJoinTableNames(leftScope, rightScope *scope) {
	// skipDups creates a FastIntSet containing the ordinal of each column that
	// has a different table name than the previous column. Only these non-
	// duplicate names need to be validated.
	skipDups := func(scope *scope) util.FastIntSet {
		var ords util.FastIntSet
		for i := range scope.cols {
			// Allow joins of sources that define columns with no
			// associated table name. At worst, the USING/NATURAL
			// detection code or expression analysis for ON will detect an
			// ambiguity later.
			if scope.cols[i].table.TableName == "" {
				continue
			}

			if i == 0 || scope.cols[i].table != scope.cols[i-1].table {
				ords.Add(i)
			}
		}
		return ords
	}

	leftOrds := skipDups(leftScope)
	rightOrds := skipDups(rightScope)

	// Look for table name in left scope that exists in right scope.
	for left, ok := leftOrds.Next(0); ok; left, ok = leftOrds.Next(left + 1) {
		leftName := &leftScope.cols[left].table

		for right, ok := rightOrds.Next(0); ok; right, ok = rightOrds.Next(right + 1) {
			rightName := &rightScope.cols[right].table

			// Must match all name parts.
			if leftName.TableName != rightName.TableName ||
				leftName.SchemaName != rightName.SchemaName ||
				leftName.CatalogName != rightName.CatalogName {
				continue
			}

			panic(builderError{pgerror.NewErrorf(
				pgerror.CodeDuplicateAliasError,
				"source name %q specified more than once (missing AS clause)",
				tree.ErrString(&leftName.TableName),
			)})
		}
	}
}

// commonColumns returns the names of columns common on the
// left and right sides, for use by NATURAL JOIN.
func commonColumns(leftScope, rightScope *scope) (common tree.NameList) {
	for i := range leftScope.cols {
		leftCol := &leftScope.cols[i]
		if leftCol.hidden {
			continue
		}
		for j := range rightScope.cols {
			rightCol := &rightScope.cols[j]
			if rightCol.hidden {
				continue
			}

			if leftCol.name == rightCol.name {
				common = append(common, leftCol.name)
				break
			}
		}
	}

	return common
}

// buildUsingJoin builds a set of memo groups that represent the given join
// table expression with the given `USING` column names. It is used for both
// USING and NATURAL joins.
//
// joinType    The join type (inner, left, right or outer)
// names       The list of `USING` column names
// leftScope   The outScope from the left table
// rightScope  The outScope from the right table
//
// See Builder.buildStmt for a description of the remaining input and
// return values.
func (b *Builder) buildUsingJoin(
	joinType sqlbase.JoinType, names tree.NameList, leftScope, rightScope, inScope *scope,
) (outScope *scope) {
	// Build the join predicate.
	mergedCols, filter, outScope := b.buildUsingJoinPredicate(
		joinType, leftScope.cols, rightScope.cols, names, inScope,
	)

	outScope.group = b.constructJoin(joinType, leftScope.group, rightScope.group, filter)

	if len(mergedCols) > 0 {
		// Wrap in a projection to include the merged columns and ensure that all
		// remaining columns are passed through unchanged.
		for i := range outScope.cols {
			col := &outScope.cols[i]
			if mergedCol, ok := mergedCols[col.id]; ok {
				col.group = mergedCol
			} else {
				// Mark column as passthrough.
				col.group = 0
			}
		}

		outScope.group = b.constructProject(outScope.group, outScope.cols)
	}

	return outScope
}

// buildUsingJoinPredicate builds a set of memo groups that represent the join
// conditions for a USING join or natural join. It finds the columns in the
// left and right relations that match the columns provided in the names
// parameter, and creates equality predicate(s) with those columns. It also
// ensures that there is a single output column for each name in `names`
// (other columns with the same name are hidden).
//
// -- Merged columns --
//
// With NATURAL JOIN or JOIN USING (a,b,c,...), SQL allows us to refer to the
// columns a,b,c directly; these columns have the following semantics:
//   a = IFNULL(left.a, right.a)
//   b = IFNULL(left.b, right.b)
//   c = IFNULL(left.c, right.c)
//   ...
//
// Furthermore, a star has to resolve the columns in the following order:
// merged columns, non-equality columns from the left table, non-equality
// columns from the right table. To perform this rearrangement, we use a
// projection on top of the join. Note that the original columns must
// still be accessible via left.a, right.a (they will just be hidden).
//
// For inner or left outer joins, a is always the same as left.a.
//
// For right outer joins, a is always equal to right.a; but for some types
// (like collated strings), this doesn't mean it is the same as right.a. In
// this case we must still use the IFNULL construct.
//
// Example:
//
//  left has columns (a,b,x)
//  right has columns (a,b,y)
//
//  - SELECT * FROM left JOIN right USING(a,b)
//
//  join has columns:
//    1: left.a
//    2: left.b
//    3: left.x
//    4: right.a
//    5: right.b
//    6: right.y
//
//  projection has columns and corresponding variable expressions:
//    1: a aka left.a        @1
//    2: b aka left.b        @2
//    3: left.x              @3
//    4: right.a (hidden)    @4
//    5: right.b (hidden)    @5
//    6: right.y             @6
//
// If the join was a FULL OUTER JOIN, the columns would be:
//    1: a                   IFNULL(@1,@4)
//    2: b                   IFNULL(@2,@5)
//    3: left.a (hidden)     @1
//    4: left.b (hidden)     @2
//    5: left.x              @3
//    6: right.a (hidden)    @4
//    7: right.b (hidden)    @5
//    8: right.y             @6
//
// If new merged columns are created (as in the FULL OUTER JOIN example above),
// the return value mergedCols contains a mapping from the column id to the
// memo group ID of the IFNULL expression. out contains the top-level memo
// group ID of the join predicate.
//
// See Builder.buildStmt for a description of the remaining input and
// return values.
func (b *Builder) buildUsingJoinPredicate(
	joinType sqlbase.JoinType,
	leftCols []scopeColumn,
	rightCols []scopeColumn,
	names tree.NameList,
	inScope *scope,
) (mergedCols map[opt.ColumnID]memo.GroupID, out memo.GroupID, outScope *scope) {
	joined := make(map[tree.Name]*scopeColumn, len(names))
	conditions := make([]memo.GroupID, 0, len(names))
	mergedCols = make(map[opt.ColumnID]memo.GroupID)
	outScope = inScope.push()

	for i, name := range names {
		if _, ok := joined[name]; ok {
			panic(builderError{pgerror.NewErrorf(pgerror.CodeDuplicateColumnError,
				"column %q appears more than once in USING clause", tree.ErrString(&names[i]))})
		}

		// For every adjacent pair of tables, add an equality predicate.
		leftCol := findUsingColumn(leftCols, name, "left")
		rightCol := findUsingColumn(rightCols, name, "right")

		if !leftCol.typ.Equivalent(rightCol.typ) {
			// First, check if the comparison would even be valid.
			if _, found := tree.FindEqualComparisonFunction(leftCol.typ, rightCol.typ); !found {
				panic(builderError{pgerror.NewErrorf(pgerror.CodeDatatypeMismatchError,
					"JOIN/USING types %s for left and %s for right cannot be matched for column %q",
					leftCol.typ, rightCol.typ, tree.ErrString(&leftCol.name))})
			}
		}

		// Construct the predicate.
		leftVar := b.factory.ConstructVariable(b.factory.InternColumnID(leftCol.id))
		rightVar := b.factory.ConstructVariable(b.factory.InternColumnID(rightCol.id))
		eq := b.factory.ConstructEq(leftVar, rightVar)
		conditions = append(conditions, eq)

		// Add the merged column to the scope, constructing a new column if needed.
		if joinType == sqlbase.InnerJoin || joinType == sqlbase.LeftOuterJoin {
			// The merged column is the same as the corresponding column from the
			// left side.
			outScope.cols = append(outScope.cols, *leftCol)
			joined[name] = leftCol
		} else if joinType == sqlbase.RightOuterJoin &&
			!sqlbase.DatumTypeHasCompositeKeyEncoding(leftCol.typ) {
			// The merged column is the same as the corresponding column from the
			// right side.
			outScope.cols = append(outScope.cols, *rightCol)
			joined[name] = rightCol
		} else {
			// Construct a new merged column to represent IFNULL(left, right).
			var typ types.T
			if leftCol.typ != types.Unknown {
				typ = leftCol.typ
			} else {
				typ = rightCol.typ
			}
			texpr := tree.NewTypedCoalesceExpr(tree.TypedExprs{leftCol, rightCol}, typ)
			merged := b.factory.ConstructCoalesce(b.factory.InternList([]memo.GroupID{leftVar, rightVar}))
			col := b.synthesizeColumn(outScope, string(leftCol.name), typ, texpr, merged)
			mergedCols[col.id] = merged
			joined[name] = nil
		}
	}

	// Hide other columns that have the same name as the merged columns.
	hideMatchingColumns(leftCols, joined, outScope)
	hideMatchingColumns(rightCols, joined, outScope)

	return mergedCols, b.constructFilter(conditions), outScope
}

// hideMatchingColumns iterates through each of the columns in cols and
// performs one of the following actions:
// (1) If the column is equal to one of the columns in `joined`, it is skipped
//     since it was one of the merged columns already added to the scope.
// (2) If the column has the same name as one of the columns in `joined` but is
//     not equal, it is marked as hidden and added to the scope.
// (3) All other columns are added to the scope without modification.
func hideMatchingColumns(cols []scopeColumn, joined map[tree.Name]*scopeColumn, scope *scope) {
	for i := range cols {
		col := &cols[i]
		if foundCol, ok := joined[col.name]; ok {
			// Hide other columns with the same name.
			if col == foundCol {
				continue
			}
			col.hidden = true
		}
		scope.cols = append(scope.cols, *col)
	}
}

// constructFilter builds a set of memo groups that represent the given
// list of filter conditions. It returns the top-level memo group ID for the
// filter.
func (b *Builder) constructFilter(conditions []memo.GroupID) memo.GroupID {
	switch len(conditions) {
	case 0:
		return b.factory.ConstructTrue()
	case 1:
		return conditions[0]
	default:
		return b.factory.ConstructAnd(b.factory.InternList(conditions))
	}
}

func (b *Builder) constructJoin(
	joinType sqlbase.JoinType, left, right, filter memo.GroupID,
) memo.GroupID {
	// Wrap the ON condition in a FiltersOp.
	filter = b.factory.ConstructFilters(b.factory.InternList([]memo.GroupID{filter}))
	switch joinType {
	case sqlbase.InnerJoin:
		return b.factory.ConstructInnerJoin(left, right, filter)
	case sqlbase.LeftOuterJoin:
		return b.factory.ConstructLeftJoin(left, right, filter)
	case sqlbase.RightOuterJoin:
		return b.factory.ConstructRightJoin(left, right, filter)
	case sqlbase.FullOuterJoin:
		return b.factory.ConstructFullJoin(left, right, filter)
	default:
		panic(fmt.Errorf("unsupported JOIN type %d", joinType))
	}
}

// findUsingColumn finds the column in cols that has the given name. If the
// column exists it is returned. Otherwise, an error is thrown.
//
// context is a string ("left" or "right") used to indicate in the error
// message whether the name is missing from the left or right side of the join.
func findUsingColumn(cols []scopeColumn, name tree.Name, context string) *scopeColumn {
	for i := range cols {
		col := &cols[i]
		if !col.hidden && col.name == name {
			return col
		}
	}

	panic(builderError{pgerror.NewErrorf(pgerror.CodeUndefinedColumnError,
		"column \"%s\" specified in USING clause does not exist in %s table", name, context)})
}

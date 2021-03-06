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

package xform

import (
	"bytes"
	"fmt"
	"sort"
	"strings"

	"github.com/cockroachdb/cockroach/pkg/sql/opt"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/util/treeprinter"
)

// memoLoc describes the location of an expression in the memo, which is a
// tuple of the expression's memo group and its index within that group.
type memoLoc struct {
	group opt.GroupID
	expr  exprID
}

// makeNormLoc creates a memoLoc that refers to the normalized expression in
// the given group.
func makeNormLoc(group opt.GroupID) memoLoc {
	return memoLoc{group: group, expr: normExprID}
}

// memo is a data structure for efficiently storing a forest of query plans.
// Conceptually, the memo is composed of a numbered set of equivalency classes
// called groups where each group contains a set of logically equivalent
// expressions. The different expressions in a single group are called memo
// expressions (memo-ized expressions). A memo expression has a list of child
// groups as its children rather than a list of individual expressions. The
// forest is composed of every possible combination of parent expression with
// its children, recursively applied.
//
// Memo expressions can be relational (e.g. join) or scalar (e.g. <). Operators
// are always both logical (specify results) and physical (specify results and
// a particular implementation). This means that even a "raw" unoptimized
// expression tree can be executed (naively). Both relational and scalar
// operators are uniformly represented as nodes in memo expression trees, which
// facilitates tree pattern matching and replacement.
//
// Because memo groups contain logically equivalent expressions, all the memo
// expressions in a group share the same logical properties. However, it's
// possible for two logically equivalent expression to be placed in different
// memo groups. This occurs because determining logical equivalency of two
// relational expressions is too complex to perform 100% correctly. A
// correctness failure (i.e. considering two expressions logically equivalent
// when they are not) results in invalid transformations and invalid plans.
// But placing two logically equivalent expressions in different groups has a
// much gentler failure mode: the memo and transformations are less efficient.
// Expressions within the memo may have different physical properties. For
// example, a memo group might contain both hash join and merge join
// expressions which produce the same set of output rows, but produce them in
// different orders.
//
// Expressions are inserted into the memo by the factory, which ensure that
// expressions have been fully normalized before insertion (see the comment in
// factory.go for more details). A new group is created only when unique
// normalized expressions are created by the factory during construction or
// rewrite of the tree. Uniqueness is determined by computing the fingerprint
// for a memo expression, which is simply the expression operator and its list
// of child groups. For example, consider this query:
//
//   SELECT * FROM a, b WHERE a.x = b.x
//
// After insertion into the memo, the memo would contain these six groups:
//
//   6: [inner-join [1 2 5]]
//   5: [eq [3 4]]
//   4: [variable b.x]
//   3: [variable a.x]
//   2: [scan b]
//   1: [scan a]
//
// The fingerprint for the inner-join expression is [inner-join [1 2 5]]. The
// memo maintains a map from expression fingerprint to memo group which allows
// quick determination of whether the normalized form of an expression already
// exists in the memo.
//
// The normalizing factory will never add more than one expression to a memo
// group. But the explorer does add denormalized expressions to existing memo
// groups, since oftentimes one of these equivalent, but denormalized
// expressions will have a lower cost than the initial normalized expression
// added by the factory. For example, the join commutativity transformation
// expands the memo like this:
//
//   6: [inner-join [1 2 5]] [inner-join [2 1 5]]
//   5: [eq [3 4]]
//   4: [variable b.x]
//   3: [variable a.x]
//   2: [scan b]
//   1: [scan a]
//
// TODO(andyk): See the comments in explorer.go for more details.
type memo struct {
	// metadata provides information about the columns and tables used in this
	// particular query.
	metadata *opt.Metadata

	// exprMap maps from expression fingerprint (memoExpr.fingerprint()) to
	// that expression's group. Multiple different fingerprints can map to the
	// same group, but only one of them is the fingerprint of the group's
	// normalized expression.
	exprMap map[fingerprint]opt.GroupID

	// groups is the set of all groups in the memo, indexed by group ID. Note
	// the group ID 0 is invalid in order to allow zero initialization of an
	// expression to indicate that it did not originate from the memo.
	groups []memoGroup

	// logPropsFactory is used to derive logical properties for an expression,
	// based on the logical properties of its children.
	logPropsFactory logicalPropsFactory

	// physPropsFactory is used to derive required physical properties for the
	// children of an expression, based on the required physical properties for
	// the parent.
	physPropsFactory physicalPropsFactory

	// Intern the set of unique physical properties used by expressions in the
	// memo, since there are so many duplicates.
	physPropsMap map[string]opt.PhysicalPropsID
	physProps    []opt.PhysicalProps

	// Some memoExprs have a variable number of children. The memoExpr stores
	// the list as a ListID struct, which is a slice of an array maintained by
	// listStorage. Note that ListID 0 is invalid in order to indicate an
	// unknown list.
	listStorage listStorage

	// Intern the set of unique privates used by expressions in the memo, since
	// there are so many duplicates. Note that PrivateID 0 is invalid in order
	// to indicate an unknown private.
	privatesMap map[interface{}]opt.PrivateID
	privates    []interface{}
}

func newMemo() *memo {
	// NB: group 0 is reserved and intentionally nil so that the 0 group index
	// can indicate that we don't know the group for an expression. Similarly,
	// index 0 for private data, index 0 for physical properties, and index 0
	// for lists are all reserved. In addition, deliberately leave the physical
	// properties for opt.NormPhysPropsID (index 1) uninitialized, since
	// physical should never actually be accessed when traversing the normalized
	// tree.
	m := &memo{
		metadata:     opt.NewMetadata(),
		exprMap:      make(map[fingerprint]opt.GroupID),
		groups:       make([]memoGroup, 1),
		physPropsMap: make(map[string]opt.PhysicalPropsID),
		physProps:    make([]opt.PhysicalProps, 2, 3),
		privatesMap:  make(map[interface{}]opt.PrivateID),
		privates:     make([]interface{}, 1),
	}

	// Intern physical properties that require nothing of operator.
	physProps := opt.PhysicalProps{}
	m.physProps = append(m.physProps, physProps)
	m.physPropsMap[physProps.Fingerprint()] = opt.MinPhysPropsID

	m.listStorage.init()
	return m
}

// newGroup creates a new group and adds it to the memo.
func (m *memo) newGroup(norm memoExpr) *memoGroup {
	id := opt.GroupID(len(m.groups))
	exprs := []memoExpr{norm}
	m.groups = append(m.groups, memoGroup{
		id:    id,
		exprs: exprs,
	})
	return &m.groups[len(m.groups)-1]
}

// addAltFingerprint adds an additional fingerprint that references an existing
// group. The new fingerprint corresponds to a denormalized expression that is
// an alternate form of the group's normalized expression. Adding it to the
// fingerprint map avoids re-adding the same expression in the future.
func (m *memo) addAltFingerprint(alt fingerprint, group opt.GroupID) {
	existing, ok := m.exprMap[alt]
	if ok {
		if existing != group {
			panic("same fingerprint cannot map to different groups")
		}
	} else {
		m.exprMap[alt] = group
	}
}

// memoizeNormExpr enters a normalized expression into the memo. This requires
// the creation of a new memo group with the normalized expression as its first
// expression.
func (m *memo) memoizeNormExpr(norm memoExpr) opt.GroupID {
	if m.exprMap[norm.fingerprint()] != 0 {
		panic("normalized expression has been entered into the memo more than once")
	}

	mgrp := m.newGroup(norm)
	ev := makeExprView(m, mgrp.id, opt.NormPhysPropsID)
	mgrp.logical = m.logPropsFactory.constructProps(ev)

	m.exprMap[norm.fingerprint()] = mgrp.id
	return mgrp.id
}

// lookupGroup returns the memo group for the given ID.
func (m *memo) lookupGroup(group opt.GroupID) *memoGroup {
	return &m.groups[group]
}

// lookupGroupByFingerprint returns the group of the expression that has the
// given fingerprint.
func (m *memo) lookupGroupByFingerprint(f fingerprint) opt.GroupID {
	return m.exprMap[f]
}

// lookupExpr returns the expression referenced by the given location.
func (m *memo) lookupExpr(loc memoLoc) *memoExpr {
	return m.groups[loc.group].lookupExpr(loc.expr)
}

// lookupNormExpr returns the normalized expression for the given group. Each
// group has one canonical expression that is always the first expression in
// the group, and which results from running normalization rules on the
// expression until the final normal state has been reached.
func (m *memo) lookupNormExpr(group opt.GroupID) *memoExpr {
	return m.groups[group].lookupExpr(normExprID)
}

// internList adds the given list of group IDs to memo storage and returns an
// ID that can be used for later lookup. If the same list was added previously,
// this method is a no-op and returns the ID of the previous value.
func (m *memo) internList(items []opt.GroupID) opt.ListID {
	return m.listStorage.intern(items)
}

// lookupList returns a list of group IDs that was earlier stored in the memo
// by a call to internList.
func (m *memo) lookupList(id opt.ListID) []opt.GroupID {
	return m.listStorage.lookup(id)
}

// internPhysicalProps adds the given props to the memo if that set hasn't yet
// been added, and returns an ID which can later be used to look up the props.
// If the same list was added previously, then this method is a no-op and
// returns the same ID as did the previous call.
func (m *memo) internPhysicalProps(props *opt.PhysicalProps) opt.PhysicalPropsID {
	fingerprint := props.Fingerprint()
	id, ok := m.physPropsMap[fingerprint]
	if !ok {
		id = opt.PhysicalPropsID(len(m.physProps))
		m.physProps = append(m.physProps, *props)
		m.physPropsMap[fingerprint] = id
	}
	return id
}

// lookupPhysicalProps returns the set of physical props that was earlier
// interned in the memo by a call to internPhysicalProps.
func (m *memo) lookupPhysicalProps(id opt.PhysicalPropsID) *opt.PhysicalProps {
	return &m.physProps[id]
}

// internPrivate adds the given private value to the memo and returns an ID
// that can be used for later lookup. If the same value was added previously,
// this method is a no-op and returns the ID of the previous value.
// NOTE: Because the internment uses the private value as a map key, only data
//       types which can be map types can be used here.
func (m *memo) internPrivate(private interface{}) opt.PrivateID {
	// Intern the value of certain Datum types rather than a pointer to their
	// value in order to support fast value comparison by private id. This is
	// only possible for Datum types that can be used as map types.
	key := private
	switch t := private.(type) {
	case *tree.DString:
		// Key as a string, so that it compares equal to interned string.
		key = string(*t)
	}

	id, ok := m.privatesMap[key]
	if !ok {
		id = opt.PrivateID(len(m.privates))
		m.privates = append(m.privates, private)
		m.privatesMap[key] = id
	}
	return id
}

// lookupPrivate returns a private value that was earlier interned in the memo
// by a call to internPrivate.
func (m *memo) lookupPrivate(id opt.PrivateID) interface{} {
	return m.privates[id]
}

func (m *memo) String() string {
	tp := treeprinter.New()
	root := tp.Child("memo")

	var buf bytes.Buffer
	for i := len(m.groups) - 1; i > 0; i-- {
		mgrp := &m.groups[i]

		buf.Reset()
		for i := range mgrp.exprs {
			if i != 0 {
				buf.WriteByte(' ')
			}

			// Wrap the memo expr in ExprView to make it easy to get children.
			eid := exprID(i)
			ev := ExprView{
				mem:      m,
				loc:      memoLoc{group: mgrp.id, expr: eid},
				op:       mgrp.exprs[eid].op,
				required: opt.MinPhysPropsID,
			}

			m.formatExpr(ev, &buf, false /* includeRequired */)
		}

		child := root.Childf("%d: %s", i, buf.String())
		m.formatBestExprs(mgrp, child)
	}

	return tp.String()
}

type bestExprSort struct {
	required    opt.PhysicalPropsID
	fingerprint string
	bestExpr    *bestExpr
}

func (m *memo) formatBestExprs(mgrp *memoGroup, tp treeprinter.Node) {
	// Sort the bestExprs by required properties.
	beSort := make([]bestExprSort, 0, len(mgrp.bestExprs))
	mgrp.forEachBestExpr(func(required opt.PhysicalPropsID, best *bestExpr) {
		beSort = append(beSort, bestExprSort{
			required:    required,
			fingerprint: m.lookupPhysicalProps(required).Fingerprint(),
			bestExpr:    best,
		})
	})

	sort.Slice(beSort, func(i, j int) bool {
		return strings.Compare(beSort[i].fingerprint, beSort[j].fingerprint) < 0
	})

	var buf bytes.Buffer
	for _, be := range beSort {
		buf.Reset()

		// Don't show best expressions for scalar groups because they're not too
		// interesting.
		ev := makeExprView(m, mgrp.id, be.required)
		if !ev.IsScalar() {
			child := tp.Childf("\"%s\" [cost=0.0]", be.fingerprint)

			m.formatExpr(ev, &buf, true /* includeRequired */)
			child.Childf("best: %s", buf.String())
		}
	}
}

func (m *memo) formatExpr(ev ExprView, buf *bytes.Buffer, includeRequired bool) {
	fmt.Fprintf(buf, "(%s", ev.Operator())

	private := ev.Private()
	if private != nil {
		switch t := private.(type) {
		case nil:
		case *opt.ScanOpDef:
			fmt.Fprintf(buf, " %s", m.metadata.Table(t.Table).TabName())
		case opt.ColumnIndex:
			fmt.Fprintf(buf, " %s", m.metadata.ColumnLabel(t))
		case *opt.ColSet, *opt.ColMap, *opt.ColList:
			// Don't show anything, because it's mostly redundant.
		default:
			fmt.Fprintf(buf, " %s", private)
		}
	}

	if ev.ChildCount() > 0 {
		for i := 0; i < ev.ChildCount(); i++ {
			child := ev.ChildGroup(i)
			fmt.Fprintf(buf, " %d", child)

			if !includeRequired {
				// Print properties required of the child if they are interesting.
				required := m.physPropsFactory.constructChildProps(ev, i)
				if required != opt.MinPhysPropsID {
					fmt.Fprintf(buf, "=\"%s\"", m.lookupPhysicalProps(required).Fingerprint())
				}
			}
		}
	}

	buf.WriteString(")")
}

// Package catalog keeps the definitions of tables and indexes in the database
// file. Page 0 is the first page of a heap of catalog rows, each of one fixed
// schema, with a kind column that says what the row describes.
package catalog

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"

	"github.com/wilfex81/DB_Engine/internal/buffer"
	"github.com/wilfex81/DB_Engine/internal/index"
	"github.com/wilfex81/DB_Engine/internal/storage"
	"github.com/wilfex81/DB_Engine/internal/table"
	"github.com/wilfex81/DB_Engine/internal/types"
)

var (
	ErrNotEmpty       = errors.New("catalog: the file is not empty")
	ErrCorrupt        = errors.New("catalog: corrupt catalog")
	ErrInvalidName    = errors.New("catalog: invalid name")
	ErrTableExists    = errors.New("catalog: table already exists")
	ErrTableNotFound  = errors.New("catalog: table not found")
	ErrIndexExists    = errors.New("catalog: index already exists")
	ErrIndexNotFound  = errors.New("catalog: index not found")
	ErrColumnNotFound = errors.New("catalog: column not found")
	ErrNotIndexable   = errors.New("catalog: only INT columns can be indexed")
)

const maxNameLen = 64

// Row kinds.
const (
	kindTable  = 1
	kindColumn = 2
	kindIndex  = 3
)

// The schema of a catalog row. These bytes end up in database files.
//
//	kind  1 table, 2 column, 3 index
//	tbl   the table the row belongs to
//	name  table name, column name, or index name
//	ref   index: the indexed column
//	pos   column: its position, from 0
//	typ   column: its types.Type
//	nn    column: NOT NULL
//	page  table: first page of its heap. index: meta page of its tree
var rowSchema = func() *types.Schema {
	s, err := types.NewSchema([]types.Column{
		{Name: "kind", Type: types.Int, NotNull: true},
		{Name: "tbl", Type: types.Text, NotNull: true},
		{Name: "name", Type: types.Text, NotNull: true},
		{Name: "ref", Type: types.Text},
		{Name: "pos", Type: types.Int},
		{Name: "typ", Type: types.Int},
		{Name: "nn", Type: types.Bool},
		{Name: "page", Type: types.Int},
	})
	if err != nil {
		panic(err)
	}
	return s
}()

func tableRow(name string, page storage.PageID) []types.Value {
	return []types.Value{
		types.NewInt(kindTable), types.NewText(name), types.NewText(name),
		types.Null(types.Text), types.Null(types.Int), types.Null(types.Int), types.Null(types.Bool),
		types.NewInt(int64(page)),
	}
}

func columnRow(tbl string, c types.Column, pos int) []types.Value {
	return []types.Value{
		types.NewInt(kindColumn), types.NewText(tbl), types.NewText(c.Name),
		types.Null(types.Text), types.NewInt(int64(pos)), types.NewInt(int64(c.Type)), types.NewBool(c.NotNull),
		types.Null(types.Int),
	}
}

func indexRow(tbl, name, column string, meta storage.PageID) []types.Value {
	return []types.Value{
		types.NewInt(kindIndex), types.NewText(tbl), types.NewText(name),
		types.NewText(column), types.Null(types.Int), types.Null(types.Int), types.Null(types.Bool),
		types.NewInt(int64(meta)),
	}
}

// Catalog is the set of tables and indexes of one database file. It is safe
// for concurrent use.
type Catalog struct {
	mu      sync.RWMutex
	pool    *buffer.Pool
	rows    *table.Heap
	tables  map[string]*Table // by lower case name
	indexes map[string]*Table // lower case index name to the table it belongs to
}

// Table is a table definition and the heap that stores its rows.
type Table struct {
	Name   string
	Schema *types.Schema
	Heap   *table.Heap

	cat     *Catalog
	rid     table.RID   // the table's own catalog row
	rids    []table.RID // the catalog rows of its columns
	indexes []*Index    // guarded by cat.mu
}

// Index is a unique B+ tree over one INT column of a table.
type Index struct {
	Name      string
	Column    string
	ColumnPos int
	Tree      *index.Tree

	rid table.RID
}

func newCatalog(pool *buffer.Pool, rows *table.Heap) *Catalog {
	return &Catalog{
		pool:    pool,
		rows:    rows,
		tables:  map[string]*Table{},
		indexes: map[string]*Table{},
	}
}

// Create starts a catalog in an empty file. Its first page becomes page 0.
func Create(pool *buffer.Pool) (*Catalog, error) {
	if n := pool.NumPages(); n != 0 {
		return nil, fmt.Errorf("%w: it has %d pages", ErrNotEmpty, n)
	}
	rows, err := table.Create(pool, rowSchema)
	if err != nil {
		return nil, err
	}
	if rows.FirstPage() != 0 {
		return nil, fmt.Errorf("%w: the catalog heap starts at page %d, not 0", ErrCorrupt, rows.FirstPage())
	}
	return newCatalog(pool, rows), nil
}

// Open reads the catalog of an existing file. Rows left behind by a crash in
// the middle of a CreateTable or DropTable are removed.
func Open(pool *buffer.Pool) (*Catalog, error) {
	rows, err := table.Open(pool, rowSchema, 0)
	if err != nil {
		return nil, fmt.Errorf("catalog: open: %w", err)
	}
	c := newCatalog(pool, rows)
	if err := c.load(); err != nil {
		return nil, err
	}
	return c, nil
}

type entry struct {
	rid  table.RID
	vals []types.Value
}

func (c *Catalog) load() error {
	var tables []entry
	cols := map[string][]entry{}
	idxs := map[string][]entry{}

	it := c.rows.Scan()
	for it.Next() {
		e := entry{it.RID(), it.Values()}
		key := strings.ToLower(e.vals[1].Text())
		switch kind := e.vals[0].Int(); kind {
		case kindTable:
			tables = append(tables, e)
		case kindColumn:
			cols[key] = append(cols[key], e)
		case kindIndex:
			idxs[key] = append(idxs[key], e)
		default:
			return fmt.Errorf("%w: unknown row kind %d at %s", ErrCorrupt, kind, e.rid)
		}
	}
	if err := it.Err(); err != nil {
		return fmt.Errorf("catalog: read: %w", err)
	}

	for _, te := range tables {
		key := strings.ToLower(te.vals[1].Text())
		if _, dup := c.tables[key]; dup {
			return fmt.Errorf("%w: table %q is defined twice", ErrCorrupt, te.vals[1].Text())
		}
		t, err := c.buildTable(te, cols[key], idxs[key])
		if err != nil {
			return err
		}
		c.tables[key] = t
		delete(cols, key)
		delete(idxs, key)
	}

	// What is left belongs to no table: a CreateTable that crashed before it
	// wrote the table row, or a DropTable that crashed after it removed it.
	for _, group := range cols {
		for _, e := range group {
			if err := c.rows.Delete(e.rid); err != nil {
				return fmt.Errorf("catalog: remove an orphan row: %w", err)
			}
		}
	}
	for _, group := range idxs {
		for _, e := range group {
			if err := c.rows.Delete(e.rid); err != nil {
				return fmt.Errorf("catalog: remove an orphan row: %w", err)
			}
		}
	}
	return nil
}

func intField(e entry, i int, what string) (int64, error) {
	v := e.vals[i]
	if v.IsNull() {
		return 0, fmt.Errorf("%w: %s is NULL at %s", ErrCorrupt, what, e.rid)
	}
	return v.Int(), nil
}

func pageField(e entry, i int, what string) (storage.PageID, error) {
	v, err := intField(e, i, what)
	if err != nil {
		return 0, err
	}
	if v < 0 || v > math.MaxUint32 {
		return 0, fmt.Errorf("%w: %s %d is out of range at %s", ErrCorrupt, what, v, e.rid)
	}
	return storage.PageID(v), nil
}

func (c *Catalog) buildTable(te entry, colRows, idxRows []entry) (*Table, error) {
	name := te.vals[1].Text()
	page, err := pageField(te, 7, "heap page")
	if err != nil {
		return nil, err
	}

	type numbered struct {
		pos int64
		e   entry
	}
	list := make([]numbered, 0, len(colRows))
	for _, e := range colRows {
		pos, err := intField(e, 4, "column position")
		if err != nil {
			return nil, err
		}
		list = append(list, numbered{pos, e})
	}
	sort.Slice(list, func(a, b int) bool { return list[a].pos < list[b].pos })

	defs := make([]types.Column, len(list))
	rids := make([]table.RID, len(list))
	for i, n := range list {
		if n.pos != int64(i) {
			return nil, fmt.Errorf("%w: the columns of table %q are not numbered from 0 without gaps", ErrCorrupt, name)
		}
		typ, err := intField(n.e, 5, "column type")
		if err != nil {
			return nil, err
		}
		if typ < 0 || typ > math.MaxUint8 {
			return nil, fmt.Errorf("%w: column type %d is out of range at %s", ErrCorrupt, typ, n.e.rid)
		}
		if n.e.vals[6].IsNull() {
			return nil, fmt.Errorf("%w: the NOT NULL flag is NULL at %s", ErrCorrupt, n.e.rid)
		}
		defs[i] = types.Column{Name: n.e.vals[2].Text(), Type: types.Type(typ), NotNull: n.e.vals[6].Bool()}
		rids[i] = n.e.rid
	}
	schema, err := types.NewSchema(defs)
	if err != nil {
		return nil, fmt.Errorf("%w: table %q: %v", ErrCorrupt, name, err)
	}
	heap, err := table.Open(c.pool, schema, page)
	if err != nil {
		return nil, fmt.Errorf("catalog: table %q: %w", name, err)
	}
	t := &Table{Name: name, Schema: schema, Heap: heap, cat: c, rid: te.rid, rids: rids}

	sort.Slice(idxRows, func(a, b int) bool {
		return strings.ToLower(idxRows[a].vals[2].Text()) < strings.ToLower(idxRows[b].vals[2].Text())
	})
	for _, e := range idxRows {
		iname := e.vals[2].Text()
		ikey := strings.ToLower(iname)
		if _, dup := c.indexes[ikey]; dup {
			return nil, fmt.Errorf("%w: index %q is defined twice", ErrCorrupt, iname)
		}
		if e.vals[3].IsNull() {
			return nil, fmt.Errorf("%w: index %q has no column", ErrCorrupt, iname)
		}
		pos, ok := schema.Index(e.vals[3].Text())
		if !ok {
			return nil, fmt.Errorf("%w: index %q is on a column that does not exist", ErrCorrupt, iname)
		}
		col := schema.Column(pos)
		if col.Type != types.Int {
			return nil, fmt.Errorf("%w: index %q is on a %s column", ErrCorrupt, iname, col.Type)
		}
		meta, err := pageField(e, 7, "index meta page")
		if err != nil {
			return nil, err
		}
		tree, err := index.Open(c.pool, meta)
		if err != nil {
			return nil, fmt.Errorf("catalog: index %q: %w", iname, err)
		}
		t.indexes = append(t.indexes, &Index{Name: iname, Column: col.Name, ColumnPos: pos, Tree: tree, rid: e.rid})
		c.indexes[ikey] = t
	}
	return t, nil
}

// validName allows letters, digits and underscores, starting with a letter or
// an underscore, up to 64 bytes.
func validName(s string) bool {
	if len(s) == 0 || len(s) > maxNameLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '_', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// CreateTable adds a table with an empty heap. Names are not case sensitive.
func (c *Catalog) CreateTable(name string, cols []types.Column) (*Table, error) {
	if !validName(name) {
		return nil, fmt.Errorf("%w: table name %q", ErrInvalidName, name)
	}
	for _, col := range cols {
		if !validName(col.Name) {
			return nil, fmt.Errorf("%w: column name %q", ErrInvalidName, col.Name)
		}
	}
	schema, err := types.NewSchema(cols)
	if err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	key := strings.ToLower(name)
	if _, ok := c.tables[key]; ok {
		return nil, fmt.Errorf("%w: %q", ErrTableExists, name)
	}

	heap, err := table.Create(c.pool, schema)
	if err != nil {
		return nil, err
	}
	t := &Table{Name: name, Schema: schema, Heap: heap, cat: c}

	// The table row goes last: it is what makes the table exist.
	for i, col := range schema.Columns() {
		rid, err := c.rows.Insert(columnRow(name, col, i))
		if err != nil {
			c.undo(t.rids)
			return nil, err
		}
		t.rids = append(t.rids, rid)
	}
	t.rid, err = c.rows.Insert(tableRow(name, heap.FirstPage()))
	if err != nil {
		c.undo(t.rids)
		return nil, err
	}
	c.tables[key] = t
	return t, nil
}

// undo removes catalog rows after a failure. It is best effort: Open removes
// whatever is left.
func (c *Catalog) undo(rids []table.RID) {
	for _, rid := range rids {
		_ = c.rows.Delete(rid)
	}
}

// DropTable removes a table and its indexes. Their pages are not reused, because
// the file has no free page list yet.
func (c *Catalog) DropTable(name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	key := strings.ToLower(name)
	t, ok := c.tables[key]
	if !ok {
		return fmt.Errorf("%w: %q", ErrTableNotFound, name)
	}
	// The table row goes first: from then on the table is gone, and a crash
	// leaves only orphan rows that Open removes.
	if err := c.rows.Delete(t.rid); err != nil {
		return err
	}
	delete(c.tables, key)

	var first error
	remove := func(rid table.RID) {
		if err := c.rows.Delete(rid); err != nil && first == nil {
			first = err
		}
	}
	for _, ix := range t.indexes {
		delete(c.indexes, strings.ToLower(ix.Name))
		remove(ix.rid)
	}
	for _, rid := range t.rids {
		remove(rid)
	}
	return first
}

// CreateIndex adds a unique index on an INT column. The tree starts empty:
// filling it with the rows that are already in the table is up to the caller.
// Index names are unique in the whole database.
func (c *Catalog) CreateIndex(tableName, indexName, column string) (*Index, error) {
	if !validName(indexName) {
		return nil, fmt.Errorf("%w: index name %q", ErrInvalidName, indexName)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.tables[strings.ToLower(tableName)]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrTableNotFound, tableName)
	}
	ikey := strings.ToLower(indexName)
	if _, ok := c.indexes[ikey]; ok {
		return nil, fmt.Errorf("%w: %q", ErrIndexExists, indexName)
	}
	pos, ok := t.Schema.Index(column)
	if !ok {
		return nil, fmt.Errorf("%w: %q in table %q", ErrColumnNotFound, column, t.Name)
	}
	col := t.Schema.Column(pos)
	if col.Type != types.Int {
		return nil, fmt.Errorf("%w: column %q is %s", ErrNotIndexable, col.Name, col.Type)
	}

	tree, err := index.Create(c.pool)
	if err != nil {
		return nil, err
	}
	rid, err := c.rows.Insert(indexRow(t.Name, indexName, col.Name, tree.MetaPage()))
	if err != nil {
		return nil, err
	}
	ix := &Index{Name: indexName, Column: col.Name, ColumnPos: pos, Tree: tree, rid: rid}
	t.indexes = append(t.indexes, ix)
	c.indexes[ikey] = t
	return ix, nil
}

// DropIndex removes an index. Its pages are not reused.
func (c *Catalog) DropIndex(name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	key := strings.ToLower(name)
	t, ok := c.indexes[key]
	if !ok {
		return fmt.Errorf("%w: %q", ErrIndexNotFound, name)
	}
	for i, ix := range t.indexes {
		if strings.ToLower(ix.Name) != key {
			continue
		}
		if err := c.rows.Delete(ix.rid); err != nil {
			return err
		}
		t.indexes = append(t.indexes[:i:i], t.indexes[i+1:]...)
		delete(c.indexes, key)
		return nil
	}
	return fmt.Errorf("%w: %q is listed but missing from its table", ErrCorrupt, name)
}

// Table finds a table by name, ignoring case.
func (c *Catalog) Table(name string) (*Table, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	t, ok := c.tables[strings.ToLower(name)]
	return t, ok
}

// Tables lists the table names, sorted ignoring case.
func (c *Catalog) Tables() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	keys := make([]string, 0, len(c.tables))
	for k := range c.tables {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	names := make([]string, len(keys))
	for i, k := range keys {
		names[i] = c.tables[k].Name
	}
	return names
}

// Index finds an index by name, ignoring case.
func (c *Catalog) Index(name string) (*Index, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	key := strings.ToLower(name)
	t, ok := c.indexes[key]
	if !ok {
		return nil, false
	}
	for _, ix := range t.indexes {
		if strings.ToLower(ix.Name) == key {
			return ix, true
		}
	}
	return nil, false
}

// Indexes returns the indexes of the table, sorted by name.
func (t *Table) Indexes() []*Index {
	t.cat.mu.RLock()
	defer t.cat.mu.RUnlock()
	out := append([]*Index(nil), t.indexes...)
	sort.Slice(out, func(a, b int) bool {
		return strings.ToLower(out[a].Name) < strings.ToLower(out[b].Name)
	})
	return out
}

// IndexOn returns an index over the given column, ignoring case.
func (t *Table) IndexOn(column string) (*Index, bool) {
	for _, ix := range t.Indexes() {
		if strings.EqualFold(ix.Column, column) {
			return ix, true
		}
	}
	return nil, false
}

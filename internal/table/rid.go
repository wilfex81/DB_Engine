// stores the rows of one table in a chain of pages
package table

import (
	"fmt"

	"github.com/wilfex81/DB_Engine/internal/storage"
)

// RID is the address of a row: a page and a slot in that page. It can be
// compared with == and used as a map key
type RID struct {
	Page storage.PageID
	Slot uint16
}

func (r RID) String() string { return fmt.Sprintf("(%d,%d)", r.Page, r.Slot) }

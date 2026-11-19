package storage

import (
	"errors"
	"fmt"
	"math"
	"os"
	"sync"
)

type PageID uint32

var (
	ErrPageNotFound = errors.New("storage: page does not exist")
	ErrClosed       = errors.New("storage: disk manager is closed")
)

// Disk manage reads and writes whole page of one file. It is safe for
// concurrent use. Reads and writes of different pages run in parallel, and
// allocation is exclusive. Two writers of the same page must be ordered by the caller, which
// will be the buffer
type DiskManager struct {
	mu       sync.RWMutex
	f        *os.File
	numPages uint32
	closed   bool
}

// OpenDiskManager opens the file, creating it if needed. A file whose size is
// not a whole number of pages was cut short, so it is reported as corrupt.
func OpenDiskManager(path string) (*DiskManager, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	size := st.Size()
	if size%PageSize != 0 {
		f.Close()
		return nil, fmt.Errorf("%w: file size %d is not a multiple of %d", ErrCorrupt, size, PageSize)
	}
	pages := size / PageSize
	if pages >= math.MaxUint32 {
		f.Close()
		return nil, fmt.Errorf("storage: file has too many pages (%d)", pages)
	}
	return &DiskManager{f: f, numPages: uint32(pages)}, nil
}

// No. of pages in the file
func (d *DiskManager) NumPages() int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return int(d.numPages)
}

// Reads page id into p and validates it.
// if it returns an error, the contents of p are undefined
func (d *DiskManager) ReadPage(id PageID, p *Page) error {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if err := d.check(id); err != nil {
		return err
	}
	n, err := d.f.ReadAt(p[:], offset(id))
	if n != PageSize {
		return fmt.Errorf("storage: read page %d: %w", id, err)
	}
	if err := p.Validate(); err != nil {
		return fmt.Errorf("page %d: %w", id, err)
	}
	return nil
}

// validates p and writes it as page id. A bad page is refused, so
// a bug in the code above can never put garbage on disk silently
func (d *DiskManager) WritePage(id PageID, p *Page) error {
	if err := p.Validate(); err != nil {
		return fmt.Errorf("refusing to write page %d: %w", id, err)
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	if err := d.check(id); err != nil {
		return err
	}
	if _, err := d.f.WriteAt(p[:], offset(id)); err != nil {
		return fmt.Errorf("storage: write page %d: %w", id, err)
	}
	return nil

}

// Appends an empty page to the file and returns its number.
// If the write fails, the file is cut back, so the page count stays right.

func (d *DiskManager) AllocatePage() (PageID, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return 0, ErrClosed
	}
	if d.numPages == math.MaxUint32 {
		return 0, errors.New("storage: file is full")
	}

	id := PageID(d.numPages)
	var p Page
	p.Init()
	if _, err := d.f.WriteAt(p[:], offset(id)); err != nil {
		_ = d.f.Truncate(offset(id))
		return 0, fmt.Errorf("storage: allocate page %d: %w", id, err)
	}
	d.numPages++
	return id, nil
}

// sync fllushes the file to stable storage
func (d *DiskManager) Sync() error {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.closed {
		return ErrClosed
	}
	return d.f.Sync()
}

// Close syncs and closes the file. Using the manager afterwards returns ErrClosed.
func (d *DiskManager) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return ErrClosed
	}
	d.closed = true
	syncErr := d.f.Sync()
	if err := d.f.Close(); err != nil {
		return err
	}
	return syncErr
}

// must be called with the lock held.
func (d *DiskManager) check(id PageID) error {
	if d.closed {
		return ErrClosed
	}
	if uint32(id) >= d.numPages {
		return fmt.Errorf("%w: %d (the file has %d)", ErrPageNotFound, id, d.numPages)
	}
	return nil
}

func offset(id PageID) int64 { return int64(id) * PageSize }

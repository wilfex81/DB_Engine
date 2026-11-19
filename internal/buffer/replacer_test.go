package buffer

import (
	"math/rand"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type step struct {
	kind string // "unpin", "pin", "victim" (expects id), "none" (expects no victim)
	id   FrameID
}

func unpin(id FrameID) step  { return step{"unpin", id} }
func pin(id FrameID) step    { return step{"pin", id} }
func victim(id FrameID) step { return step{"victim", id} }
func none() step             { return step{kind: "none"} }

func TestReplacerSequences(t *testing.T) {
	tests := []struct {
		name     string
		steps    []step
		wantSize int
	}{
		{"oldest goes first", []step{unpin(0), unpin(1), unpin(2), victim(0), victim(1), victim(2), none()}, 0},
		{"empty has no victim", []step{none()}, 0},
		{"pin removes a frame", []step{unpin(0), unpin(1), unpin(2), pin(0), victim(1), victim(2), none()}, 0},
		{"unpin twice keeps the place", []step{unpin(0), unpin(1), unpin(0), victim(0), victim(1)}, 0},
		{"pin then unpin makes it newest", []step{unpin(0), unpin(1), pin(0), unpin(0), victim(1), victim(0)}, 0},
		{"pin of an untracked frame is fine", []step{pin(3), none()}, 0},
		{"a victim can be unpinned again", []step{unpin(0), victim(0), none(), unpin(0), victim(0)}, 0},
		{"frames left over", []step{unpin(0), unpin(1), unpin(2), victim(0)}, 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := NewLRUReplacer(4)
			for i, s := range tc.steps {
				switch s.kind {
				case "unpin":
					r.Unpin(s.id)
				case "pin":
					r.Pin(s.id)
				case "victim":
					got, ok := r.Victim()
					require.True(t, ok, "step %d", i)
					require.Equal(t, s.id, got, "step %d", i)
				case "none":
					_, ok := r.Victim()
					require.False(t, ok, "step %d", i)
				}
			}
			assert.Equal(t, tc.wantSize, r.Size())
		})
	}
}

func TestReplacerRejectsBadFrames(t *testing.T) {
	r := NewLRUReplacer(4)
	assert.Panics(t, func() { NewLRUReplacer(0) })
	assert.Panics(t, func() { r.Pin(-1) })
	assert.Panics(t, func() { r.Pin(4) })
	assert.Panics(t, func() { r.Unpin(-1) })
	assert.Panics(t, func() { r.Unpin(4) })
	assert.NotPanics(t, func() { r.Unpin(3) })
}

func TestEvictableOrder(t *testing.T) {
	r := NewLRUReplacer(5)
	for _, id := range []FrameID{3, 1, 4, 0} {
		r.Unpin(id)
	}
	r.Pin(1)
	assert.Equal(t, []FrameID{3, 4, 0}, r.Evictable())
}

// Random operations, checked against a plain slice after every step.
func TestReplacerAgainstAModel(t *testing.T) {
	const frames = 10
	rng := rand.New(rand.NewSource(1))
	r := NewLRUReplacer(frames)
	var model []FrameID // oldest first

	indexOf := func(id FrameID) int {
		for i, m := range model {
			if m == id {
				return i
			}
		}
		return -1
	}

	for step := 0; step < 20000; step++ {
		id := FrameID(rng.Intn(frames))
		switch rng.Intn(3) {
		case 0:
			r.Unpin(id)
			if indexOf(id) < 0 {
				model = append(model, id)
			}
		case 1:
			r.Pin(id)
			if i := indexOf(id); i >= 0 {
				model = append(model[:i], model[i+1:]...)
			}
		case 2:
			got, ok := r.Victim()
			if len(model) == 0 {
				require.False(t, ok, "step %d", step)
				break
			}
			require.True(t, ok, "step %d", step)
			require.Equal(t, model[0], got, "step %d", step)
			model = model[1:]
		}
		require.Equal(t, len(model), r.Size(), "step %d", step)
		require.Equal(t, append([]FrameID{}, model...), r.Evictable(), "step %d", step)
	}
}

// run with -race
func TestReplacerConcurrentUse(t *testing.T) {
	const frames, workers, ops = 16, 8, 2000
	r := NewLRUReplacer(frames)

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(seed))
			for i := 0; i < ops; i++ {
				id := FrameID(rng.Intn(frames))
				switch rng.Intn(3) {
				case 0:
					r.Unpin(id)
				case 1:
					r.Pin(id)
				case 2:
					r.Victim()
				}
			}
		}(int64(w))
	}
	wg.Wait()

	ev := r.Evictable()
	assert.Equal(t, r.Size(), len(ev))
	seen := map[FrameID]bool{}
	for _, id := range ev {
		assert.False(t, seen[id], "frame %d listed twice", id)
		seen[id] = true
	}
}

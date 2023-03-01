package walker

import (
	"context"
	"sync"

	"github.com/moby/buildkit/client/llb"
	solverpb "github.com/moby/buildkit/solver/pb"
	"github.com/pkg/errors"
	"golang.org/x/sync/errgroup"
)

// BreakContext contains information about the current breakpoint
type BreakContext struct {
	// State is the current LLB state
	State llb.State

	// Cursors are current cursor locations
	Cursors []solverpb.Range // walker should pass the hit range to the hander.

	// Hits are all breakpoints hit.
	Hits map[string][]*solverpb.Range // walker should pass this to the handler.
}

// BreakHandlerFunc is a callback function to be call on each break
type BreakHandlerFunc func(ctx context.Context, bCtx *BreakContext) error

// Walker walks an LLB tree from the leaves to the root. Can be controlled using breakpoints.
type Walker struct {
	breakHandler BreakHandlerFunc
	breakpoints  *Breakpoints
	closed       bool
	cursors      map[solverpb.Range]int
	cursorsMu    sync.Mutex

	mu sync.Mutex

	onVertexHandler func(st llb.State) error
}

// NewWalker returns a walker configured with the breakpoints and breakpoint handler.
// onVertexHandlerFunc is called on each vertex including non breakpoints.
func NewWalker(bps *Breakpoints, breakHandler BreakHandlerFunc, onVertexHandlerFunc func(st llb.State) error) *Walker {
	bps.ForEach(func(key string, bp Breakpoint) bool {
		bp.Init()
		return true
	})
	return &Walker{
		breakHandler:    breakHandler,
		breakpoints:     bps,
		onVertexHandler: onVertexHandlerFunc,
	}
}

// Close closes the walker.
func (w *Walker) Close() {
	w.closed = true
	w.breakpoints = nil
}

// GetCursors returns positions where the walker is currently looking at.
func (w *Walker) GetCursors() (res []solverpb.Range) {
	w.cursorsMu.Lock()
	defer w.cursorsMu.Unlock()
	for r, i := range w.cursors {
		if i > 0 {
			res = append(res, r)
		}
	}
	return
}

// BreakAllNode configures whether the walker breaks on each vertex.
func (w *Walker) BreakAllNode(v bool) {
	w.breakpoints.BreakAllNode(v)
}

// Walk starts walking the specified LLB from the leaves to the root.
func (w *Walker) Walk(ctx context.Context, st llb.State) error {
	return w.walk(ctx, st, 0)
}

func (w *Walker) walk(ctx context.Context, st llb.State, depth int) error {
	eg, egCtx := errgroup.WithContext(ctx)
	for _, o := range st.Output().Vertex(ctx, nil).Inputs() {
		o := o
		eg.Go(func() error {
			return w.walk(egCtx, llb.NewState(o), depth+1)
		})
	}
	if err := eg.Wait(); err != nil {
		return err
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	def, err := st.Marshal(ctx)
	if err != nil {
		return err
	}
	dgst, _, _, _, err := st.Output().Vertex(ctx, nil).Marshal(ctx, nil)
	if err != nil {
		return err
	}
	var ranges []solverpb.Range
	if def.Source != nil {
		if locs, ok := def.Source.Locations[dgst.String()]; ok {
			for _, loc := range locs.Locations {
				for _, r := range loc.Ranges {
					ranges = append(ranges, *r)
				}
			}
		}
	}
	w.addCursor(ranges)
	defer func() {
		w.removeCursor(ranges)

	}()
	if w.closed {
		return errors.Errorf("walker closed")
	}
	handleErr := w.onVertexHandler(st)
	isBreak, hits, err := w.breakpoints.isBreakpoint(ctx, st, handleErr)
	if err != nil {
		return err

	}
	if isBreak {
		err = w.breakHandler(ctx, &BreakContext{
			State:   st,
			Cursors: w.GetCursors(),
			Hits:    hits,
		})
	}
	if err != nil {
		return err
	}
	return handleErr
}

func (w *Walker) addCursor(ranges []solverpb.Range) {
	w.cursorsMu.Lock()
	defer w.cursorsMu.Unlock()
	for _, r := range ranges {
		if w.cursors == nil {
			w.cursors = make(map[solverpb.Range]int)
		}
		w.cursors[r]++
	}
}

func (w *Walker) removeCursor(ranges []solverpb.Range) {
	w.cursorsMu.Lock()
	defer w.cursorsMu.Unlock()
	if w.cursors == nil {
		return
	}
	for _, r := range ranges {
		w.cursors[r] = w.cursors[r] - 1
		if w.cursors[r] == 0 {
			delete(w.cursors, r)
		}
	}
}

// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

package ssa

import (
	"sync/atomic"
)

// Each task has states: it is initially "active", and transitions to "done".
type task struct {
	done       chan unit      // close when the task is done.
	edges      map[*task]unit // set of predecessors of this task.
	transitive atomic.Bool    // true once it is known all predecessors are done.
}

func (x *task) isTransitivelyDone() bool { return x == nil || x.transitive.Load() }

// addEdge creates an edge from x to y, indicating that
// x.wait() will not return before y is done.
// All calls to x.addEdge(...) should happen before x.markDone().
func (x *task) addEdge(y *task) {
	if x == y || y.isTransitivelyDone() {
		return // no work remaining
	}

	// heuristic done check
	select {
	case <-x.done:
		panic("cannot add an edge to a done task")
	default:
	}

	if x.edges == nil {
		x.edges = make(map[*task]unit)
	}
	x.edges[y] = unit{}
}

// markDone changes the task's state to markDone.
func (x *task) markDone() {
	if x != nil {
		close(x.done)
	}
}

// wait blocks until x and all the tasks it can reach through edges are done.
func (x *task) wait() {
	if x.isTransitivelyDone() {
		return // already known to be done. Skip allocations.
	}

	// Use BFS to wait on u.done to be closed, for all u transitively reachable from x via edges.
	work := []*task{x}
	enqueued := map[*task]unit{x: {}}
	for i := 0; i < len(work); i++ {
		u := work[i]
		if u.isTransitivelyDone() { // already transitively done
			work[i] = nil
			continue
		}
		<-u.done // wait for u to be marked done.

		for v := range u.edges {
			if _, ok := enqueued[v]; !ok {
				enqueued[v] = unit{}
				work = append(work, v)
			}
		}
	}

	// work is transitively closed over dependencies. u in work is done (or
	// transitively done and skipped). u is transitively done.
	for _, u := range work {
		if u != nil {
			x.transitive.Store(true)
		}
	}
}

/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 * SPDX-License-Identifier: BUSL-1.1
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

// pool.go parallelizes CSR's two matvec directions across a small set of
// persistent goroutines (perf Prompt, step 2). CSR stores Phi^T row-major
// (n series rows, m bucket columns; see csr.go's header comment), so the
// two directions need different splitting strategies to stay race-free
// with zero per-call allocation:
//
//   - MulInto (out = Phi^T @ x, a per-row gather): output is indexed by
//     row, so splitting n rows across workers is embarrassingly parallel —
//     each worker owns a disjoint output range and reads the (shared,
//     read-only) input, needing no precomputed structure.
//   - MulTransposeInto (out = Phi @ x, a per-row scatter into m bucket
//     columns): output is indexed by column, and any row can scatter into
//     any column, so splitting by row would race on shared output
//     columns. Instead each worker owns a disjoint COLUMN range and a
//     precomputed column-filtered copy of the CSR's entries (built once
//     per CSR/solve, reused for every MulTransposeInto call the solve
//     makes: FISTA's iteration loop, the Lipschitz power iteration, and
//     Group-OMP's residual scan all share it).
//
// Dispatch is a blocking channel handoff, not a busy-spin: FISTA's loop
// spends real time on serial work (softThreshold, restart bookkeeping,
// residual computation — each an O(n) or O(m) pass) between matvec calls,
// and this machine has 8 physical cores under 16 logical (SMT) threads.
// Two busy-spin designs were measured and rejected: an unbounded spin
// left (nWorkers) threads hammering the CPU through every serial section
// too, competing with the dispatching goroutine's own work for physical
// execution ports and making full solves 8-13x *slower*; a bounded spin
// with a runtime.Gosched() fallback avoided that but triggered constant
// scheduler churn (runtime.gosched_m dominated the profile) because
// Gosched has nothing else to hand the P to — every other goroutine is
// either also spinning or the dispatcher itself. Blocking channels let
// idle workers truly sleep (no CPU stolen from the serial sections) at
// the cost of a real wake per round; nWorkers is kept small (see
// newMatvecPool) to keep the number of wakes per solve bounded.
package recover

import (
	"runtime"
	"sync"
)

// minParallelNNZ is the nonzero-count threshold below which newMatvecPool
// stays single-threaded: below this, goroutine dispatch overhead exceeds
// the serial matvec cost.
const minParallelNNZ = 20000

// maxWorkers caps parallelism well below GOMAXPROCS. FISTA dispatches
// roughly a thousand matvec calls per solve, each a blocking wake/sleep
// round-trip per worker; on this project's target (a solve embedded
// alongside other work, not a dedicated benchmark host — ADR-007), more
// workers means more wake overhead and more contention for the serial
// sections between dispatches, for a shrinking marginal return past a
// handful of cores. Measured empirically (see PERF.md): worker counts
// above this stopped paying for their own overhead.
const maxWorkers = 3

// csrBlock is one worker's slice of MulTransposeInto's entries: only the
// (col, val) pairs whose col falls in this worker's owned column range,
// kept row-major (rowPtr indexed by the CSR's original row number) so a
// worker's scatter loop looks exactly like CSR.MulTransposeInto's, just
// over a filtered subset.
type csrBlock struct {
	rowPtr []int32
	colIdx []int32
	vals   []float32
}

type poolOp int8

const (
	opGather  poolOp = iota // MulInto:          out[i]   = row i . x
	opScatter               // MulTransposeInto: out[col] += ... (per worker's column block)
)

// matvecPool parallelizes one CSR's MulInto/MulTransposeInto across a
// small set of persistent goroutines. Build with newMatvecPool and Close
// it when the solve finishes. Not safe for concurrent MulInto/
// MulTransposeInto/Close calls — one Recover/RecoverGroup call drives it
// sequentially, matching fista's single-threaded iteration loop.
type matvecPool struct {
	csr      *CSR
	nWorkers int

	rowLo, rowHi []int      // per-worker row range, for MulInto
	blocks       []csrBlock // per-worker column block, for MulTransposeInto

	// Round state: written by the dispatching goroutine strictly before
	// signaling, read by workers only after receiving their signal. The
	// channel send-before-receive edge (Go memory model) makes this
	// race-free without a separate mutex.
	op  poolOp
	x   []float64
	out []float64

	signal []chan struct{}
	wg     sync.WaitGroup
}

// newMatvecPool builds a matvecPool over csr, sized to min(GOMAXPROCS,
// maxWorkers) and capped to the problem size. Small problems (nnz <
// minParallelNNZ) fall back to single-threaded dispatch — MulInto/
// MulTransposeInto then call csr's own serial methods directly.
func newMatvecPool(csr *CSR) *matvecPool {
	n, m := csr.NRows, csr.NCols
	nnz := len(csr.Vals)

	nWorkers := runtime.GOMAXPROCS(0)
	if nWorkers > maxWorkers {
		nWorkers = maxWorkers
	}
	if nWorkers > n {
		nWorkers = n
	}
	if nWorkers < 1 {
		nWorkers = 1
	}
	if nnz < minParallelNNZ {
		nWorkers = 1
	}

	p := &matvecPool{csr: csr, nWorkers: nWorkers}
	if nWorkers <= 1 {
		return p
	}

	p.buildRowRanges(n)
	p.buildColBlocks(n, m)

	// p.nWorkers is the total shard count; the dispatcher computes the
	// last shard itself (see dispatch), so only nWorkers-1 background
	// goroutines are needed.
	p.signal = make([]chan struct{}, nWorkers-1)
	for w := 0; w < nWorkers-1; w++ {
		p.signal[w] = make(chan struct{})
		go p.worker(w)
	}
	return p
}

// buildRowRanges splits [0,n) into p.nWorkers contiguous, balanced ranges
// for MulInto. Every row has the same nonzero count (Dictionary.BuildCSR
// gives each series exactly d entries), so equal-sized row ranges already
// balance nnz across workers.
func (p *matvecPool) buildRowRanges(n int) {
	nWorkers := p.nWorkers
	p.rowLo = make([]int, nWorkers)
	p.rowHi = make([]int, nWorkers)
	base, rem := n/nWorkers, n%nWorkers
	lo := 0
	for w := 0; w < nWorkers; w++ {
		hi := lo + base
		if w < rem {
			hi++
		}
		p.rowLo[w], p.rowHi[w] = lo, hi
		lo = hi
	}
}

// buildColBlocks partitions csr's entries into p.nWorkers column-disjoint
// blocks for MulTransposeInto, via a two-pass counting sort (like building
// a CSR from COO): pass 1 counts each worker's per-row entry count, pass 2
// places entries. O(nnz + n*nWorkers) time, paid once per CSR and
// amortized over the hundreds of MulTransposeInto calls one solve makes.
func (p *matvecPool) buildColBlocks(n, m int) {
	csr := p.csr
	nWorkers := p.nWorkers
	colWidth := (m + nWorkers - 1) / nWorkers
	colWorker := func(col int32) int {
		w := int(col) / colWidth
		if w >= nWorkers {
			w = nWorkers - 1
		}
		return w
	}

	rowPtrs := make([][]int32, nWorkers)
	for w := range rowPtrs {
		rowPtrs[w] = make([]int32, n+1)
	}
	for i := 0; i < n; i++ {
		for k := csr.RowPtr[i]; k < csr.RowPtr[i+1]; k++ {
			w := colWorker(csr.ColIdx[k])
			rowPtrs[w][i+1]++
		}
	}

	p.blocks = make([]csrBlock, nWorkers)
	cursor := make([][]int32, nWorkers)
	for w := 0; w < nWorkers; w++ {
		rp := rowPtrs[w]
		for i := 0; i < n; i++ {
			rp[i+1] += rp[i]
		}
		total := rp[n]
		p.blocks[w] = csrBlock{
			rowPtr: rp,
			colIdx: make([]int32, total),
			vals:   make([]float32, total),
		}
		cursor[w] = append([]int32(nil), rp[:n]...)
	}
	for i := 0; i < n; i++ {
		for k := csr.RowPtr[i]; k < csr.RowPtr[i+1]; k++ {
			col := csr.ColIdx[k]
			w := colWorker(col)
			at := cursor[w][i]
			p.blocks[w].colIdx[at] = col
			p.blocks[w].vals[at] = csr.Vals[k]
			cursor[w][i] = at + 1
		}
	}
}

// doShard executes the current op for shard w (either a row range for
// opGather or a column block for opScatter). Called both by background
// workers (over a channel signal) and directly by the dispatching
// goroutine for its own shard, inline.
func (p *matvecPool) doShard(w int) {
	switch p.op {
	case opGather:
		// Hoist the CSR's slice headers into locals: BCE hints below need
		// to reason about a stable local slice value, not a field re-read
		// through the csr pointer on every access.
		rowPtr, colIdx, vals := p.csr.RowPtr, p.csr.ColIdx, p.csr.Vals
		x, out := p.x, p.out
		lo, hi := p.rowLo[w], p.rowHi[w]
		if hi > lo {
			// BCE hints: prove rowPtr has >= hi+1 entries and out has >=
			// hi entries once, so the compiler can drop the per-iteration
			// bounds check on rowPtr[i]/rowPtr[i+1]/out[i] below (the
			// x[colIdx[k]] gather itself stays checked — colIdx[k] is a
			// data-dependent index the compiler can't bound statically).
			_ = rowPtr[hi]
			_ = out[hi-1]
		}
		for i := lo; i < hi; i++ {
			var sum float64
			for k := rowPtr[i]; k < rowPtr[i+1]; k++ {
				sum += float64(vals[k]) * x[colIdx[k]]
			}
			out[i] = sum
		}
	case opScatter:
		blk := &p.blocks[w]
		rowPtr, colIdx, vals := blk.rowPtr, blk.colIdx, blk.vals
		x, out := p.x, p.out
		n := p.csr.NRows
		if n > 0 {
			// Same BCE hint pattern for the scatter's row walk; out[colIdx[k]]
			// stays checked for the same data-dependent-index reason.
			_ = rowPtr[n]
			_ = x[n-1]
		}
		for i := 0; i < n; i++ {
			xi := x[i]
			if xi == 0 {
				continue
			}
			for k := rowPtr[i]; k < rowPtr[i+1]; k++ {
				out[colIdx[k]] += float64(vals[k]) * xi
			}
		}
	}
}

// worker runs one persistent goroutine, blocked on its own channel between
// rounds (so it consumes no CPU and doesn't contend with the dispatching
// goroutine's serial work), executing whatever op the dispatcher set for
// each signal it receives until the pool is Closed.
func (p *matvecPool) worker(w int) {
	for range p.signal[w] {
		p.doShard(w)
		p.wg.Done()
	}
}

// dispatch signals the pool's nWorkers-1 background workers (shards
// 0..nWorkers-2), then computes the last shard (nWorkers-1) itself on the
// calling goroutine before waiting for the background workers to finish.
// Doing a share of the work inline — rather than only orchestrating —
// overlaps the background workers' wake latency with useful work instead
// of the dispatcher sitting idle in Wait(), which matters at the small
// worker counts this pool uses (see maxWorkers).
func (p *matvecPool) dispatch(op poolOp, x, out []float64) {
	p.op, p.x, p.out = op, x, out
	bg := p.nWorkers - 1
	p.wg.Add(bg)
	for w := 0; w < bg; w++ {
		p.signal[w] <- struct{}{}
	}
	p.doShard(bg)
	p.wg.Wait()
}

// MulInto parallelizes CSR.MulInto (out = Phi^T @ x, gather) across the
// pool's row ranges; falls back to csr.MulInto directly when the pool
// wasn't parallelized (small problem, or GOMAXPROCS=1).
func (p *matvecPool) MulInto(x, out []float64) {
	if p.nWorkers <= 1 {
		p.csr.MulInto(x, out)
		return
	}
	p.dispatch(opGather, x, out)
}

// MulTransposeInto parallelizes CSR.MulTransposeInto (out = Phi @ x,
// scatter) across the pool's precomputed column blocks. out is zeroed
// first (on the calling goroutine, strictly before any worker is
// signaled), matching CSR.MulTransposeInto's contract.
func (p *matvecPool) MulTransposeInto(x, out []float64) {
	for i := range out {
		out[i] = 0
	}
	if p.nWorkers <= 1 {
		p.csr.MulTransposeInto(x, out)
		return
	}
	p.dispatch(opScatter, x, out)
}

// MulTransposeIntoAdd parallelizes CSR.MulTransposeIntoAdd: the same
// scatter as MulTransposeInto, but accumulating into out's existing
// contents instead of zeroing it first (Prompt 12 step 4's fused
// Phi^T(Phi z - y) pass — see fista.go).
func (p *matvecPool) MulTransposeIntoAdd(x, out []float64) {
	if p.nWorkers <= 1 {
		p.csr.MulTransposeIntoAdd(x, out)
		return
	}
	p.dispatch(opScatter, x, out)
}

// Close stops the pool's persistent workers, if any were started. Call
// once, after the pool's last matvec call.
func (p *matvecPool) Close() {
	for _, ch := range p.signal {
		close(ch)
	}
}

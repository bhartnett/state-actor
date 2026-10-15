//go:build cgo_reth

package reth

import (
	"fmt"

	"github.com/erigontech/mdbx-go/mdbx"
)

// mdbxOptSplitReserve is MDBX_opt_split_reserve (enum ordinal 21). mdbx-go
// v0.40.1 has no named constant for it; SetOption/GetOption take the raw
// MDBX_option_t ordinal.
const mdbxOptSplitReserve = 21

// fillController switches MDBX_opt_split_reserve between Put calls, so each
// table in the active --mdbx-fill-profile gets its own page-fill density.
// mdbx_env_set_option takes no lock when called from the thread already
// holding the write txn (mdbx.c: lock_needed = ENV_ACTIVE && basal_txn &&
// !env_owned_wrtxn(env)), so switching mid-txn is safe as long as every call
// happens on the goroutine that opened the enclosing env.Update — which
// locks its OS thread for the duration (mdbx-go env.go Update).
type fillController struct {
	env      *mdbx.Env
	reserves map[string]uint64 // table -> precomputed split_reserve
	current  uint64
	set      bool
}

// newFillController precomputes each controlled table's split_reserve from
// its target leaf fill. A table absent from targets stays dense (reserve 0).
func newFillController(env *mdbx.Env, targets map[string]float64) (*fillController, error) {
	reserves := make(map[string]uint64, len(FillTables))
	for _, t := range FillTables {
		f, ok := targets[t]
		if !ok {
			reserves[t] = 0
			continue
		}
		r, err := reserveFor(f)
		if err != nil {
			return nil, fmt.Errorf("reth: fill profile table %s: %w", t, err)
		}
		reserves[t] = r
	}
	return &fillController{env: env, reserves: reserves}, nil
}

// use switches split_reserve to table's target, reading the value back to
// confirm it took. A no-op when the reserve is already active.
func (c *fillController) use(table string) error {
	r := c.reserves[table] // unlisted tables (not in FillTables) stay dense
	if c.set && c.current == r {
		return nil
	}
	if err := c.env.SetOption(mdbxOptSplitReserve, r); err != nil {
		return fmt.Errorf("reth: fill: SetOption(split_reserve=%d) for %s: %w", r, table, err)
	}
	got, err := c.env.GetOption(mdbxOptSplitReserve)
	if err != nil {
		return fmt.Errorf("reth: fill: GetOption(split_reserve) for %s: %w", table, err)
	}
	if got != r {
		return fmt.Errorf("reth: fill: split_reserve readback mismatch for %s: set %d, got %d", table, r, got)
	}
	c.current = r
	c.set = true
	return nil
}

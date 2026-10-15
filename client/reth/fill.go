package reth

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// FillTables lists the MDBX tables --mdbx-fill-profile / --mdbx-fill control.
var FillTables = []string{"HashedAccounts", "AccountsTrie", "HashedStorages", "StoragesTrie", "Bytecodes"}

// FillProfiles maps a --mdbx-fill-profile name to its per-table target leaf
// fill. A table absent from the map stays dense (split_reserve=0) — "fresh"
// is the generator's canonical dense output, i.e. an empty map.
var FillProfiles = map[string]map[string]float64{
	"fresh": {},
	// jochemnet converted to reth storage v2 (migrate-v2 keeps the hashed
	// tables page for page and rebuilds the tries densely). HashedStorages is
	// one env-wide value for two populations: J's 335M-slot bloated contract
	// sits at 0.68 (uniform leaf sample), its 29-4000-slot contracts at 0.72.
	"migrated": {
		"HashedAccounts": 0.59,
		"HashedStorages": 0.70,
	},
	"aged": {
		"HashedAccounts": 0.59,
		"HashedStorages": 0.68,
		"AccountsTrie":   0.55,
		"StoragesTrie":   0.68,
	},
}

// fillReservePoints are measured (split_reserve, leaf_fill) pairs for
// ascending inserts, random 32-byte keys, ~55-byte values, 4 KiB pages.
// Branch fill tracks leaf fill and nested dupsort trees follow the same
// curve, so one table serves every controlled table. reserveFor
// piecewise-linearly interpolates it; re-fit these points (and nothing
// else) if a measured run misses its target by more than 0.03.
var fillReservePoints = []struct {
	reserve uint64
	fill    float64
}{
	{0, 0.988},
	{8192, 0.863},
	{16384, 0.738},
	{24576, 0.613},
	{32768, 0.506},
}

// reserveFor maps a target leaf fill to an MDBX_opt_split_reserve value
// (range 0..32768, in 1/65536ths of a page) by piecewise-linear
// interpolation over fillReservePoints. Rejects targets outside (0.5, 1.0].
func reserveFor(fill float64) (uint64, error) {
	if !(fill > 0.5 && fill <= 1.0) {
		return 0, fmt.Errorf("reth: fill target %.4f out of range (0.5, 1.0]", fill)
	}
	pts := fillReservePoints
	if fill >= pts[0].fill {
		return pts[0].reserve, nil
	}
	last := pts[len(pts)-1]
	if fill <= last.fill {
		return last.reserve, nil
	}
	for i := 1; i < len(pts); i++ {
		hi, lo := pts[i-1], pts[i] // hi.fill > fill >= lo.fill, hi.reserve < lo.reserve
		if fill >= lo.fill {
			frac := (hi.fill - fill) / (hi.fill - lo.fill)
			return hi.reserve + uint64(frac*float64(lo.reserve-hi.reserve)+0.5), nil
		}
	}
	// Unreachable: the >= / <= bounds above cover the full (0.5, 1.0] domain.
	return 0, fmt.Errorf("reth: fill target %.4f not covered by the interpolation table", fill)
}

// ParseFillOverrides parses a --mdbx-fill value: "Table=0.xx[,Table=0.xx]".
// Empty input returns an empty, non-nil map.
func ParseFillOverrides(s string) (map[string]float64, error) {
	out := map[string]float64{}
	if strings.TrimSpace(s) == "" {
		return out, nil
	}
	valid := make(map[string]bool, len(FillTables))
	for _, t := range FillTables {
		valid[t] = true
	}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			return nil, fmt.Errorf("reth: --mdbx-fill entry %q must be Table=0.xx", part)
		}
		table := strings.TrimSpace(kv[0])
		if !valid[table] {
			return nil, fmt.Errorf("reth: --mdbx-fill table %q not recognized; valid: %s", table, strings.Join(FillTables, ", "))
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(kv[1]), 64)
		if err != nil {
			return nil, fmt.Errorf("reth: --mdbx-fill value %q for %s: %w", kv[1], table, err)
		}
		if !(v > 0.5 && v <= 1.0) {
			return nil, fmt.Errorf("reth: --mdbx-fill value %.4f for %s out of range (0.5, 1.0]", v, table)
		}
		out[table] = v
	}
	return out, nil
}

// ResolveFillProfile merges profileName's base targets with overrides
// (overrides win) into the final per-table fill map the writer applies.
func ResolveFillProfile(profileName string, overrides map[string]float64) (map[string]float64, error) {
	base, ok := FillProfiles[profileName]
	if !ok {
		names := make([]string, 0, len(FillProfiles))
		for k := range FillProfiles {
			names = append(names, k)
		}
		sort.Strings(names)
		return nil, fmt.Errorf("reth: --mdbx-fill-profile %q not recognized; valid: %s", profileName, strings.Join(names, ", "))
	}
	resolved := make(map[string]float64, len(base)+len(overrides))
	for k, v := range base {
		resolved[k] = v
	}
	for k, v := range overrides {
		resolved[k] = v
	}
	return resolved, nil
}

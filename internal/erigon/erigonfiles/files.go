//go:build cgo_erigon_commitment

// Package erigonfiles writes each state domain's snapshot files with the pinned Erigon's own
// writers, the way Domain.buildFileRange (db/state/domain.go) builds a frozen file: the .kv
// through seg.Compressor at the domain's schema compression, then the accessors the schema
// lists (.kvi through recsplit; .bt and .kvei through btindex), each named at the version
// statecfg.Schema stamps on new files. Encoding, accessors and versions all follow the pin, so a
// generated store reads like a node's own: state-actor's own raw-word seg cost a 24,576-byte
// contract 7 pages per read against 1 on a node's file (Erigon state-DB study, round 14).
//
// Like internal/erigon/commitment, this package imports github.com/erigontech/erigon, so it
// must only be imported from files under the cgo_erigon_commitment build tag.
package erigonfiles

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/erigontech/erigon/common/background"
	"github.com/erigontech/erigon/common/log/v3"
	"github.com/erigontech/erigon/db/datastruct/btindex"
	"github.com/erigontech/erigon/db/recsplit"
	"github.com/erigontech/erigon/db/seg"
	"github.com/erigontech/erigon/db/state/statecfg"
	"github.com/erigontech/erigon/db/version"

	"github.com/ethereum/state-actor/internal/erigon/snap"
)

// Files implements snap.FileSet.
type Files struct {
	// Workers parallelizes the compressor and the .kvi build, like Erigon's own compress-workers
	// setting. Both write the same file at any count (byte for byte at 1, 8 and 16 workers on a
	// 50 GB store), so it only trades CPU for time. 0 or 1 is sequential.
	Workers int
}

var _ snap.FileSet = Files{}

func domainCfg(d snap.Domain) (statecfg.DomainCfg, error) {
	switch d {
	case snap.DomainAccounts:
		return statecfg.Schema.AccountsDomain, nil
	case snap.DomainStorage:
		return statecfg.Schema.StorageDomain, nil
	case snap.DomainCode:
		return statecfg.Schema.CodeDomain, nil
	case snap.DomainCommitment:
		return statecfg.Schema.CommitmentDomain, nil
	}
	return statecfg.DomainCfg{}, fmt.Errorf("erigonfiles: no Erigon schema for domain %d", d)
}

// filePaths returns where Erigon's schema puts d's files for r under domainDir, by extension
// (kv, kvi, bt, kvei); an accessor the schema doesn't list has no entry.
func filePaths(domainDir string, d snap.Domain, r snap.StepRange) (map[string]string, error) {
	cfg, err := domainCfg(d)
	if err != nil {
		return nil, err
	}
	kvVersion := cfg.FileVersion.DataKV.Current
	if cfg.KVWriteVersion != nil {
		kvVersion = cfg.KVWriteVersion(&cfg)
	}
	name := func(v version.Version, ext string) string {
		return filepath.Join(domainDir, fmt.Sprintf("%s-%s.%d-%d.%s", v, cfg.Name.String(), r.From, r.To, ext))
	}
	paths := map[string]string{"kv": name(kvVersion, "kv")}
	if cfg.Accessors.Has(statecfg.AccessorHashMap) {
		paths["kvi"] = name(cfg.FileVersion.AccessorKVI.Current, "kvi")
	}
	if cfg.Accessors.Has(statecfg.AccessorBTree) {
		paths["bt"] = name(cfg.FileVersion.AccessorBT.Current, "bt")
		// Erigon builds the .kvei only inside the B-tree build.
		if cfg.Accessors.Has(statecfg.AccessorExistence) {
			paths["kvei"] = name(cfg.FileVersion.AccessorKVEI.Current, "kvei")
		}
	}
	return paths, nil
}

func (f Files) WriteDomain(ctx context.Context, domainDir, tmpDir string, d snap.Domain, r snap.StepRange, keyCount uint64, salt uint32,
	entries func(yield func(snap.DomainEntry) bool)) (err error) {
	cfg, err := domainCfg(d)
	if err != nil {
		return err
	}
	paths, err := filePaths(domainDir, d, r)
	if err != nil {
		return err
	}
	// A partial file set must not pass for a complete domain on a re-run.
	defer func() {
		if err != nil {
			for _, p := range paths {
				_ = os.Remove(p)
			}
		}
	}()
	logger := log.New()

	if err := compress(ctx, cfg, paths["kv"], tmpDir, entries, f.Workers, logger); err != nil {
		return fmt.Errorf("erigonfiles: write %s: %w", paths["kv"], err)
	}
	dec, err := seg.NewDecompressor(paths["kv"])
	if err != nil {
		return err
	}
	defer dec.Close()
	if n := uint64(dec.Count() / 2); n != keyCount {
		return fmt.Errorf("erigonfiles: %s holds %d keys, expected %d", paths["kv"], n, keyCount)
	}

	if p, ok := paths["kvi"]; ok {
		if err := buildHashMap(ctx, dec, cfg, p, tmpDir, salt, f.Workers, logger); err != nil {
			return fmt.Errorf("erigonfiles: build %s: %w", p, err)
		}
	}
	if p, ok := paths["bt"]; ok {
		if err := btindex.BuildBtreeIndexWithDecompressor(p, paths["kvei"], seg.NewReader(dec.MakeGetter(), cfg.Compression),
			background.NewProgressSet(), tmpDir, salt, logger, false, cfg.Accessors); err != nil {
			return fmt.Errorf("erigonfiles: build %s: %w", p, err)
		}
	}
	return nil
}

// compress writes the .kv with the domain's compressor config at the given worker count.
func compress(ctx context.Context, cfg statecfg.DomainCfg, path, tmpDir string, entries func(yield func(snap.DomainEntry) bool), workers int, logger log.Logger) error {
	cc := cfg.CompressCfg
	cc.Workers = max(cc.Workers, workers)
	comp, err := seg.NewCompressor(ctx, "state-actor "+cfg.Name.String(), path, tmpDir, cc, log.LvlDebug, logger)
	if err != nil {
		return err
	}
	defer comp.Close()
	w := seg.NewWriter(comp, cfg.Compression)
	var werr error
	entries(func(e snap.DomainEntry) bool {
		if _, werr = w.Write(e.Key); werr != nil {
			return false
		}
		_, werr = w.Write(e.Value)
		return werr == nil
	})
	if werr != nil {
		return werr
	}
	return comp.Compress()
}

// buildHashMap mirrors buildHashMapAccessorAt and buildHashMapAccessor (db/state/domain.go),
// which are unexported: each key maps to the offset of its own word, and a collision retries
// with the next salt, which recsplit stores in the file.
func buildHashMap(ctx context.Context, dec *seg.Decompressor, cfg statecfg.DomainCfg, path, tmpDir string, salt uint32, workers int, logger log.Logger) error {
	rsVersion := version.DataStructureVersion(0)
	if !cfg.FileVersion.AccessorKVI.Current.Eq(version.V1_0) { // v1.0 files predate the fuse filter
		rsVersion = recsplit.ExistenceFilterVersion
	}
	rs, err := recsplit.NewRecSplit(recsplit.RecSplitArgs{
		Version:            rsVersion,
		Enums:              false,
		LessFalsePositives: true,
		KeyCount:           dec.Count() / 2,
		BucketSize:         recsplit.DefaultBucketSize,
		LeafSize:           recsplit.DefaultLeafSize,
		TmpDir:             tmpDir,
		IndexFile:          path,
		Salt:               &salt,
		Workers:            workers,
	}, logger)
	if err != nil {
		return err
	}
	defer rs.Close()
	g := seg.NewReader(dec.MakeGetter(), cfg.Compression)
	word := make([]byte, 0, 256)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		g.Reset(0)
		var keyPos uint64
		for g.HasNext() {
			word, _ = g.Next(word[:0])
			if err := rs.AddKey(word, keyPos); err != nil {
				return err
			}
			keyPos, _ = g.Skip()
		}
		if err := rs.Build(ctx); err != nil {
			if !rs.Collision() {
				return err
			}
			logger.Info("erigonfiles: recsplit collision, retrying with the next salt", "file", path)
			if err := rs.ResetNextSalt(); err != nil {
				return err
			}
			continue
		}
		return nil
	}
}

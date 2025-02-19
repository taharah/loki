package stats

import (
	"encoding/binary"
	"sync"

	"github.com/prometheus/common/model"
	"github.com/willf/bloom"

	"github.com/grafana/loki/pkg/storage/stores/shipper/indexgateway/indexgatewaypb"
	"github.com/grafana/loki/pkg/storage/stores/tsdb/index"
)

var BloomPool PoolBloom

type Stats = indexgatewaypb.IndexStatsResponse

func MergeStats(xs ...*Stats) (s Stats) {
	for _, x := range xs {
		if x == nil {
			continue
		}
		s.Streams += x.Streams
		s.Chunks += x.Chunks
		s.Bytes += x.Bytes
		s.Entries += x.Entries

	}
	return s
}

type PoolBloom struct {
	pool sync.Pool
}

func (p *PoolBloom) Get() *Blooms {
	if x := p.pool.Get(); x != nil {
		return x.(*Blooms)
	}

	return newBlooms()

}

func (p *PoolBloom) Put(x *Blooms) {
	x.Streams.ClearAll()
	x.Chunks.ClearAll()
	x.stats = Stats{}
	p.pool.Put(x)
}

// These are very expensive in terms of memory usage,
// each requiring ~12.5MB. Therefore we heavily rely on pool usage.
// See https://hur.st/bloomfilter for play around with this idea.
// We use bloom filter per process per query to avoid double-counting duplicates
// when calculating statistics across multiple tsdb files, however
// we cannot guarantee this when querying across period config boundaries
// as the data is requested via separate calls to the underlying store,
// which may reside on a different process (index-gateway).
// This is an accepted fault and we may double-count some values which
// are on both sides of a schema line:
// streams+chunks and thus bytes/lines.
// To avoid this, we'd need significant refactoring
// to ensure we resolve statistics for all periods together
// and this doesn't seem worth it: the code paths for iterating across different
// stores are separate.
// Another option is to ship the bloom filter bitmaps sequentially to each
// store, but this is too inefficient (~12.5MB payloads).
// signed, @owen-d
func newBlooms() *Blooms {
	// 1 million streams @ 1% error =~ 1.14MB
	streams := bloom.NewWithEstimates(1e6, 0.01)
	// 10 million chunks @ 1% error =~ 11.43MB
	chunks := bloom.NewWithEstimates(10e6, 0.01)
	return &Blooms{
		Streams: streams,
		Chunks:  chunks,
	}
}

// TODO(owen-d): shard this across a slice of smaller bloom filters to reduce
// lock contention
// Bloom filters for estimating duplicate statistics across both series
// and chunks within TSDB indices. These are used to calculate data topology
// statistics prior to running queries.
type Blooms struct {
	sync.RWMutex
	Streams, Chunks *bloom.BloomFilter
	stats           Stats
}

func (b *Blooms) Stats() Stats { return b.stats }

func (b *Blooms) AddStream(fp model.Fingerprint) {
	key := make([]byte, 8)
	binary.BigEndian.PutUint64(key, uint64(fp))
	b.add(b.Streams, key, func() {
		b.stats.Streams++
	})
}

func (b *Blooms) AddChunk(fp model.Fingerprint, chk index.ChunkMeta) {
	// fingerprint + mintime + maxtime + checksum
	ln := 8 + 8 + 8 + 4
	key := make([]byte, ln)
	binary.BigEndian.PutUint64(key, uint64(fp))
	binary.BigEndian.PutUint64(key[8:], uint64(chk.MinTime))
	binary.BigEndian.PutUint64(key[16:], uint64(chk.MaxTime))
	binary.BigEndian.PutUint32(key[24:], chk.Checksum)
	b.add(b.Chunks, key, func() {
		b.stats.Chunks++
		b.stats.Bytes += uint64(chk.KB << 10)
		b.stats.Entries += uint64(chk.Entries)
	})
}

func (b *Blooms) add(filter *bloom.BloomFilter, key []byte, update func()) {
	b.RLock()
	ok := filter.Test(key)
	b.RUnlock()

	if ok {
		return
	}

	b.Lock()
	defer b.Unlock()
	if ok = filter.TestAndAdd(key); !ok {
		update()
	}
}

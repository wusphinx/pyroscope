package storage

import (
	"encoding/binary"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/dgraph-io/badger/v2"
	"github.com/dgraph-io/badger/v2/options"
	"github.com/petethepig/pyroscope/pkg/config"
	"github.com/petethepig/pyroscope/pkg/storage/cache"
	"github.com/petethepig/pyroscope/pkg/storage/dict"
	"github.com/petethepig/pyroscope/pkg/storage/dimension"
	"github.com/petethepig/pyroscope/pkg/storage/labels"
	"github.com/petethepig/pyroscope/pkg/storage/segment"
	"github.com/petethepig/pyroscope/pkg/storage/tree"
	"github.com/petethepig/pyroscope/pkg/structs/merge"
	"github.com/sirupsen/logrus"
)

type Storage struct {
	cfg      *config.Config
	segments *cache.Cache

	dimensions *cache.Cache
	dicts      *cache.Cache
	trees      *cache.Cache
	labels     *labels.Labels

	db *badger.DB
}

func New(cfg *config.Config) (*Storage, error) {
	// spew.Dump(cfg)
	badgerPath := filepath.Join(cfg.Server.StoragePath, "badger")
	err := os.MkdirAll(badgerPath, 0755)
	if err != nil {
		return nil, err
	}
	badgerOptions := badger.DefaultOptions(badgerPath)
	badgerOptions = badgerOptions.WithTruncate(true)
	badgerOptions = badgerOptions.WithCompression(options.ZSTD)
	badgerOptions = badgerOptions.WithLogger(badgerLogger{})
	// badgerOptions = badgerOptions.WithSyncWrites(true)
	db, err := badger.Open(badgerOptions)
	if err != nil {
		return nil, err
	}

	s := &Storage{
		cfg:    cfg,
		labels: labels.New(cfg, db),
		db:     db,
	}

	s.dimensions = cache.New(db, cfg.Server.CacheDimensionSize, "i:")
	s.dimensions.Bytes = func(_k string, v interface{}) []byte {
		return v.(*dimension.Dimension).Bytes()
	}
	s.dimensions.FromBytes = func(_k string, v []byte) interface{} {
		return dimension.FromBytes(v)
	}
	s.dimensions.New = func(_k string) interface{} {
		return dimension.New()
	}

	s.segments = cache.New(db, cfg.Server.CacheSegmentSize, "s:")
	s.segments.Bytes = func(_k string, v interface{}) []byte {
		return v.(*segment.Segment).Bytes()
	}
	s.segments.FromBytes = func(_k string, v []byte) interface{} {
		// TODO:
		//   these configuration params should be saved in db when it initializes
		return segment.FromBytes(cfg.Server.MinResolution, cfg.Server.Multiplier, v)
	}
	s.segments.New = func(_k string) interface{} {
		return segment.New(s.cfg.Server.MinResolution, s.cfg.Server.Multiplier)
	}

	s.dicts = cache.New(db, cfg.Server.CacheDictionarySize, "d:")
	s.dicts.Bytes = func(_k string, v interface{}) []byte {
		return v.(*dict.Dict).Bytes()
	}
	s.dicts.FromBytes = func(_k string, v []byte) interface{} {
		return dict.FromBytes(v)
	}
	s.dicts.New = func(_k string) interface{} {
		return dict.New()
	}

	s.trees = cache.New(db, cfg.Server.CacheSegmentSize, "t:")
	s.trees.Bytes = func(k string, v interface{}) []byte {
		d := s.dicts.Get(k[:32]).(*dict.Dict)
		return v.(*tree.Tree).Bytes(d, cfg.Server.MaxNodesSerialization)
	}
	s.trees.FromBytes = func(k string, v []byte) interface{} {
		d := s.dicts.Get(k[:32]).(*dict.Dict)
		return tree.FromBytes(d, v)
	}
	s.trees.New = func(_k string) interface{} {
		return tree.New()
	}

	// TODO: horrible, remove soon
	segment.InitializeGlobalState(s.cfg.Server.MinResolution, s.cfg.Server.Multiplier)

	return s, nil
}

func treeKey(sk segment.Key, depth int, t time.Time) string {
	b := make([]byte, 32)
	copy(b[:16], sk)
	binary.BigEndian.PutUint64(b[16:24], uint64(depth))
	binary.BigEndian.PutUint64(b[24:32], uint64(t.Unix()))
	b2 := make([]byte, 64)
	hex.Encode(b2, b)
	return string(b2)
}

func (s *Storage) Put(startTime, endTime time.Time, key *Key, val *tree.Tree) error {
	logrus.WithFields(logrus.Fields{
		"startTime": startTime.String(),
		"endTime":   endTime.String(),
		"key":       key.Normalized(),
	}).Info("storage.Put")
	for k, v := range key.labels {
		s.labels.Put(k, v)
	}

	sk := segment.Key(key.Normalized())

	for k, v := range key.labels {
		d := s.dimensions.Get(k + ":" + v).(*dimension.Dimension)
		d.Insert(sk)
	}

	st := s.segments.Get(string(sk)).(*segment.Segment)
	samples := val.Samples()
	st.Put(startTime, endTime, samples, func(depth int, t time.Time, m, d int, addons []segment.Addon) {
		tk := treeKey(sk, depth, t)
		existingTree := s.trees.Get(tk).(*tree.Tree)
		// treeClone := val.Clone(m, d)
		treeClone := val.Clone(1, 1)
		for _, addon := range addons {
			tk2 := treeKey(sk, addon.Depth, addon.T)
			addonTree := s.trees.Get(tk2).(*tree.Tree)
			treeClone.Merge(addonTree)
		}
		if existingTree != nil {
			existingTree.Merge(treeClone)
			s.trees.Put(tk, existingTree)
		} else {
			s.trees.Put(tk, treeClone)
		}
	})
	s.segments.Put(string(sk), st)

	return nil
}

func (s *Storage) Get(startTime, endTime time.Time, key *Key) (*tree.Tree, *segment.Timeline, error) {
	logrus.WithFields(logrus.Fields{
		"startTime": startTime.String(),
		"endTime":   endTime.String(),
		"key":       key.Normalized(),
	}).Info("storage.Get")
	triesToMerge := []merge.Merger{}

	dimensions := []*dimension.Dimension{}
	for k, v := range key.labels {
		d := s.dimensions.Get(k + ":" + v).(*dimension.Dimension)
		dimensions = append(dimensions, d)
	}

	segmentKeys := dimension.Intersection(dimensions...)

	tl := segment.GenerateTimeline(startTime, endTime)
	for _, sk := range segmentKeys {
		st := s.segments.Get(string(sk)).(*segment.Segment)
		if st == nil {
			continue
		}

		tl.PopulateTimeline(startTime, endTime, st)

		st.Get(startTime, endTime, func(depth int, t time.Time, m, d int) {
			k := treeKey(sk, depth, t)
			tr := s.trees.Get(k).(*tree.Tree)
			// TODO: these clones are probably are not the most efficient way of doing this
			//   instead this info should be passed to the merger function imo
			tr2 := tr.Clone(1, 1) //m, d)
			triesToMerge = append(triesToMerge, merge.Merger(tr2))
		})
	}

	resultTrie := merge.MergeTriesConcurrently(runtime.NumCPU(), triesToMerge...)
	if resultTrie == nil {
		return nil, tl, nil
	}
	return resultTrie.(*tree.Tree), tl, nil
}

func (s *Storage) Close() error {
	wg := sync.WaitGroup{}
	wg.Add(3)
	go func() { s.dimensions.Flush(); wg.Done() }()
	go func() { s.segments.Flush(); wg.Done() }()
	go func() { s.trees.Flush(); wg.Done() }()
	wg.Wait()
	// dictionary has to flush last because trees write to dictionaries
	s.dicts.Flush()
	return s.db.Close()
}

func (s *Storage) GetKeys(cb func(_k string) bool) {
	s.labels.GetKeys(cb)
}

func (s *Storage) GetValues(key string, cb func(v string) bool) {
	s.labels.GetValues(key, cb)
}

func (s *Storage) Cleanup() {

}

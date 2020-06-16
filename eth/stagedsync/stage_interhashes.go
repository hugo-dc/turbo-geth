package stagedsync

import (
	"bytes"
	"fmt"
	"os"

	"github.com/ledgerwatch/turbo-geth/common"
	"github.com/ledgerwatch/turbo-geth/common/dbutils"
	"github.com/ledgerwatch/turbo-geth/common/etl"
	"github.com/ledgerwatch/turbo-geth/core/rawdb"
	"github.com/ledgerwatch/turbo-geth/core/types/accounts"
	"github.com/ledgerwatch/turbo-geth/ethdb"
	"github.com/ledgerwatch/turbo-geth/log"
	"github.com/ledgerwatch/turbo-geth/trie"
)

//nolint:interfacer
func SpawnIntermediateHashesStage(s *StageState, stateDB ethdb.Database, datadir string, quit chan struct{}) error {
	syncHeadNumber, err := s.ExecutionAt(stateDB)
	if err != nil {
		return err
	}

	if s.BlockNumber == syncHeadNumber {
		// we already did hash check for this block
		// we don't do the obvious `if s.BlockNumber > syncHeadNumber` to support reorgs more naturally
		s.Done()
		return nil
	}
	log.Info("Generating intermediate hashes", "from", s.BlockNumber, "to", syncHeadNumber)

	if err := updateIntermediateHashes(s, stateDB, s.BlockNumber, syncHeadNumber, datadir, quit); err != nil {
		return err
	}
	return s.DoneAndUpdate(stateDB, syncHeadNumber)
}

func updateIntermediateHashes(s *StageState, db ethdb.Database, from, to uint64, datadir string, quit chan struct{}) error {
	hash := rawdb.ReadCanonicalHash(db, to)
	syncHeadHeader := rawdb.ReadHeader(db, hash, to)
	expectedRootHash := syncHeadHeader.Root
	if s.BlockNumber == 0 {
		return regenerateIntermediateHashes(s, db, to, datadir, expectedRootHash, quit)
	}
	return incrementIntermediateHashes(s, db, from, to, datadir, expectedRootHash, quit)
}

func regenerateIntermediateHashes(s *StageState, db ethdb.Database, to uint64, datadir string, expectedRootHash common.Hash, quit chan struct{}) error {
	collector := etl.NewCollector(".", etl.NewSortableBuffer(etl.BufferOptimalSize))
	hashCollector := func(keyHex []byte, hash []byte) error {
		if len(keyHex)%2 != 0 || len(keyHex) == 0 {
			return nil
		}
		var k []byte
		trie.CompressNibbles(keyHex, &k)
		return collector.Collect(k, common.CopyBytes(hash))
	}
	loader := trie.NewFlatDbSubTrieLoader()
	if err := loader.Reset(db, trie.NewRetainList(0), trie.NewRetainList(0), hashCollector /* HashCollector */, [][]byte{nil}, []int{0}, false); err != nil {
		return err
	}
	if subTries, err := loader.LoadSubTries(); err == nil {
		if subTries.Hashes[0] != expectedRootHash {
			return fmt.Errorf("wrong trie root: %x, expected (from header): %x", subTries.Hashes[0], expectedRootHash)
		}
		log.Info("Collection finished", "root hash", subTries.Hashes[0].Hex())
	} else {
		return err
	}
	quitCh := make(chan struct{})
	if err := collector.Load(db, dbutils.IntermediateTrieHashBucket, etl.IdentityLoadFunc, etl.TransformArgs{Quit: quitCh}); err != nil {
		return err
	}
	log.Info("Regeneration ended")
	return nil
}

type Receiver struct {
	defaultReceiver *trie.DefaultReceiver
	accountMap      map[string]*accounts.Account
	storageMap      map[string][]byte
	unfurlList      []string
	currentIdx      int
}

func (r *Receiver) Receive(
	itemType trie.StreamItem,
	accountKey []byte,
	storageKey []byte,
	accountValue *accounts.Account,
	storageValue []byte,
	hash []byte,
	cutoff int,
	witnessSize uint64,
) error {
	for r.currentIdx < len(r.unfurlList) {
		ks := r.unfurlList[r.currentIdx]
		k := []byte(ks)
		var c int
		switch itemType {
		case trie.StorageStreamItem, trie.SHashStreamItem:
			c = bytes.Compare(k, storageKey)
		case trie.AccountStreamItem, trie.AHashStreamItem:
			c = bytes.Compare(k, accountKey)
		case trie.CutoffStreamItem:
			c = -1
		}
		if c > 0 {
			return r.defaultReceiver.Receive(itemType, accountKey, storageKey, accountValue, storageValue, hash, cutoff, witnessSize)
		}
		if len(k) > common.HashLength {
			v := r.storageMap[ks]
			if len(v) > 0 {
				if err := r.defaultReceiver.Receive(trie.StorageStreamItem, nil, k, nil, v, nil, 0, 0); err != nil {
					return err
				}
			}
		} else {
			v := r.accountMap[ks]
			if v != nil {
				if err := r.defaultReceiver.Receive(trie.AccountStreamItem, k, nil, v, nil, nil, 0, 0); err != nil {
					return err
				}
			}
		}
		r.currentIdx++
		if c == 0 {
			return nil
		}
	}
	// We ran out of modifications, simply pass through
	return r.defaultReceiver.Receive(itemType, accountKey, storageKey, accountValue, storageValue, hash, cutoff, witnessSize)
}

func (r *Receiver) Result() trie.SubTries {
	return r.defaultReceiver.Result()
}

func (r *Receiver) accountLoad(k []byte, value []byte, state etl.State, next etl.LoadNextFunc) error {
	newK, err := transformPlainStateKey(k)
	if err != nil {
		return err
	}
	if len(value) > 0 {
		var a accounts.Account
		if err = a.DecodeForStorage(value); err != nil {
			return err
		}
		r.accountMap[string(newK)] = &a
	} else {
		r.accountMap[string(newK)] = nil
	}
	r.unfurlList = append(r.unfurlList, string(newK))
	return nil
}

func (r *Receiver) storageLoad(k []byte, value []byte, state etl.State, next etl.LoadNextFunc) error {
	newK, err := transformPlainStateKey(k)
	if err != nil {
		return err
	}
	if len(value) > 0 {
		r.storageMap[string(newK)] = common.CopyBytes(value)
	} else {
		r.storageMap[string(newK)] = nil
	}
	r.unfurlList = append(r.unfurlList, string(newK))
	return nil
}

func NewHashUpdater(db ethdb.Database, quitCh chan struct{}) *HashUpdater {
	return &HashUpdater{
		db:               db,
		ChangeSetBufSize: 256 * 1024 * 1024,
		TempDir:          os.TempDir(),
	}
}

type HashUpdater struct {
	db               ethdb.Database
	ChangeSetBufSize uint64
	TempDir          string
	quitCh           chan struct{}
}

func (p *HashUpdater) Update(s *StageState, from, to uint64, storage bool, index byte, r *Receiver) error {
	var changeSetBucket []byte
	if storage {
		changeSetBucket = dbutils.PlainStorageChangeSetBucket
	} else {
		changeSetBucket = dbutils.PlainAccountChangeSetBucket
	}
	log.Info("Incremental update of intermediate hashes", "from", from, "to", to, "csbucket", string(changeSetBucket))

	startkey := dbutils.EncodeTimestamp(from + 1)
	skip := false

	var loadStartKey []byte
	if len(s.StageData) != 0 {
		// we have finished this stage but didn't start the next one
		if len(s.StageData) == 1 && s.StageData[0] == index {
			skip = true
			// we are already at the next stage
		} else if s.StageData[0] > index {
			skip = true
			// if we at the current stage and we have something meaningful at StageData
		} else if s.StageData[0] == index {
			var err error
			loadStartKey, err = etl.NextKey(s.StageData[1:])
			if err != nil {
				return err
			}
		}
	}
	if skip {
		return nil
	}
	var loadFunc etl.LoadFunc
	if storage {
		loadFunc = r.storageLoad
	} else {
		loadFunc = r.accountLoad
	}

	if err := etl.Transform(
		p.db,
		changeSetBucket,
		dbutils.CurrentStateBucket,
		p.TempDir,
		getExtractFunc(changeSetBucket),
		// here we avoid getting the state from changesets,
		// we just care about the accounts that did change,
		// so we can directly read from the PlainTextBuffer
		getFromPlainStateAndLoad(p.db, loadFunc),
		etl.TransformArgs{
			BufferType:      etl.SortableAppendBuffer,
			ExtractStartKey: startkey,
			LoadStartKey:    loadStartKey,
			OnLoadCommit: func(putter ethdb.Putter, key []byte, isDone bool) error {
				if isDone {
					return s.UpdateWithStageData(putter, from, []byte{index})
				}
				return s.UpdateWithStageData(putter, from, append([]byte{index}, key...))
			},
			Quit: p.quitCh,
		},
	); err != nil {
		return err
	}
	return nil
}

func incrementIntermediateHashes(s *StageState, db ethdb.Database, from, to uint64, datadir string, expectedRootHash common.Hash, quit chan struct{}) error {
	updater := NewHashUpdater(db, quit)
	updater.TempDir = datadir
	r := &Receiver{defaultReceiver: trie.NewDefaultReceiver()}
	if err := updater.Update(s, from, to, false /* storage */, 0x01, r); err != nil {
		return err
	}
	if err := updater.Update(s, from, to, true /* storage */, 0x02, r); err != nil {
		return err
	}
	unfurl := trie.NewRetainList(0)
	for _, ks := range r.unfurlList {
		unfurl.AddKey([]byte(ks))
	}
	collector := etl.NewCollector(".", etl.NewSortableBuffer(etl.BufferOptimalSize))
	hashCollector := func(keyHex []byte, hash []byte) error {
		if len(keyHex)%2 != 0 || len(keyHex) == 0 {
			return nil
		}
		var k []byte
		trie.CompressNibbles(keyHex, &k)
		return collector.Collect(k, common.CopyBytes(hash))
	}
	loader := trie.NewFlatDbSubTrieLoader()
	// hashCollector in the line below will collect deletes
	if err := loader.Reset(db, unfurl, trie.NewRetainList(0), hashCollector, [][]byte{nil}, []int{0}, false); err != nil {
		return err
	}
	// hashCollector in the line below will collect creations of new intermediate hashes
	r.defaultReceiver.Reset(trie.NewRetainList(0), hashCollector, false)
	loader.SetStreamReceiver(r)
	if subTries, err := loader.LoadSubTries(); err == nil {
		if subTries.Hashes[0] != expectedRootHash {
			return fmt.Errorf("wrong trie root: %x, expected (from header): %x", subTries.Hashes[0], expectedRootHash)
		}
		log.Info("Collection finished", "root hash", subTries.Hashes[0].Hex())
	} else {
		return err
	}
	quitCh := make(chan struct{})
	if err := collector.Load(db, dbutils.IntermediateTrieHashBucket, etl.IdentityLoadFunc, etl.TransformArgs{Quit: quitCh}); err != nil {
		return err
	}
	return nil
}

//nolint:interfacer
func unwindIntermediateHashesStage(u *UnwindState, _ *StageState, stateDB ethdb.Database, _ string, _ chan struct{}) error {
	if err := u.Done(stateDB); err != nil {
		return fmt.Errorf("unwind IntermediateHashes: reset: %w", err)
	}
	return nil
}

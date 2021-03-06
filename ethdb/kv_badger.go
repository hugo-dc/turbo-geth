package ethdb

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/dgraph-io/badger/v2"
	"github.com/ledgerwatch/turbo-geth/common"
	"github.com/ledgerwatch/turbo-geth/common/dbutils"
	"github.com/ledgerwatch/turbo-geth/log"
)

var (
	badgerTxPool     = sync.Pool{New: func() interface{} { return &badgerTx{} }}     // pool of ethdb.badgerTx objects
	badgerCursorPool = sync.Pool{New: func() interface{} { return &badgerCursor{} }} // pool of ethdb.badgerCursor objects
)

type badgerOpts struct {
	Badger badger.Options
}

func (opts badgerOpts) Path(path string) badgerOpts {
	opts.Badger = opts.Badger.WithDir(path).WithValueDir(path)
	return opts
}

func (opts badgerOpts) InMem() badgerOpts {
	opts.Badger = opts.Badger.WithInMemory(true)
	return opts
}

func (opts badgerOpts) ReadOnly() badgerOpts {
	opts.Badger = opts.Badger.WithReadOnly(true)
	return opts
}

func (opts badgerOpts) Open() (KV, error) {
	logger := log.New("badger_db", opts.Badger.Dir)
	opts.Badger = opts.Badger.WithMaxTableSize(128 << 20) // 128MB, default 64Mb

	if opts.Badger.InMemory {
		opts.Badger = opts.Badger.WithEventLogging(false).WithNumCompactors(1)
	}

	if !opts.Badger.InMemory {
		if err := os.MkdirAll(opts.Badger.Dir, 0744); err != nil {
			return nil, fmt.Errorf("could not create dir: %s, %w", opts.Badger.Dir, err)
		}
	}

	badgerDB, err := badger.Open(opts.Badger)
	if err != nil {
		return nil, err
	}

	db := &badgerKV{
		opts:   opts,
		badger: badgerDB,
		log:    logger,
		wg:     &sync.WaitGroup{},
	}

	if !opts.Badger.InMemory {
		ctx, ctxCancel := context.WithCancel(context.Background())
		db.stopGC = ctxCancel
		db.wg.Add(1)
		go func() {
			defer db.wg.Done()
			gcTicker := time.NewTicker(gcPeriod)
			defer gcTicker.Stop()
			db.vlogGCLoop(ctx, gcTicker)
		}()
	}

	return db, nil
}

func (opts badgerOpts) MustOpen() KV {
	db, err := opts.Open()
	if err != nil {
		panic(err)
	}
	return db
}

type badgerKV struct {
	opts   badgerOpts
	badger *badger.DB
	log    log.Logger
	stopGC context.CancelFunc
	wg     *sync.WaitGroup
}

func NewBadger() badgerOpts {
	return badgerOpts{Badger: badger.DefaultOptions("")}
}

func (db *badgerKV) vlogGCLoop(ctx context.Context, gcTicker *time.Ticker) {
	// DB.RunValueLogGC():
	// This method is designed to do garbage collection while Badger is online.
	// Along with randomly picking a file, it uses statistics generated by the LSM-tree compactions
	// to pick files that are likely to lead to maximum space reclamation. It is recommended to be called
	// during periods of low activity in your system, or periodically. One call would only result in removal
	// of at max one log file. As an optimization, you could also immediately re-run it whenever
	// it returns nil error (indicating a successful value log GC), as shown below.
	i := 0
	for {
		for { // do work until badger.ErrNoRewrite
			err := db.badger.RunValueLogGC(0.7)
			if err == nil {
				i++
			}
			if errors.Is(err, badger.ErrNoRewrite) {
				db.log.Info("Badger GC happened", "rewritten_vlog_files", i)
				i = 0
				break
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-gcTicker.C:
		}
	}
}

// Close closes BoltKV
// All transactions must be closed before closing the database.
func (db *badgerKV) Close() {
	if db.stopGC != nil {
		db.stopGC()
	}
	db.wg.Wait()
	if db.badger != nil {
		if err := db.badger.Close(); err != nil {
			db.log.Warn("failed to close badger DB", "err", err)
		} else {
			db.log.Info("badger database closed")
		}
	}
}

func (db *badgerKV) DiskSize(_ context.Context) (common.StorageSize, error) {
	lsm, vlog := db.badger.Size()
	return common.StorageSize(lsm + vlog), nil
}

func (db *badgerKV) BucketsStat(_ context.Context) (map[string]common.StorageBucketWriteStats, error) {
	return map[string]common.StorageBucketWriteStats{}, nil
}

func (db *badgerKV) IdealBatchSize() int {
	return int(db.badger.MaxBatchSize() / 2)
}

func (db *badgerKV) Begin(ctx context.Context, writable bool) (Tx, error) {
	t := badgerTxPool.Get().(*badgerTx)
	defer badgerTxPool.Put(t)
	t.ctx = ctx
	t.db = db
	t.badger = db.badger.NewTransaction(writable)
	return t, nil
}

type badgerTx struct {
	ctx context.Context
	db  *badgerKV

	badger  *badger.Txn
	cursors []*badgerCursor
}

type badgerBucket struct {
	nameLen uint
	tx      *badgerTx
	prefix  []byte
	id      int
}

type badgerCursor struct {
	badgerOpts badger.IteratorOptions
	ctx        context.Context
	bucket     badgerBucket
	prefix     []byte

	badger *badger.Iterator

	k   []byte
	v   []byte
	err error
}

func (db *badgerKV) View(ctx context.Context, f func(tx Tx) error) (err error) {
	t := badgerTxPool.Get().(*badgerTx)
	defer badgerTxPool.Put(t)
	t.db = db
	t.ctx = ctx
	return db.badger.View(func(tx *badger.Txn) error {
		defer t.closeCursors()
		t.badger = tx
		return f(t)
	})
}

func (db *badgerKV) Update(ctx context.Context, f func(tx Tx) error) (err error) {
	t := badgerTxPool.Get().(*badgerTx)
	defer badgerTxPool.Put(t)
	t.ctx = ctx
	t.db = db
	return db.badger.Update(func(tx *badger.Txn) error {
		defer t.closeCursors()
		t.badger = tx
		return f(t)
	})
}

func (tx *badgerTx) Bucket(name []byte) Bucket {
	b := badgerBucket{tx: tx, nameLen: uint(len(name)), id: dbutils.BucketsIndex[string(name)]}
	b.prefix = name
	return b
}

func (tx *badgerTx) Commit(ctx context.Context) error {
	tx.closeCursors()
	return tx.badger.Commit()
}

func (tx *badgerTx) Rollback() {
	tx.closeCursors()
	tx.badger.Discard()
}

func (tx *badgerTx) closeCursors() {
	for _, c := range tx.cursors {
		if c.badger != nil {
			c.badger.Close()
		}
		badgerCursorPool.Put(c)
	}
	tx.cursors = tx.cursors[:0]
}

func (c *badgerCursor) Prefix(v []byte) Cursor {
	c.prefix = append(c.prefix[:0], c.bucket.prefix[:c.bucket.nameLen]...)
	c.prefix = append(c.prefix, v...)

	c.badgerOpts.Prefix = append(c.badgerOpts.Prefix[:0], c.prefix...)
	return c
}

func (c *badgerCursor) MatchBits(n uint) Cursor {
	panic("not implemented yet")
}

func (c *badgerCursor) Prefetch(v uint) Cursor {
	c.badgerOpts.PrefetchSize = int(v)
	return c
}

func (c *badgerCursor) NoValues() NoValuesCursor {
	c.badgerOpts.PrefetchValues = false
	return &badgerNoValuesCursor{badgerCursor: c}
}

func (b badgerBucket) Get(key []byte) (val []byte, err error) {
	select {
	case <-b.tx.ctx.Done():
		return nil, b.tx.ctx.Err()
	default:
	}

	var item *badger.Item
	b.prefix = append(b.prefix[:b.nameLen], key...)
	item, err = b.tx.badger.Get(b.prefix)
	if err != nil {
		if errors.Is(err, badger.ErrKeyNotFound) {
			return nil, nil
		}
		return nil, err
	}
	if item != nil {
		val, err = item.ValueCopy(nil) // can improve this by using pool
	}
	if val == nil {
		val = []byte{}
	}
	return val, err
}

func (b badgerBucket) Put(key []byte, value []byte) error {
	select {
	case <-b.tx.ctx.Done():
		return b.tx.ctx.Err()
	default:
	}

	b.prefix = append(b.prefix[:b.nameLen], key...) // avoid passing buffer in Put, need copy bytes
	return b.tx.badger.Set(common.CopyBytes(b.prefix), value)
}

func (b badgerBucket) Delete(key []byte) error {
	select {
	case <-b.tx.ctx.Done():
		return b.tx.ctx.Err()
	default:
	}

	b.prefix = append(b.prefix[:b.nameLen], key...)
	return b.tx.badger.Delete(b.prefix)
}

func (b badgerBucket) Size() (uint64, error) {
	panic("not implemented")
}

func (b badgerBucket) Clear() error {
	return b.tx.db.badger.DropPrefix(dbutils.Buckets[b.id])
}

func (b badgerBucket) Cursor() Cursor {
	c := badgerCursorPool.Get().(*badgerCursor)
	c.bucket = b
	c.ctx = b.tx.ctx
	c.badgerOpts = badger.DefaultIteratorOptions
	c.prefix = append(c.prefix[:0], c.bucket.prefix[:c.bucket.nameLen]...)
	c.badgerOpts.Prefix = append(c.badgerOpts.Prefix[:0], c.prefix...)
	c.k = nil
	c.v = nil
	c.err = nil
	c.badger = nil
	// add to auto-close on end of transactions
	if b.tx.cursors == nil {
		b.tx.cursors = make([]*badgerCursor, 0, 1)
	}
	b.tx.cursors = append(b.tx.cursors, c)
	return c
}

func (c *badgerCursor) initCursor() {
	if c.badger != nil {
		return
	}

	c.badger = c.bucket.tx.badger.NewIterator(c.badgerOpts)
}

func (c *badgerCursor) First() ([]byte, []byte, error) {
	c.initCursor()

	c.badger.Rewind()
	if !c.badger.Valid() {
		c.k, c.v = nil, nil
		return c.k, c.v, nil
	}
	item := c.badger.Item()
	c.k = item.Key()[c.bucket.nameLen:]
	if c.badgerOpts.PrefetchValues {
		c.v, c.err = item.ValueCopy(c.v) // bech show: using .ValueCopy on same buffer has same speed as item.Value()
	}
	if c.err != nil {
		return []byte{}, nil, c.err
	}
	if c.v == nil {
		c.v = []byte{}
	}
	return c.k, c.v, nil
}

func (c *badgerCursor) Seek(seek []byte) ([]byte, []byte, error) {
	select {
	case <-c.ctx.Done():
		return []byte{}, nil, c.ctx.Err()
	default:
	}

	c.initCursor()

	c.badger.Seek(append(c.prefix[:c.bucket.nameLen], seek...))
	if !c.badger.Valid() {
		c.k, c.v = nil, nil
		return c.k, c.v, nil
	}
	item := c.badger.Item()
	c.k = item.Key()[c.bucket.nameLen:]
	if c.badgerOpts.PrefetchValues {
		c.v, c.err = item.ValueCopy(c.v)
	}
	if c.err != nil {
		return []byte{}, nil, c.err
	}
	if c.v == nil {
		c.v = []byte{}
	}

	return c.k, c.v, nil
}

func (c *badgerCursor) SeekTo(seek []byte) ([]byte, []byte, error) {
	return c.Seek(seek)
}

func (c *badgerCursor) Next() ([]byte, []byte, error) {
	select {
	case <-c.ctx.Done():
		return []byte{}, nil, c.ctx.Err() // on error key should be != nil
	default:
	}

	c.badger.Next()
	if !c.badger.Valid() {
		c.k, c.v = nil, nil
		return c.k, c.v, nil
	}
	item := c.badger.Item()
	c.k = item.Key()[c.bucket.nameLen:]
	if c.badgerOpts.PrefetchValues {
		c.v, c.err = item.ValueCopy(c.v)
	}
	if c.err != nil {
		return []byte{}, nil, c.err // on error key should be != nil
	}
	if c.v == nil {
		c.v = []byte{}
	}
	return c.k, c.v, nil
}

func (c *badgerCursor) Delete(key []byte) error {
	select {
	case <-c.ctx.Done():
		return c.ctx.Err()
	default:
	}

	return c.bucket.Delete(key)
}

func (c *badgerCursor) Put(key []byte, value []byte) error {
	select {
	case <-c.ctx.Done():
		return c.ctx.Err()
	default:
	}

	return c.bucket.Put(key, value)
}

func (c *badgerCursor) Append(key []byte, value []byte) error {
	return c.Put(key, value)
}

func (c *badgerCursor) Walk(walker func(k, v []byte) (bool, error)) error {
	for k, v, err := c.First(); k != nil; k, v, err = c.Next() {
		if err != nil {
			return err
		}
		ok, err := walker(k, v)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
	}
	return nil
}

type badgerNoValuesCursor struct {
	*badgerCursor
}

func (c *badgerNoValuesCursor) Walk(walker func(k []byte, vSize uint32) (bool, error)) error {
	for k, vSize, err := c.First(); k != nil; k, vSize, err = c.Next() {
		if err != nil {
			return err
		}
		ok, err := walker(k, vSize)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
	}
	return nil
}

func (c *badgerNoValuesCursor) First() ([]byte, uint32, error) {
	c.initCursor()
	c.badger.Rewind()
	if !c.badger.Valid() {
		c.k, c.v = nil, nil
		return c.k, 0, nil
	}
	item := c.badger.Item()
	c.k = item.Key()[c.bucket.nameLen:]
	return c.k, uint32(item.ValueSize()), nil
}

func (c *badgerNoValuesCursor) Seek(seek []byte) ([]byte, uint32, error) {
	select {
	case <-c.ctx.Done():
		return []byte{}, 0, c.ctx.Err()
	default:
	}

	c.initCursor()

	c.badger.Seek(append(c.prefix[:c.bucket.nameLen], seek...))
	if !c.badger.Valid() {
		c.k, c.v = nil, nil
		return c.k, 0, nil
	}
	item := c.badger.Item()
	c.k = item.Key()[c.bucket.nameLen:]

	return c.k, uint32(item.ValueSize()), nil
}

func (c *badgerNoValuesCursor) SeekTo(seek []byte) ([]byte, uint32, error) {
	return c.Seek(seek)
}

func (c *badgerNoValuesCursor) Next() ([]byte, uint32, error) {
	select {
	case <-c.ctx.Done():
		return []byte{}, 0, c.ctx.Err()
	default:
	}

	c.badger.Next()
	if !c.badger.Valid() {
		c.k, c.v = nil, nil
		return c.k, 0, nil
	}
	item := c.badger.Item()
	c.k = item.Key()[c.bucket.nameLen:]
	return c.k, uint32(item.ValueSize()), nil
}

// Copyright 2014 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

// Package ethdb defines the interfaces for an Ethereum data store.
package ethdb

import (
	"bytes"
	"context"
	"fmt"

	"github.com/ledgerwatch/turbo-geth/common"
	"github.com/ledgerwatch/turbo-geth/common/dbutils"
	"github.com/ledgerwatch/turbo-geth/log"
)

// RemoteBoltDatabase is a wrapper over BoltDb,
// compatible with the Database interface.
type RemoteBoltDatabase struct {
	db  KV         // BoltDB instance
	log log.Logger // Contextual logger tracking the database path
}

// NewRemoteBoltDatabase returns a BoltDB wrapper.
func NewRemoteBoltDatabase(db KV) *RemoteBoltDatabase {
	logger := log.New()

	return &RemoteBoltDatabase{
		db:  db,
		log: logger,
	}
}

// Has checks if the value exists
//
// Deprecated: DB accessors must accept Tx object instead of open Read transaction internally
func (db *RemoteBoltDatabase) Has(bucket, key []byte) (bool, error) {
	var has bool
	err := db.db.View(context.Background(), func(tx Tx) error {
		b := tx.Bucket(bucket)
		if b == nil {
			has = false
		} else {
			v, err := b.Get(key)
			if err != nil {
				return err
			}
			has = v != nil
		}
		return nil
	})
	return has, err
}

func (db *RemoteBoltDatabase) DiskSizeDiskSize(ctx context.Context) (common.StorageSize, error) {
	return db.db.(HasStats).DiskSize(ctx)
}

// Get returns the value for a given key if it's present.
//
// Deprecated: DB accessors must accept Tx object instead of open Read transaction internally
func (db *RemoteBoltDatabase) Get(bucket, key []byte) ([]byte, error) {

	// Retrieve the key and increment the miss counter if not found
	var dat []byte
	err := db.db.View(context.Background(), func(tx Tx) error {
		b := tx.Bucket(bucket)
		if b == nil {
			return fmt.Errorf("dbi not found, %s", bucket)
		}

		v, err := b.Get(key)
		if err != nil {
			return fmt.Errorf("%w. dbi: %s, key: %s", err, bucket, key)
		}
		if v != nil {
			dat = make([]byte, len(v))
			copy(dat, v)
		}
		return nil
	})
	if dat == nil {
		return nil, ErrKeyNotFound
	}
	return dat, err
}

// Get returns the value for a given key if it's present.
//
// Deprecated: DB accessors must accept Tx object instead of open Read transaction internally
func (db *RemoteBoltDatabase) GetIndexChunk(bucket, key []byte, timestamp uint64) ([]byte, error) {
	// Retrieve the key and increment the miss counter if not found
	var dat []byte
	err := db.db.View(context.Background(), func(tx Tx) error {
		b := tx.Bucket(bucket)
		if b == nil {
			return fmt.Errorf("dbi not found, %s", bucket)
		}

		c := b.Cursor()
		k, v, err := c.Seek(dbutils.IndexChunkKey(key, timestamp))
		if err != nil {
			return fmt.Errorf("%w. dbi: %s, key: %s", err, bucket, key)
		}

		if !bytes.HasPrefix(k, key) {
			return ErrKeyNotFound
		}
		if v == nil {
			return nil
		}

		dat = make([]byte, len(v))
		copy(dat, v)

		return nil
	})
	if dat == nil {
		return nil, ErrKeyNotFound
	}
	return dat, err
}

func (db *RemoteBoltDatabase) Walk(bucket, startkey []byte, fixedbits int, walker func(k, v []byte) (bool, error)) error {
	fixedbytes, mask := Bytesmask(fixedbits)
	err := db.db.View(context.Background(), func(tx Tx) error {
		b := tx.Bucket(bucket)
		if b == nil {
			return nil
		}
		c := b.Cursor()
		k, v, err := c.Seek(startkey)
		if err != nil {
			return err
		}

		for k != nil && (fixedbits == 0 || bytes.Equal(k[:fixedbytes-1], startkey[:fixedbytes-1]) && (k[fixedbytes-1]&mask) == (startkey[fixedbytes-1]&mask)) {
			goOn, err := walker(k, v)
			if err != nil {
				return err
			}
			if !goOn {
				break
			}
			k, v, err = c.Next()
			if err != nil {
				return err
			}
		}
		return nil
	})
	return err
}

func (db *RemoteBoltDatabase) MultiWalk(bucket []byte, startkeys [][]byte, fixedbits []int, walker func(int, []byte, []byte) error) error {
	if len(startkeys) == 0 {
		return nil
	}
	rangeIdx := 0 // What is the current range we are extracting
	fixedbytes, mask := Bytesmask(fixedbits[rangeIdx])
	startkey := startkeys[rangeIdx]
	err := db.db.View(context.Background(), func(tx Tx) error {
		b := tx.Bucket(bucket)
		if b == nil {
			return nil
		}
		c := b.Cursor()

		k, v, err := c.Seek(startkey)
		if err != nil {
			return err
		}

		for k != nil {
			// Adjust rangeIdx if needed
			if fixedbytes > 0 {
				cmp := int(-1)
				for cmp != 0 {
					cmp = bytes.Compare(k[:fixedbytes-1], startkey[:fixedbytes-1])
					if cmp == 0 {
						k1 := k[fixedbytes-1] & mask
						k2 := startkey[fixedbytes-1] & mask
						if k1 < k2 {
							cmp = -1
						} else if k1 > k2 {
							cmp = 1
						}
					}
					if cmp < 0 {
						//k, v, err = c.SeekTo(startkey)
						k, v, err = c.Seek(startkey)
						if err != nil {
							return err
						}
						if k == nil {
							return nil
						}
					} else if cmp > 0 {
						rangeIdx++
						if rangeIdx == len(startkeys) {
							return nil
						}
						fixedbytes, mask = Bytesmask(fixedbits[rangeIdx])
						startkey = startkeys[rangeIdx]
					}
				}
			}
			if len(v) > 0 {
				if err := walker(rangeIdx, k, v); err != nil {
					return err
				}
			}
			k, v, err = c.Next()
			if err != nil {
				return err
			}
		}
		return nil
	})
	return err
}

func (db *RemoteBoltDatabase) Close() {
	db.db.Close()
}

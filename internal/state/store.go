// Package state is the local record of every file the courier has seen:
// its size, mtime and checksum, and whether it has been uploaded. It lets a
// restart skip unchanged files without re-hashing them, and holds the upload
// queue while the server is unreachable.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	bolt "go.etcd.io/bbolt"
)

type Status string

const (
	Queued    Status = "queued"    // hashed, waiting to upload
	Failed    Status = "failed"    // upload was refused; retried at NextTry
	Uploaded  Status = "uploaded"  // uploaded by us
	Duplicate Status = "duplicate" // the server already had it
	Rejected  Status = "rejected"  // the server won't accept this file type
)

// InQueue reports whether a record still needs uploading.
func (s Status) InQueue() bool { return s == Queued || s == Failed }

type Record struct {
	Account   string    `json:"a"`
	Size      int64     `json:"s"`
	ModTime   int64     `json:"m"` // unix nanoseconds
	SHA1      string    `json:"h"` // hex
	Status    Status    `json:"st"`
	AssetID   string    `json:"id,omitempty"`
	Attempts  int       `json:"n,omitempty"`
	NextTry   int64     `json:"nt,omitempty"` // unix seconds
	LastError string    `json:"e,omitempty"`
	Updated   time.Time `json:"u"`
}

// Entry is a record with its file path.
type Entry struct {
	Path string
	Record
}

var (
	bFiles = []byte("files")
	bQueue = []byte("queue") // key: account \x00 path
	bMeta  = []byte("meta")
)

type Store struct{ db *bolt.DB }

// Open opens (or creates) the state database. Only one process can hold it.
func Open(path string) (*Store, error) {
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: time.Second})
	if errors.Is(err, bolt.ErrTimeout) {
		return nil, fmt.Errorf("state database %s is in use; is immich-courier already running?", path)
	}
	if err != nil {
		return nil, err
	}
	err = db.Update(func(tx *bolt.Tx) error {
		for _, b := range [][]byte{bFiles, bQueue, bMeta} {
			if _, err := tx.CreateBucketIfNotExists(b); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Get(path string) (Record, bool, error) {
	var r Record
	var ok bool
	err := s.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket(bFiles).Get([]byte(path))
		if v == nil {
			return nil
		}
		ok = true
		return json.Unmarshal(v, &r)
	})
	return r, ok, err
}

func queueKey(account, path string) []byte { return []byte(account + "\x00" + path) }

// Put stores a record and keeps the queue index in step with its status.
func (s *Store) Put(path string, r Record) error {
	r.Updated = time.Now().UTC()
	v, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		files, queue := tx.Bucket(bFiles), tx.Bucket(bQueue)
		if old := files.Get([]byte(path)); old != nil {
			var o Record
			if json.Unmarshal(old, &o) == nil {
				if err := queue.Delete(queueKey(o.Account, path)); err != nil {
					return err
				}
			}
		}
		if r.Status.InQueue() {
			if err := queue.Put(queueKey(r.Account, path), nil); err != nil {
				return err
			}
		}
		return files.Put([]byte(path), v)
	})
}

// Due returns up to limit queued records for an account whose retry time has come.
func (s *Store) Due(account string, now time.Time, limit int) ([]Entry, error) {
	var out []Entry
	prefix := []byte(account + "\x00")
	err := s.db.View(func(tx *bolt.Tx) error {
		files := tx.Bucket(bFiles)
		c := tx.Bucket(bQueue).Cursor()
		for k, _ := c.Seek(prefix); k != nil && len(out) < limit; k, _ = c.Next() {
			if len(k) < len(prefix) || string(k[:len(prefix)]) != string(prefix) {
				break
			}
			path := string(k[len(prefix):])
			var r Record
			if v := files.Get([]byte(path)); v == nil || json.Unmarshal(v, &r) != nil {
				continue
			}
			if r.NextTry > now.Unix() {
				continue
			}
			out = append(out, Entry{Path: path, Record: r})
		}
		return nil
	})
	return out, err
}

// Counts returns the number of records per account and status.
func (s *Store) Counts() (map[string]map[Status]int, error) {
	out := map[string]map[Status]int{}
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bFiles).ForEach(func(_, v []byte) error {
			var r Record
			if json.Unmarshal(v, &r) != nil {
				return nil
			}
			if out[r.Account] == nil {
				out[r.Account] = map[Status]int{}
			}
			out[r.Account][r.Status]++
			return nil
		})
	})
	return out, err
}

func (s *Store) GetMeta(key string, v any) (bool, error) {
	var ok bool
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bMeta).Get([]byte(key))
		if b == nil {
			return nil
		}
		ok = true
		return json.Unmarshal(b, v)
	})
	return ok, err
}

func (s *Store) SetMeta(key string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error { return tx.Bucket(bMeta).Put([]byte(key), b) })
}

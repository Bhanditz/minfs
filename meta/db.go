package meta

// Meta package maintains the caching of all meta data of the files and directories.

import (
	"errors"
	"os"
	"path/filepath"

	"gopkg.in/vmihailenco/msgpack.v2"

	"github.com/boltdb/bolt"
)

// RegisterExt -
func RegisterExt(id int8, value interface{}) interface{} {
	msgpack.RegisterExt(id, value)
	return value
}

// Open -
func Open(path string, mode os.FileMode, options *bolt.Options) (*DB, error) {
	dname := filepath.Dir(path)
	if err := os.MkdirAll(dname, 0700); err != nil {
		return nil, err
	}
	db, err := bolt.Open(path, 0600, nil)
	if err != nil {
		return nil, err
	}

	return &DB{
		db,
	}, nil

}

// DB -
type DB struct {
	*bolt.DB
}

// Begin -
func (db *DB) Begin(writable bool) (*Tx, error) {
	tx, err := db.DB.Begin(writable)
	return &Tx{tx}, err
}

// View -
func (db *DB) View(fn func(*Tx) error) error {
	return db.DB.View(func(tx *bolt.Tx) error {
		return fn(&Tx{tx})
	})
}

// Bucket -
type Bucket struct {
	InnerBucket *bolt.Bucket
}

// Bucket -
func (b *Bucket) Bucket(name string) *Bucket {
	return &Bucket{
		b.InnerBucket.Bucket([]byte(name)),
	}
}

// NextSequence -
func (b *Bucket) NextSequence() (uint64, error) {
	return b.InnerBucket.NextSequence()
}

// ForEach -
func (b *Bucket) ForEach(fn func(string, interface{}) error) error {
	return b.InnerBucket.ForEach(func(k, v []byte) error {
		if k[len(k)-1] == '/' {
			return nil
		}

		var o interface{}
		if err := msgpack.Unmarshal(v, &o); err != nil {
			return err
		}

		return fn(string(k), o)
	})
}

// CreateBucketIfNotExists -
func (b *Bucket) CreateBucketIfNotExists(key string) (*Bucket, error) {
	child, err := b.InnerBucket.CreateBucketIfNotExists([]byte(key))
	return &Bucket{child}, err
}

// Tx -
type Tx struct {
	*bolt.Tx
}

// Bucket -
func (tx *Tx) Bucket(name string) *Bucket {
	return &Bucket{
		tx.Tx.Bucket([]byte(name)),
	}
}

// ErrNoSuchObject - returned when object is not found.
var ErrNoSuchObject = errors.New("No such object.")

// IsNoSuchObject - is err ErrNoSuchObject ?
func IsNoSuchObject(err error) bool {
	if err == nil {
		return false
	}
	// Validate if the type is same as well.
	if err == ErrNoSuchObject {
		return true
	} else if err.Error() == ErrNoSuchObject.Error() {
		// Reaches here when type did not match but err string matches.
		// Someone wrapped this error? - still return true since
		// they are the same.
		return true
	}
	return false
}

// DeleteBucket -
func (b *Bucket) DeleteBucket(key string) error {
	return b.InnerBucket.DeleteBucket([]byte(key))
}

// Delete -
func (b *Bucket) Delete(key string) error {
	return b.InnerBucket.Delete([]byte(key))
}

// Get -
func (b *Bucket) Get(key string, v ...interface{}) error {
	data := b.InnerBucket.Get([]byte(key))
	if data == nil {
		return ErrNoSuchObject
	}
	return msgpack.Unmarshal(data, v...)
}

// Put -
func (b *Bucket) Put(key string, v interface{}) error {
	data, err := msgpack.Marshal(v)
	if err != nil {
		return err
	}
	return b.InnerBucket.Put([]byte(key), data)
}

/*
 * MinFS for Amazon S3 Compatible Cloud Storage (C) 2015, 2016 Minio, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */
package minfs

import (
	"fmt"
	"log"
	"net/url"
	"os"
	"os/signal"
	"path"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/boltdb/bolt"
	"github.com/minio/minfs/meta"
	"github.com/minio/minfs/queue"
	"github.com/minio/minio-go"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
)

var (
	_ = meta.RegisterExt(1, File{})
	_ = meta.RegisterExt(2, Dir{})
)

// MinFS
type MinFS struct {
	config *Config
	api    *minio.Client

	db *meta.DB

	// contains all open handles
	handles []*FileHandle

	m sync.Mutex

	queue *queue.Queue
}

func New(options ...func(*Config)) (*MinFS, error) {
	// set defaults
	cfg := &Config{
		cacheSize: 10000000,
		cache:     "./cache/",
		accountID: fmt.Sprintf("%d", time.Now().UTC().Unix()),
		gid:       0,
		uid:       0,
		mode:      os.FileMode(0660),
	}

	for _, optionFn := range options {
		optionFn(cfg)
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}

	fs := &MinFS{
		config: cfg,
	}
	return fs, nil
}

func (mfs *MinFS) stopNotificationListener() error {
	// try to set and listen for notifications
	// Fetch the bucket location.
	location, err := mfs.api.GetBucketLocation(mfs.config.bucket)
	if err != nil {
		return err
	}

	// Fetch any existing bucket notification on the bucket.
	bn, err := mfs.api.GetBucketNotification(mfs.config.bucket)
	if err != nil {
		return err
	}

	accountARN := minio.NewArn("minio", "sns", location, mfs.config.accountID, "listen")

	// Remove account ARN if any.
	bn.RemoveTopicByArn(accountARN)

	// Set back the new sets of notifications.
	err = mfs.api.SetBucketNotification(mfs.config.bucket, bn)
	if err != nil {
		return err
	}

	// Success.
	return nil
}

func (mfs *MinFS) startNotificationListener() error {
	// try to set and listen for notifications
	// Fetch the bucket location.
	location, err := mfs.api.GetBucketLocation(mfs.config.bucket)
	if err != nil {
		return err
	}

	// Fetch any existing bucket notification on the bucket.
	bn, err := mfs.api.GetBucketNotification(mfs.config.bucket)
	if err != nil {
		return err
	}

	accountARN := minio.NewArn("minio", "sns", location, mfs.config.accountID, "listen")

	// If there are no SNS topics configured, configure the first one.
	shouldSetNotification := len(bn.TopicConfigs) == 0
	if !shouldSetNotification {
		// We found previously configure SNS topics, validate if current account-id is the same.
		// this will always set shouldSetNotification right?
		for _, topicConfig := range bn.TopicConfigs {
			if topicConfig.Topic == accountARN.String() {
				shouldSetNotification = false
				break
			}
		}
	}

	if shouldSetNotification {
		topicConfig := minio.NewNotificationConfig(accountARN)
		topicConfig.AddEvents(minio.ObjectCreatedAll, minio.ObjectRemovedAll)
		bn.AddTopic(topicConfig)

		if err := mfs.api.SetBucketNotification(mfs.config.bucket, bn); err != nil {
			return err
		}
	}

	doneCh := make(chan struct{})

	// todo(nl5887): reconnect on close
	eventsCh := mfs.api.ListenBucketNotification(mfs.config.bucket, accountARN, doneCh)
	go func() {
		for notificationInfo := range eventsCh {
			if notificationInfo.Err != nil {
				continue
			}

			// Start a writable transaction.
			tx, err := mfs.db.Begin(true)
			if err != nil {
				panic(err)
			}

			defer tx.Rollback()
			// todo(nl5887): defer not called in for each
			// todo(nl5887): how to ignore my own created events?
			// can we use eventsource?

			for _, record := range notificationInfo.Records {
				key, e := url.QueryUnescape(record.S3.Object.Key)
				if e != nil {
					fmt.Print("Error:", err)
					continue
				}

				fmt.Printf("%#v", record)

				dir, _ := path.Split(key)

				b := tx.Bucket("minio")

				if v, err := b.CreateBucketIfNotExists(dir); err != nil {
					fmt.Print("Error:", err)
					continue
				} else {
					b = v
				}

				var f interface{}
				if err := b.Get(key, &f); err == nil {
				} else if !meta.IsNoSuchObject(err) {
					fmt.Println("Error:", err)
					continue
				} else if i, err := mfs.NextSequence(tx); err != nil {
					fmt.Println("Error:", err)
					continue
				} else {
					oi := record.S3.Object
					f = File{
						Size:  uint64(oi.Size),
						Inode: i,
						UID:   mfs.config.uid,
						GID:   mfs.config.gid,
						Mode:  mfs.config.mode,
						/*
							objectMeta doesn't contain those fields

							Chgtime: oi.LastModified,
							Crtime:  oi.LastModified,
							Mtime:   oi.LastModified,
							Atime:   oi.LastModified,
						*/
						Path: "/" + key,
						ETag: oi.ETag,
					}

					if err := f.(*File).store(tx); err != nil {
						fmt.Println("Error:", err)
						continue
					}
				}

			}

			// Commit the transaction and check for error.
			if err := tx.Commit(); err != nil {
				panic(err)
			}

		}
	}()

	return nil
}

func (mfs *MinFS) mount() (*fuse.Conn, error) {
	return fuse.Mount(
		mfs.config.mountpoint,
		fuse.FSName("MinFS"),
		fuse.Subtype("MinFS"), // todo: bucket? or amazon /minio?
		fuse.LocalVolume(),
		fuse.VolumeName(mfs.config.bucket), // bucket?
	)
}

func (mfs *MinFS) Serve() error {
	if mfs.config.debug {
		fuse.Debug = func(msg interface{}) {
			fmt.Printf("%#v\n", msg)
		}
	}

	// initialize
	fmt.Println("Opening cache database...")
	if db, err := meta.Open(path.Join(mfs.config.cache, "cache.db"), 0600, nil); err != nil {
		return err
	} else {
		mfs.db = db
	}

	defer mfs.db.Close()

	fmt.Println("Initializing cache database...")
	mfs.db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists([]byte("minio"))
		return err
	})

	fmt.Println("Initializing minio client...")

	host := mfs.config.target.Host
	access := mfs.config.target.User.Username()
	secret, _ := mfs.config.target.User.Password()
	secure := (mfs.config.target.Scheme == "https")
	if api, err := minio.New(host, access, secret, secure); err != nil {
		return err
	} else {
		mfs.api = api
	}

	// set notifications
	fmt.Println("Starting notification listener...")
	if err := mfs.startNotificationListener(); err != nil {
		return err
	}

	// we are doing an initial scan of the filesystem
	fmt.Println("Scanning source bucket....")
	if err := mfs.scan("/"); err != nil {
		return err
	}

	go func() {
		// have a channel doing all get operations
	}()

	go func() {
		// have a channel doing all put operations
	}()

	fmt.Println("Mounting target....")
	// mount the drive
	c, err := mfs.mount()
	if err != nil {
		return err
	}

	defer c.Close()

	fmt.Println("Mounted... Have fun!")
	// serve the filesystem
	if err := fs.Serve(c, mfs); err != nil {
		return err
	}

	// todo(nl5887): implement this
	fmt.Println("HOW TO QUIT?")

	// todo(nl5887): move trap signals to Main, this is not supposed to be in Serve
	signalCh := make(chan os.Signal, 1)
	signal.Notify(signalCh, os.Interrupt, syscall.SIGUSR1)

loop:
	for {
		// check if the mount process has an error to report
		select {
		case <-c.Ready:
			if err := c.MountError; err != nil {
				log.Fatal(err)
			}
		case s := <-signalCh:
			if s == os.Interrupt {
				return mfs.stopNotificationListener()
			} else if s == syscall.SIGUSR1 {
				fmt.Println("PRINT STATS")
				continue
			}
			break loop
		}
	}

	return nil
}

func (mfs *MinFS) NewHandle(f *File) *FileHandle {
	mfs.m.Lock()
	defer mfs.m.Unlock()

	h := &FileHandle{
		f: f,
	}

	mfs.handles = append(mfs.handles, h)

	h.handle = uint64(len(mfs.handles))
	return h
}

// NextSequence will return the next free iNode
func (mfs *MinFS) NextSequence(tx *meta.Tx) (sequence uint64, err error) {
	bucket := tx.Bucket("minio")
	return bucket.NextSequence()
}

// Root is the root folder of the MinFS mountpoint
func (mfs *MinFS) Root() (fs.Node, error) {
	return &Dir{
		mfs:  mfs,
		Mode: os.ModeDir | 0555,
		Path: "/",
	}, nil
}

func (mfs *MinFS) scan(p string) error {
	tx, err := mfs.db.Begin(true)
	if err != nil {
		return err
	}

	defer tx.Rollback()

	b := tx.Bucket("minio")

	if child, err := b.CreateBucketIfNotExists(p); err != nil {
		return err
	} else {
		b = child
	}

	doneCh := make(chan struct{})
	defer close(doneCh)

	ch := mfs.api.ListObjectsV2(mfs.config.bucket, p[1:], false, doneCh)

	for message := range ch {
		key := message.Key

		if strings.HasSuffix(key, "/") {
			var d Dir
			if err := b.Get(key, &d); err == nil {
			} else if !meta.IsNoSuchObject(err) {
				return err
			} else if i, err := mfs.NextSequence(tx); err != nil {
				return err
			} else {
				// todo(nl5887): check if we need to update, and who'll win?
				d = Dir{
					Path:  "/" + key,
					Inode: i,

					Mode: 0770 | os.ModeDir,
					GID:  mfs.config.gid,
					UID:  mfs.config.uid,

					Chgtime: message.LastModified,
					Crtime:  message.LastModified,
					Mtime:   message.LastModified,
					Atime:   message.LastModified,
				}

				if err := b.Put(path.Base(key), &d); err != nil {
					return err
				}
			}
		} else {
			var f File
			if err := b.Get(key, &f); err == nil {
			} else if !meta.IsNoSuchObject(err) {
				return err
			} else if i, err := mfs.NextSequence(tx); err != nil {
				return err
			} else {
				// todo(nl5887): check if we need to update, and who'll win?
				f = File{
					Size:    uint64(message.Size),
					Inode:   i,
					Mode:    mfs.config.mode,
					GID:     mfs.config.gid,
					UID:     mfs.config.uid,
					Chgtime: message.LastModified,
					Crtime:  message.LastModified,
					Mtime:   message.LastModified,
					Atime:   message.LastModified,
					Path:    "/" + key,
					ETag:    message.ETag,
				}

				if err := f.store(tx); err != nil {
					return err
				}
			}
		}
	}

	return tx.Commit()
}

type Storer interface {
	store(tx *meta.Tx)
}

package minfs

import (
	"fmt"
	"os"
	"path"
	"strings"
	"time"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	"github.com/minio/minfs/meta"
	"golang.org/x/net/context"
)

// Dir implements both Node and Handle for the root directory.
type Dir struct {
	mfs *MinFS

	parent *Dir

	Path  string
	Inode uint64
	Mode  os.FileMode

	Size uint64
	ETag string

	Atime time.Time
	Mtime time.Time

	UID uint32
	GID uint32

	// OS X only
	Bkuptime time.Time
	Chgtime  time.Time
	Crtime   time.Time
	Flags    uint32 // see chflags(2)
}

// Attr returns the attributes for the directory
func (dir *Dir) Attr(ctx context.Context, a *fuse.Attr) error {
	*a = fuse.Attr{
		Inode: dir.Inode,
		Size:  dir.Size,
		/*
		   Blocks    :dir.Size / 512,
		   Nlink     : 1,
		   BlockSize : 512,
		*/
		Atime:  dir.Atime,
		Mtime:  dir.Mtime,
		Ctime:  dir.Chgtime,
		Crtime: dir.Crtime,
		Mode:   dir.Mode,
		Uid:    dir.UID,
		Gid:    dir.GID,
		Flags:  dir.Flags,
	}

	return nil
}

// todo(nl5887): implement cancel
// todo(nl5887): implement rename
// todo(nl5887): buckets in buckets in buckets? or just subbuckets in minio bucket?

// Lookup returns the directory node
func (dir *Dir) Lookup(ctx context.Context, name string) (fs.Node, error) {
	// todo: make sure that we know of the folder, when not yet initialized
	if err := dir.scan(); err != nil {
		return nil, err
	}

	var o interface{} // meta.Object
	if err := dir.mfs.db.View(func(tx *meta.Tx) error {
		b := dir.bucket(tx)
		return b.Get(name, &o)
	}); err == nil {
	} else if true /* todo(nl5887): check for no such object */ {
		return nil, fuse.ENOENT
	} else if err != nil {
		return nil, err
	}

	if file, ok := o.(File); ok {
		file.mfs = dir.mfs
		file.dir = dir
		return &file, nil
	} else if subdir, ok := o.(Dir); ok {
		subdir.mfs = dir.mfs
		subdir.parent = dir
		return &subdir, nil
	}

	return nil, fuse.ENOENT
}

// FullPath returns the full path including parent paths for current dir
func (dir *Dir) FullPath() string {
	fullPath := ""

	p := dir
	for {
		if p == nil {
			break
		}

		fullPath = path.Join(p.Path, fullPath)

		p = p.parent
	}

	fmt.Println(fullPath)
	return fullPath
}

func (dir *Dir) scan() error {
	tx, err := dir.mfs.db.Begin(true)
	if err != nil {
		return err
	}

	defer tx.Rollback()

	b := dir.bucket(tx)

	doneCh := make(chan struct{})
	defer close(doneCh)

	prefix := dir.FullPath()
	if prefix != "" {
		prefix = prefix + "/"
	}

	ch := dir.mfs.api.ListObjectsV2(dir.mfs.config.bucket, prefix, false, doneCh)
	for message := range ch {
		key := message.Key

		fmt.Println("scan", key)

		if strings.HasSuffix(key, "/") {
			// todo(nl5887): remove "/"

			var d Dir
			if err := b.Get(key, &d); err == nil {
			} else if !meta.IsNoSuchObject(err) {
				return err
			} else if i, err := dir.mfs.NextSequence(tx); err != nil {
				return err
			} else {
				// todo(nl5887): check if we need to update, and who'll win?
				d = Dir{
					parent: dir,

					Path:  path.Base(key),
					Inode: i,

					Mode: 0770 | os.ModeDir,
					GID:  dir.mfs.config.gid,
					UID:  dir.mfs.config.uid,

					Chgtime: message.LastModified,
					Crtime:  message.LastModified,
					Mtime:   message.LastModified,
					Atime:   message.LastModified,
				}

				if err := d.store(tx); err != nil {
					return err
				}
			}
		} else {
			var f File
			if err := b.Get(key, &f); err == nil {
			} else if !meta.IsNoSuchObject(err) {
				return err
			} else if i, err := dir.mfs.NextSequence(tx); err != nil {
				return err
			} else {
				// todo(nl5887): check if we need to update, and who'll win?
				f = File{
					dir:  dir,
					Path: path.Base(key),

					Size:    uint64(message.Size),
					Inode:   i,
					Mode:    dir.mfs.config.mode,
					GID:     dir.mfs.config.gid,
					UID:     dir.mfs.config.uid,
					Chgtime: message.LastModified,
					Crtime:  message.LastModified,
					Mtime:   message.LastModified,
					Atime:   message.LastModified,
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

// ReadDirAll will return all files in current dir
func (dir *Dir) ReadDirAll(ctx context.Context) ([]fuse.Dirent, error) {
	// if not exists then scan
	// todo(nl5887): should we keep last scan date? do periodic scan to update cache?
	if err := dir.scan(); err != nil {
		return nil, err
	}

	// cache only doesn't need writable transaction
	// update cache folder with bucket list
	tx, err := dir.mfs.db.Begin(false)
	if err != nil {
		return nil, err
	}

	defer tx.Rollback()

	b := dir.bucket(tx)

	var entries = []fuse.Dirent{}

	// todo(nl5887): use make([]fuse.Dirent{}, count)
	if err := b.ForEach(func(k string, o interface{}) error {
		if file, ok := o.(File); ok {
			file.dir = dir
			entries = append(entries, file.Dirent())
		} else if subdir, ok := o.(Dir); ok {
			subdir.parent = dir
			entries = append(entries, subdir.Dirent())
		} else {
			panic("Could not find type. Try to remove cache.")
		}

		return nil
	}); err != nil {
		return nil, err
	}

	return entries, nil
}

func (dir *Dir) bucket(tx *meta.Tx) *meta.Bucket {
	// root
	if dir.parent == nil {
		return tx.Bucket("minio/")
	}

	b := dir.parent.bucket(tx)
	return b.Bucket(dir.Path + "/")
}

// Mkdir will make a new directory below current dir
func (dir *Dir) Mkdir(ctx context.Context, req *fuse.MkdirRequest) (fs.Node, error) {
	subdir := Dir{
		parent: dir,
		mfs:    dir.mfs,

		Path: req.Name,

		Mode: 0770 | os.ModeDir,
		GID:  dir.mfs.config.gid,
		UID:  dir.mfs.config.uid,

		Chgtime: time.Now(),
		Crtime:  time.Now(),
		Mtime:   time.Now(),
		Atime:   time.Now(),
	}

	tx, err := dir.mfs.db.Begin(true)
	if err != nil {
		return nil, err
	}

	defer tx.Rollback()

	if err := subdir.store(tx); err != nil {
		return nil, err
	}

	// Commit the transaction and check for error.
	if err := tx.Commit(); err != nil {
		return nil, err
	}

	return &subdir, nil
}

func (dir *Dir) wait(path string) error {
	// todo(nl5887): should we add mutex here? We cannot use mfs.m mutex,
	// because that will create deadlock

	// check if the file is locked, and wait for max 5 seconds for the file to be
	// acquired
	for i := 0; ; /* retries */ i++ {
		if !dir.mfs.IsLocked(path) {
			break
		}

		if i > 25 /* max number of retries */ {
			return fuse.EPERM
		}

		time.Sleep(time.Millisecond * 200)
	}

	return nil
}

// Remove will delete a file or directory from current directory
func (dir *Dir) Remove(ctx context.Context, req *fuse.RemoveRequest) error {
	if err := dir.wait(req.Name); err != nil {
		return err
	}

	tx, err := dir.mfs.db.Begin(true)
	if err != nil {
		return err
	}

	defer tx.Rollback()

	b := dir.bucket(tx)

	var o interface{}
	if err := b.Get(req.Name, &o); err != nil {
		return err
	} else if f, ok := o.(*File); ok {
		f.dir = dir
	} else if d, ok := o.(*Dir); ok {
		d.parent = dir
	}

	if err := b.Delete(req.Name); err == nil {
	} else if meta.IsNoSuchObject(err) {
		return fuse.ENOENT
	} else if err != nil {
		return err
	}

	if req.Dir {
		b.DeleteBucket(req.Name + "/")
	}

	if err := dir.mfs.api.RemoveObject(dir.mfs.config.bucket, path.Join(dir.FullPath(), req.Name)); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	return nil
}

func (dir *Dir) store(tx *meta.Tx) error {
	// directories will be stored in their parent buckets
	b := dir.parent.bucket(tx)

	subbucketPath := path.Base(dir.Path)
	if _, err := b.CreateBucketIfNotExists(subbucketPath + "/"); err != nil {
		return err
	}

	return b.Put(subbucketPath, dir)
}

// Dirent will return the fuse Dirent for current dir
func (dir *Dir) Dirent() fuse.Dirent {
	return fuse.Dirent{
		Inode: dir.Inode, Name: path.Base(dir.Path), Type: fuse.DT_Dir,
	}
}

// Create will return a new empty file in current dir, if the file is currently locked, it will
// wait for the lock to be freed.
func (dir *Dir) Create(ctx context.Context, req *fuse.CreateRequest, resp *fuse.CreateResponse) (fs.Node, fs.Handle, error) {
	if err := dir.wait(req.Name); err != nil {
		return nil, nil, err
	}

	tx, err := dir.mfs.db.Begin(true)
	if err != nil {
		return nil, nil, err
	}

	defer tx.Rollback()

	b := dir.bucket(tx)

	name := req.Name

	var f File
	if err := b.Get(name, &f); err == nil {
		f.dir = dir
	} else if i, err := dir.mfs.NextSequence(tx); err != nil {
		return nil, nil, err
	} else {
		f = File{
			mfs: dir.mfs,
			dir: dir,

			Size:    uint64(0),
			Inode:   i,
			Path:    req.Name,
			Mode:    req.Mode, // dir.mfs.config.mode, // should we use same mode for scan?
			UID:     dir.mfs.config.uid,
			GID:     dir.mfs.config.gid,
			Chgtime: time.Now().UTC(),
			Crtime:  time.Now().UTC(),
			Mtime:   time.Now().UTC(),
			Atime:   time.Now().UTC(),
			ETag:    "",

			// req.Umask
		}
	}

	if err := f.store(tx); err != nil {
		return nil, nil, err
	}

	var fh *FileHandle
	if v, err := dir.mfs.Acquire(&f); err != nil {
		return nil, nil, err
	} else {
		fh = v
	}

	fh.dirty = true

	if f, err := os.Create(fh.cachePath); err == nil {
		fh.File = f
	} else if err != nil {
		return nil, nil, err
	}

	// Commit the transaction and check for error.
	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}

	resp.Handle = fuse.HandleID(fh.handle)
	return &f, fh, nil
}

package minfs

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path"
	"time"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	"github.com/minio/minfs/meta"
	"golang.org/x/net/context"
)

// File implements both Node and Handle for the hello file.
type File struct {
	mfs *MinFS

	Path string

	Inode uint64

	Mode os.FileMode

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

	Hash []byte
}

func (file *File) store(tx *meta.Tx) error {
	b := tx.Bucket("minio")

	subbucketPath := path.Dir(file.Path)
	if b, err := b.CreateBucketIfNotExists(subbucketPath); err != nil {
		return err
	} else {
		return b.Put(path.Base(file.Path), file)
	}
}

func (file *File) Forget() {
	fmt.Println("Forget")
}

func (file *File) Attr(ctx context.Context, a *fuse.Attr) error {
	*a = fuse.Attr{
		Inode: file.Inode,
		Size:  file.Size,
		/*
		   Blocks    :file.Size / 512,
		   Nlink     : 1,
		   BlockSize : 512,
		*/
		Atime:  file.Atime,
		Mtime:  file.Mtime,
		Ctime:  file.Chgtime,
		Crtime: file.Crtime,
		Mode:   file.Mode,
		Uid:    file.UID,
		Gid:    file.GID,
		Flags:  file.Flags,
	}

	return nil
}

func (f *File) Setattr(ctx context.Context, req *fuse.SetattrRequest, resp *fuse.SetattrResponse) error {
	tx, err := f.mfs.db.Begin(true)
	if err != nil {
		return err
	}

	defer tx.Rollback()

	b := tx.Bucket("minio")

	subbucketPath := path.Dir(f.Path)
	b, err = b.CreateBucketIfNotExists(subbucketPath)
	if err != nil {
		fmt.Println("Bucket not exists", subbucketPath)
		return err
	}

	// update cache
	if req.Valid.Mode() {
		f.Mode = req.Mode
	}

	if req.Valid.Uid() {
		f.UID = req.Uid
	}

	if req.Valid.Gid() {
		f.GID = req.Gid
	}

	if req.Valid.Size() {
		f.Size = req.Size
		fmt.Println("UPDATED SIZE", req.Size)
	}

	if req.Valid.Atime() {
		f.Atime = req.Atime
	}

	if req.Valid.Mtime() {
		f.Mtime = req.Mtime
	}

	if req.Valid.Handle() {
		// todo(nl5887): what is this?
		// f.Handle = req.Handle
	}

	/*
			if req.Valid&fuse.SetattrAtimeNow == fuse.SetattrAtimeNow {
				f.AtimeNow = req.AtimeNow
			}

			if req.Valid&fuse.SetattrMtimeNow == fuse.SetattrMtimeNow {
				f.MtimeNow = req.MtimeNow
			}

		if req.Valid&fuse.SetattrLockOwner == fuse.SetattrLockOwner {
			f.LockOwner = req.LockOwner
		}
	*/

	if req.Valid.Crtime() {
		f.Crtime = req.Crtime
	}

	if req.Valid.Chgtime() {
		f.Chgtime = req.Chgtime
	}

	if req.Valid.Bkuptime() {
		f.Bkuptime = req.Bkuptime
	}

	if req.Valid.Flags() {
		f.Flags = req.Flags
	}

	if err := f.store(tx); err != nil {
		return err
	}

	// Commit the transaction and check for error.
	if err := tx.Commit(); err != nil {
		return err
	}

	fmt.Printf("Setattr %#v\n", resp.Attr)
	//pretty.Print(f)

	return nil
}

func (f *File) Fsync(ctx context.Context, req *fuse.FsyncRequest) error {
	return nil
}

func (file *File) Open(ctx context.Context, req *fuse.OpenRequest, resp *fuse.OpenResponse) (fs.Handle, error) {
	cachePath := path.Join(file.mfs.config.cache, path.Base(file.Path))

	if _, err := os.Stat(cachePath); err == nil {
	} else if os.IsNotExist(err) {
		object, err := file.mfs.api.GetObject(file.mfs.config.bucket, file.Path[1:])

		if err != nil /* No such object*/ {
			return nil, fuse.ENOENT
		} else if err != nil {
			return nil, err
		}
		defer object.Close()

		// Start a writable transaction.
		tx, err := file.mfs.db.Begin(true)
		if err != nil {
			return nil, err
		}

		defer tx.Rollback()

		hasher := sha256.New()

		var r io.Reader = object
		r = io.TeeReader(r, hasher)

		// todo(nl5887): do we want to have original filenames? or hashes of the filename
		f, err := os.Create(cachePath)
		if err != nil {
			return nil, err
		}

		defer f.Close()

		if _, err := io.Copy(f, r); err != nil {
			return nil, err
		}

		// todo(nl5887): do we want to save as hashes? this will deduplicate files in cache file
		// and also introduces some kind of versioning, hasher can be saved in filehandle
		fmt.Printf("Sum: %#v\n", hasher.Sum(nil))

		file.Hash = hasher.Sum(nil)

		if err := file.store(tx); err != nil {
			return nil, err
		}

		// Commit the transaction and check for error.
		if err := tx.Commit(); err != nil {
			return nil, err
		}

	} else {
		return nil, err
	}

	// update cache bucket!

	fh := file.mfs.NewHandle(file)

	if f, err := os.OpenFile(cachePath, int(req.Flags), file.mfs.config.mode); err != nil {
		return nil, err
	} else {
		fh.File = f
	}

	resp.Handle = fuse.HandleID(fh.handle)
	return fh, nil
}

func (file *File) bucket(tx *meta.Tx) *meta.Bucket {
	b := tx.Bucket("minio")
	return b.Bucket(path.Base(file.Path))
}

func (file *File) Rename(ctx context.Context, req *fuse.RenameRequest, newDir fs.Node) error {
	fmt.Println("RENAME")

	// copy and delete the old one?
	return fmt.Errorf("Rename is not supported.")
}

func (file *File) Getattr(ctx context.Context, req *fuse.GetattrRequest, resp *fuse.GetattrResponse) error {
	resp.Attr = fuse.Attr{
		Inode: file.Inode,
		Size:  file.Size,
		/*
		   Blocks    :file.Size / 512,
		   Nlink     : 1,
		   BlockSize : 512,
		*/
		Atime:  file.Atime,
		Mtime:  file.Mtime,
		Ctime:  file.Chgtime,
		Crtime: file.Crtime,
		Mode:   file.Mode,
		Uid:    file.UID,
		Gid:    file.GID,
		Flags:  file.Flags,
	}

	fmt.Printf("Getattr %#v\n", resp.Attr)

	return nil
}

func (file *File) Dirent() fuse.Dirent {
	return fuse.Dirent{
		Inode: file.Inode, Name: path.Base(file.Path), Type: fuse.DT_File,
	}
}

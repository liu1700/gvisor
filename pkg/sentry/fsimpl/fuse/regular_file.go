// Copyright 2020 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package fuse

import (
	"math"
	"sync"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/safemem"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
	"gvisor.dev/gvisor/pkg/usermem"
)

// +stateify savable
type regularFileFD struct {
	fileDescription

	// offMu protects off.
	offMu sync.Mutex `state:"nosave"`

	// off is the file offset.
	// +checklocks:offMu
	off int64

	// Protected by inode.dataMu. A transferred handle outlives VFS Release
	// until the inode has written its dirty pages.
	handleTransferred bool
}

// Allocate implements vfs.FileDescriptionImpl.Allocate.
func (fd *regularFileFD) Allocate(ctx context.Context, mode, offset, length uint64) error {
	if mode & ^uint64(linux.FALLOC_FL_KEEP_SIZE|linux.FALLOC_FL_PUNCH_HOLE|linux.FALLOC_FL_ZERO_RANGE) != 0 {
		return linuxerr.EOPNOTSUPP
	}
	in := linux.FUSEFallocateIn{
		Fh:     fd.Fh,
		Offset: offset,
		Length: length,
		Mode:   uint32(mode),
	}
	i := fd.inode()
	i.attrMu.Lock()
	defer i.attrMu.Unlock()
	i.dataMu.Lock()
	if err := i.syncMappedLocked(ctx); err != nil {
		i.dataMu.Unlock()
		return err
	}
	if err := i.call(ctx, linux.FUSE_FALLOCATE, &in, nil); err != nil {
		i.dataMu.Unlock()
		return err
	}
	if mode&(linux.FALLOC_FL_PUNCH_HOLE|linux.FALLOC_FL_ZERO_RANGE) != 0 {
		r := memmap.MappableRange{Start: offset, End: offset + length}
		for seg := i.cache.LowerBoundSegment(r.Start); seg.Ok() && seg.Start() < r.End; seg, _ = seg.NextNonEmpty() {
			blocks, err := i.fs.mf.MapInternal(seg.FileRangeOf(seg.Range().Intersect(r)), hostarch.Write)
			if err != nil {
				i.dataMu.Unlock()
				return err
			}
			if _, err := safemem.ZeroSeq(blocks); err != nil {
				i.dataMu.Unlock()
				return err
			}
		}
	}
	size := i.size.Load()
	if mode&linux.FALLOC_FL_KEEP_SIZE == 0 && offset+length > size {
		size = offset + length
	}
	i.updateSizeAndUnlockData(ctx, size)
	i.fs.conn.attributeVersion.Add(1)
	i.touchCMtime()

	return nil
}

// Seek implements vfs.FileDescriptionImpl.Seek.
func (fd *regularFileFD) Seek(ctx context.Context, offset int64, whence int32) (int64, error) {
	fd.offMu.Lock()
	defer fd.offMu.Unlock()
	inode := fd.inode()
	inode.attrMu.Lock()
	defer inode.attrMu.Unlock()
	switch whence {
	case linux.SEEK_SET:
		// use offset as specified
	case linux.SEEK_CUR:
		offset += fd.off
	case linux.SEEK_END:
		offset += int64(inode.size.Load())
	default:
		return 0, linuxerr.EINVAL
	}
	if offset < 0 {
		return 0, linuxerr.EINVAL
	}
	fd.off = offset
	return offset, nil
}

// PRead implements vfs.FileDescriptionImpl.PRead.
func (fd *regularFileFD) PRead(ctx context.Context, dst usermem.IOSequence, offset int64, opts vfs.ReadOptions) (int64, error) {
	if offset < 0 {
		return 0, linuxerr.EINVAL
	}

	// Check that flags are supported.
	//
	// TODO(gvisor.dev/issue/2601): Support select preadv2 flags.
	if opts.Flags&^linux.RWF_HIPRI != 0 {
		return 0, linuxerr.EOPNOTSUPP
	}

	size := dst.NumBytes()
	if size == 0 {
		// Early return if count is 0.
		return 0, nil
	}
	if size > math.MaxUint32 {
		// FUSE only supports uint32 for size.
		// Overflow.
		return 0, linuxerr.EINVAL
	}

	i := fd.inode()
	if !fd.DirectIO {
		i.attrMu.Lock()
		if uint64(offset)+uint64(size) > i.size.Load() {
			if err := i.reviseAttr(ctx, linux.FUSE_GETATTR_FH, fd.Fh); err != nil {
				i.attrMu.Unlock()
				return 0, err
			}
		}
		i.touchAtime()
		i.attrMu.Unlock()
	}
	rw := fileReadWriter{ctx: ctx, fd: fd, i: i, off: uint64(offset)}
	return dst.CopyOutFrom(ctx, &rw)

}

// Read implements vfs.FileDescriptionImpl.Read.
func (fd *regularFileFD) Read(ctx context.Context, dst usermem.IOSequence, opts vfs.ReadOptions) (int64, error) {
	fd.offMu.Lock()
	n, err := fd.PRead(ctx, dst, fd.off, opts)
	fd.off += n
	fd.offMu.Unlock()
	return n, err
}

// PWrite implements vfs.FileDescriptionImpl.PWrite.
func (fd *regularFileFD) PWrite(ctx context.Context, src usermem.IOSequence, offset int64, opts vfs.WriteOptions) (int64, error) {
	n, _, err := fd.pwrite(ctx, src, offset, opts)
	return n, err
}

// Write implements vfs.FileDescriptionImpl.Write.
func (fd *regularFileFD) Write(ctx context.Context, src usermem.IOSequence, opts vfs.WriteOptions) (int64, error) {
	fd.offMu.Lock()
	n, off, err := fd.pwrite(ctx, src, fd.off, opts)
	fd.off = off
	fd.offMu.Unlock()
	return n, err
}

// pwrite returns the number of bytes written, final offset and error. The
// final offset should be ignored by PWrite.
func (fd *regularFileFD) pwrite(ctx context.Context, src usermem.IOSequence, offset int64, opts vfs.WriteOptions) (int64, int64, error) {
	if offset < 0 {
		return 0, offset, linuxerr.EINVAL
	}

	// Check that flags are supported.
	//
	// TODO(gvisor.dev/issue/2601): Support select preadv2 flags.
	if opts.Flags&^linux.RWF_HIPRI != 0 {
		return 0, offset, linuxerr.EOPNOTSUPP
	}

	inode := fd.inode()
	inode.attrMu.Lock()
	defer inode.attrMu.Unlock()

	// If the file is opened with O_APPEND, update offset to file size.
	// Note: since our Open() implements the interface of kernfs,
	// and kernfs currently does not support O_APPEND, this will never
	// be true before we switch out from kernfs.
	if fd.vfsfd.StatusFlags()&linux.O_APPEND != 0 {
		// Locking inode.metadataMu is sufficient for reading size
		offset = int64(inode.size.Load())
	}

	srclen := src.NumBytes()
	if srclen > math.MaxUint32 {
		// FUSE only supports uint32 for size.
		// Overflow.
		return 0, offset, linuxerr.EINVAL
	}
	if end := offset + srclen; end < offset {
		// Overflow.
		return 0, offset, linuxerr.EINVAL
	}

	limit, err := vfs.CheckLimit(ctx, offset, srclen)
	if err != nil {
		return 0, offset, err
	}
	if limit == 0 {
		// Return before causing any side effects.
		return 0, offset, nil
	}
	src = src.TakeFirst64(limit)
	rw := fileReadWriter{ctx: ctx, fd: fd, i: inode, off: uint64(offset)}
	n, err := src.CopyInTo(ctx, &rw)
	offset = int64(rw.off)
	if n != 0 {
		inode.fs.conn.attributeVersion.Add(1)
	}

	inode.touchCMtime()

	// As on Linux, writing clears the setuid and setgid bits.
	if n > 0 {
		if sidErr := inode.clearSUIDAndSGID(ctx, auth.CredentialsFromContext(ctx), fhOptions{useFh: true, fh: fd.Fh}); sidErr != nil && err == nil {
			return n, offset, sidErr
		}
	}
	return n, offset, err
}

// ConfigureMMap implements vfs.FileDescriptionImpl.ConfigureMMap.
func (fd *regularFileFD) ConfigureMMap(ctx context.Context, opts *memmap.MMapOpts) error {
	if fd.DirectIO && !opts.Private {
		return linuxerr.ENODEV
	}
	opts.SentryOwnedContent = true
	return vfs.GenericConfigureMMap(&fd.vfsfd, fd, opts)
}

func (fd *regularFileFD) AddMapping(ctx context.Context, ms memmap.MappingSpace, ar hostarch.AddrRange, offset uint64, writable bool) error {
	return fd.mmapAddMapping(ctx, ms, ar, offset, writable)
}
func (fd *regularFileFD) RemoveMapping(ctx context.Context, ms memmap.MappingSpace, ar hostarch.AddrRange, offset uint64, writable bool) {
	fd.mmapRemoveMapping(ctx, ms, ar, offset, writable)
}
func (fd *regularFileFD) CopyMapping(ctx context.Context, ms memmap.MappingSpace, srcAR, dstAR hostarch.AddrRange, offset uint64, writable bool) error {
	return fd.mmapCopyMapping(ctx, ms, srcAR, dstAR, offset, writable)
}
func (fd *regularFileFD) Translate(ctx context.Context, required, optional memmap.MappableRange, at hostarch.AccessType) ([]memmap.Translation, error) {
	return fd.mmapTranslate(ctx, required, optional, at)
}

// All translations use the sentry memory file, whose pages are saved along
// with the inode cache and the FUSE connection. No daemon I/O is needed while
// application tasks are stopped for checkpointing.
func (fd *regularFileFD) InvalidateUnsavable(context.Context) error { return nil }

// Sync writes shared mapped pages before asking the daemon to sync the file.
func (fd *regularFileFD) Sync(ctx context.Context, opts vfs.SyncOptions) error {
	if err := fd.inode().syncMapped(ctx, fd); err != nil {
		return err
	}
	return fd.fileDescription.Sync(ctx, opts)
}

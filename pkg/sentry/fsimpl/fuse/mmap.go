// Copyright 2026 The gVisor Authors.
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
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/safemem"
	"gvisor.dev/gvisor/pkg/sentry/fsutil"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/usage"
	"io"
)

func (fd *regularFileFD) mmapAddMapping(ctx context.Context, ms memmap.MappingSpace, ar hostarch.AddrRange, offset uint64, writable bool) error {
	i := fd.inode()
	i.mapsMu.Lock()
	defer i.mapsMu.Unlock()
	for _, r := range i.mappings.AddMapping(ms, ar, offset, writable) {
		i.fs.mf.MarkUnevictable(i, pgalloc.EvictableRange{Start: r.Start, End: r.End})
	}
	return nil
}

func (fd *regularFileFD) mmapRemoveMapping(ctx context.Context, ms memmap.MappingSpace, ar hostarch.AddrRange, offset uint64, writable bool) {
	i := fd.inode()
	i.mapsMu.Lock()
	i.dataMu.Lock()
	for _, r := range i.mappings.RemoveMapping(ms, ar, offset, writable) {
		i.dirty.AllowClean(r)
		i.fs.mf.MarkEvictable(i, pgalloc.EvictableRange{Start: r.Start, End: r.End})
	}
	i.dataMu.Unlock()
	i.mapsMu.Unlock()
}

func (fd *regularFileFD) mmapCopyMapping(ctx context.Context, ms memmap.MappingSpace, srcAR, dstAR hostarch.AddrRange, offset uint64, writable bool) error {
	return fd.mmapAddMapping(ctx, ms, dstAR, offset, writable)
}

func maxMmapFill(required, optional memmap.MappableRange) memmap.MappableRange {
	const max = 64 << 10
	if required.Length() >= max {
		return required
	}
	if optional.Length() <= max {
		return optional
	}
	optional.Start = required.Start
	if optional.Length() > max {
		optional.End = optional.Start + max
	}
	return optional
}

// mmapTranslate supplies sentry-owned pages. fd remains live because it is the
// VMA's MappingIdentity, so fd.Fh cannot be released during this call.
func (fd *regularFileFD) mmapTranslate(ctx context.Context, required, optional memmap.MappableRange, at hostarch.AccessType) ([]memmap.Translation, error) {
	i := fd.inode()
	mf := i.fs.mf
	if mf == nil {
		return nil, &memmap.BusError{linuxerr.ENODEV}
	}
	i.dataMu.Lock()
	defer i.dataMu.Unlock()
	if at.Write && i.writebackFD == nil {
		i.retainWritebackLocked(fd)
	}
	pgend, _ := hostarch.PageRoundUp(i.size.Load())
	beyond := false
	if required.End > pgend {
		if required.Start >= pgend {
			return nil, &memmap.BusError{io.EOF}
		}
		required.End = pgend
		beyond = true
	}
	if optional.End > pgend {
		optional.End = pgend
	}
	_, fillErr := i.cache.Fill(ctx, required, maxMmapFill(required, optional), i.size.Load(), mf, pgalloc.AllocOpts{
		Kind: usage.PageCache, MemCgID: pgalloc.MemoryCgroupIDFromContext(ctx), Mode: pgalloc.AllocateAndWritePopulate,
	}, fd.readToBlocksAt)
	var out []memmap.Translation
	var end uint64
	for seg := i.cache.FindSegment(required.Start); seg.Ok() && seg.Start() < required.End; seg, _ = seg.NextNonEmpty() {
		r := seg.Range().Intersect(optional)
		perms := hostarch.ReadExecute
		if at.Write {
			i.dirty.KeepDirty(r)
			perms.Write = true
		}
		out = append(out, memmap.Translation{Source: r, File: mf, Offset: seg.FileRangeOf(r).Start, Perms: perms})
		end = r.End
	}
	if end < required.End && fillErr != nil {
		return out, &memmap.BusError{fillErr}
	}
	if beyond {
		return out, &memmap.BusError{io.EOF}
	}
	return out, nil
}

// fileReadWriter copies user memory only after usermem has resolved its pages.
// This keeps faults on the same inode outside dataMu.
type fileReadWriter struct {
	ctx context.Context
	fd  *regularFileFD
	i   *inode
	off uint64
}

func (rw *fileReadWriter) ReadToBlocks(dsts safemem.BlockSeq) (uint64, error) {
	if dsts.IsEmpty() {
		return 0, nil
	}
	if rw.fd.DirectIO {
		n, err := rw.fd.readToBlocksAt(rw.ctx, dsts, rw.off)
		rw.off += n
		return n, err
	}
	i := rw.i
	i.dataMu.Lock()
	defer i.dataMu.Unlock()
	end := i.size.Load()
	if rw.off >= end {
		return 0, io.EOF
	}
	if n := dsts.NumBytes(); n < end-rw.off {
		end = rw.off + n
	}
	var done uint64
	seg, gap := i.cache.Find(rw.off)
	for rw.off < end {
		mr := memmap.MappableRange{Start: rw.off, End: end}
		var n uint64
		var err error
		var want uint64
		if seg.Ok() {
			r := seg.Range().Intersect(mr)
			want = r.Length()
			var blocks safemem.BlockSeq
			blocks, err = i.fs.mf.MapInternal(seg.FileRangeOf(r), hostarch.Read)
			if err == nil {
				n, err = safemem.CopySeq(dsts.TakeFirst64(want), blocks)
			}
		} else {
			want = gap.Range().Intersect(mr).Length()
			// Ordinary reads do not allocate sentry pages. Only mmap fills the cache.
			n, err = rw.fd.readToBlocksAt(rw.ctx, dsts.TakeFirst64(want), rw.off)
		}
		done += n
		rw.off += n
		dsts = dsts.DropFirst64(n)
		if n != want || err != nil {
			return done, err
		}
		seg, gap = i.cache.Find(rw.off)
	}
	return done, nil
}

func (rw *fileReadWriter) WriteFromBlocks(srcs safemem.BlockSeq) (uint64, error) {
	if srcs.IsEmpty() {
		return 0, nil
	}
	i := rw.i
	i.dataMu.Lock()
	defer i.dataMu.Unlock()
	// Write the requested bytes through to the daemon. Update cached pages in
	// place so concurrent MAP_SHARED writes to other bytes cannot be discarded.
	n, err := rw.fd.writeFromBlocksAt(rw.ctx, srcs, rw.off)
	end := rw.off + n
	for seg := i.cache.LowerBoundSegment(rw.off); seg.Ok() && seg.Start() < end; seg, _ = seg.NextNonEmpty() {
		r := seg.Range().Intersect(memmap.MappableRange{Start: rw.off, End: end})
		blocks, mapErr := i.fs.mf.MapInternal(seg.FileRangeOf(r), hostarch.Write)
		if mapErr != nil {
			return 0, mapErr
		}
		if _, copyErr := safemem.CopySeq(blocks, srcs.DropFirst64(r.Start-rw.off).TakeFirst64(r.Length())); copyErr != nil {
			return 0, copyErr
		}
	}
	rw.off = end
	if end > i.size.Load() {
		i.size.Store(end)
	}
	return n, err
}

// retainWritebackLocked transfers ownership of an existing FUSE handle. VFS
// mappings keep the inode alive; its cache owns this handle after FD release.
// Preconditions: i.dataMu is locked.
func (i *inode) retainWritebackLocked(fd *regularFileFD) {
	if i.writebackFD == nil {
		i.writebackFD = fd
		fd.handleTransferred = true
	}
}

// syncMapped does not acquire attrMu: faults and eviction must never wait for
// a lock held by an operation that invalidates their MM translations.
func (i *inode) syncMapped(ctx context.Context, _ *regularFileFD) error {
	i.dataMu.Lock()
	defer i.dataMu.Unlock()
	return i.syncMappedLocked(ctx)
}

func (i *inode) syncMappedLocked(ctx context.Context) error {
	if i.dirty.IsEmpty() {
		return nil
	}
	if i.writebackFD == nil {
		return linuxerr.EIO
	}
	return fsutil.SyncDirtyAll(ctx, &i.cache, &i.dirty, i.size.Load(), i.fs.mf, i.writebackFD.writeFromBlocksAt)
}

// updateSizeMMap requires attrMu. dataMu protects size against Translate and
// writeback; it is released before entering any MappingSpace.
func (i *inode) updateSizeMMap(ctx context.Context, size uint64) {
	i.dataMu.Lock()
	i.updateSizeAndUnlockData(ctx, size)
}

func (i *inode) updateSizeAndUnlockData(ctx context.Context, size uint64) {
	old := i.size.Load()
	i.size.Store(size)
	i.dataMu.Unlock()
	if size >= old {
		return
	}
	oldEnd, _ := hostarch.PageRoundUp(old)
	newEnd, _ := hostarch.PageRoundUp(size)
	if newEnd != oldEnd {
		i.mapsMu.Lock()
		i.mappings.Invalidate(memmap.MappableRange{Start: newEnd, End: oldEnd}, memmap.InvalidateOpts{InvalidatePrivate: true})
		i.mapsMu.Unlock()
	}
	i.dataMu.Lock()
	i.cache.Truncate(size, i.fs.mf)
	i.dirty.KeepClean(memmap.MappableRange{Start: size, End: oldEnd})
	i.dataMu.Unlock()
}

// Evict writes and drops only pages without a mapping. Failed writes retain
// their dirty data and handle for a later sync or eviction attempt.
func (i *inode) Evict(ctx context.Context, er pgalloc.EvictableRange) {
	i.mapsMu.Lock()
	i.dataMu.Lock()
	mr := memmap.MappableRange{Start: er.Start, End: er.End}
	for gap := i.mappings.LowerBoundGap(mr.Start); gap.Ok() && gap.Start() < mr.End; gap = gap.NextGap() {
		r := gap.Range().Intersect(mr)
		if r.Length() == 0 {
			continue
		}
		if i.writebackFD != nil {
			if err := fsutil.SyncDirty(ctx, r, &i.cache, &i.dirty, i.size.Load(), i.fs.mf, i.writebackFD.writeFromBlocksAt); err != nil {
				log.Warningf("FUSE cache writeback failed: %v", err)
				continue
			}
		}
		i.cache.Drop(r, i.fs.mf)
		i.dirty.KeepClean(r)
	}
	fd := i.detachCleanHandleLocked()
	i.dataMu.Unlock()
	i.mapsMu.Unlock()
	if fd != nil {
		fd.fileDescription.Release(ctx)
	}
}

// detachCleanHandleLocked returns a handle that must be released outside
// dataMu. A still-open descriptor resumes normal ownership of its handle.
func (i *inode) detachCleanHandleLocked() *regularFileFD {
	if !i.dirty.IsEmpty() || i.writebackFD == nil {
		return nil
	}
	fd := i.writebackFD
	i.writebackFD = nil
	fd.handleTransferred = false
	if fd.released {
		return fd
	}
	return nil
}

func (fd *regularFileFD) Release(ctx context.Context) {
	i := fd.inode()
	i.dataMu.Lock()
	fd.released = true
	transferred := fd.handleTransferred
	i.dataMu.Unlock()
	if !transferred {
		fd.fileDescription.Release(ctx)
	}
}

func (i *inode) destroyCache(ctx context.Context) {
	i.fs.mf.MarkAllUnevictable(i)
	i.dataMu.Lock()
	if err := i.syncMappedLocked(ctx); err != nil {
		log.Warningf("FUSE final cache writeback failed: %v", err)
	}
	fd := i.writebackFD
	i.writebackFD = nil
	i.cache.DropAll(i.fs.mf)
	i.dirty.RemoveAllAndAccount()
	i.dataMu.Unlock()
	if fd != nil {
		fd.fileDescription.Release(ctx)
	}
}

// OnClose flushes mapped writes made before close, including when a mapping
// will keep the FUSE handle alive after the descriptor leaves the FD table.
func (fd *regularFileFD) OnClose(ctx context.Context) error {
	if err := fd.inode().syncMapped(ctx, fd); err != nil {
		return err
	}
	return fd.fileDescription.OnClose(ctx)
}

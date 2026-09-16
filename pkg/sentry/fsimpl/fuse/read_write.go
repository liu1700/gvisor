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
	"io"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/safemem"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
)

func (fd *regularFileFD) readToBlocksAt(ctx context.Context, dsts safemem.BlockSeq, off uint64) (uint64, error) {
	if dsts.IsEmpty() {
		return 0, nil
	}
	left := dsts
	var done uint64
	for !left.IsEmpty() {
		n := left.NumBytes()
		max := uint64(fd.inode().fs.conn.maxRead)
		if pages := uint64(fd.inode().fs.conn.maxPages) << hostarch.PageShift; pages < max {
			max = pages
		}
		if max == 0 {
			return done, linuxerr.EIO
		}
		if n > max {
			n = max
		}
		in := linux.FUSEReadIn{Fh: fd.Fh, Offset: off + done, Size: uint32(n), Flags: fd.statusFlags()}
		res, err := fd.inode().callRaw(ctx, linux.FUSE_READ, &in)
		if err != nil {
			return done, err
		}
		if err := res.Error(); err != nil {
			return done, err
		}
		if len(res.data) < res.hdr.SizeBytes() {
			return done, linuxerr.EIO
		}
		payload := res.data[res.hdr.SizeBytes():]
		if uint64(len(payload)) > n {
			return done, linuxerr.EIO
		}
		copied, err := safemem.CopySeq(left.TakeFirst64(uint64(len(payload))), safemem.BlockSeqOf(safemem.BlockFromSafeSlice(payload)))
		done += copied
		left = left.DropFirst64(copied)
		if err != nil {
			return done, err
		}
		if uint64(len(payload)) != n {
			return done, io.EOF
		}
	}
	return done, nil
}

func (fd *regularFileFD) writeFromBlocksAt(ctx context.Context, srcs safemem.BlockSeq, off uint64) (uint64, error) {
	return fd.inode().writeHandle(ctx, srcs, off, fd.Fh, fd.statusFlags(), fd.vfsfd.Credentials(), false)
}

func (i *inode) writeHandle(ctx context.Context, srcs safemem.BlockSeq, off, fh uint64, flags uint32, creds *auth.Credentials, cached bool) (uint64, error) {
	if srcs.IsEmpty() {
		return 0, nil
	}
	var done uint64
	for !srcs.IsEmpty() {
		n := srcs.NumBytes()
		max := uint64(i.fs.conn.maxWrite)
		if pages := uint64(i.fs.conn.maxPages) << hostarch.PageShift; pages < max {
			max = pages
		}
		if !i.fs.conn.bigWrites && max > hostarch.PageSize {
			max = hostarch.PageSize
		}
		if max == 0 {
			return done, linuxerr.EIO
		}
		if n > max {
			n = max
		}
		buf := make([]byte, n)
		copied, err := safemem.CopySeq(safemem.BlockSeqOf(safemem.BlockFromSafeSlice(buf)), srcs.TakeFirst64(n))
		if err != nil {
			return done, err
		}
		buf = buf[:copied]
		in := linux.FUSEWritePayloadIn{Header: linux.FUSEWriteIn{Fh: fh, Offset: off + done, Size: uint32(copied), Flags: flags}, Payload: buf}
		if cached {
			in.Header.WriteFlags = 1
		} // FUSE_WRITE_CACHE: writeback from a mapped page.
		req := i.fs.conn.NewRequest(creds, pidFromContext(ctx), i.nodeID, linux.FUSE_WRITE, &in)
		res, err := i.fs.conn.Call(ctx, req)
		if err != nil {
			return done, err
		}
		if err := res.Error(); err != nil {
			return done, err
		}
		var out linux.FUSEWriteOut
		if err := res.UnmarshalPayload(&out); err != nil {
			return done, err
		}
		if out.Size > uint32(copied) {
			return done, linuxerr.EIO
		}
		done += uint64(out.Size)
		srcs = srcs.DropFirst64(uint64(out.Size))
		if out.Size == 0 {
			return done, linuxerr.EIO
		}
		if uint64(out.Size) != copied {
			return done, nil
		}
	}
	return done, nil
}

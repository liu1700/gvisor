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

// Tests for the POSIX conformance gaps found by pjdfstest and fsx on an
// in-sandbox FUSE mount (PLO-954). Each case names the assertion it comes from.

package fuse

import (
	"testing"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/errors"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
)

// TestCheckLargeFileOpen covers pjdfstest open/25.t, "interact with > 2 GB
// files". The suite writes a byte at offset 2147483649, reopens the file
// O_RDONLY and reads that byte back. Before the fix the open failed with
// EOVERFLOW because the flag mask in inode.Open dropped O_LARGEFILE before the
// check read it, so the check saw every open as a non-LFS one.
func TestCheckLargeFileOpen(t *testing.T) {
	// The size open/25.t leaves behind: 2 GiB + 2 bytes.
	const bigFileSize = 2*1024*1024*1024 + 2

	for _, test := range []struct {
		name  string
		flags uint32
		size  uint64
		want  *errors.Error
	}{
		{
			name:  "large file with O_LARGEFILE",
			flags: linux.O_RDONLY | linux.O_LARGEFILE,
			size:  bigFileSize,
			want:  nil,
		},
		{
			name:  "large file without O_LARGEFILE",
			flags: linux.O_RDONLY,
			size:  bigFileSize,
			want:  linuxerr.EOVERFLOW,
		},
		{
			name:  "file at the limit without O_LARGEFILE",
			flags: linux.O_RDONLY,
			size:  linux.MAX_NON_LFS,
			want:  nil,
		},
		{
			name:  "one byte over the limit without O_LARGEFILE",
			flags: linux.O_RDONLY,
			size:  linux.MAX_NON_LFS + 1,
			want:  linuxerr.EOVERFLOW,
		},
		{
			name:  "small file",
			flags: linux.O_RDWR,
			size:  4096,
			want:  nil,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := checkLargeFileOpen(test.flags, test.size)
			if test.want == nil {
				if got != nil {
					t.Fatalf("checkLargeFileOpen(%#o, %d) = %v, want nil", test.flags, test.size, got)
				}
				return
			}
			if !linuxerr.Equals(test.want, got) {
				t.Fatalf("checkLargeFileOpen(%#o, %d) = %v, want %v", test.flags, test.size, got, test.want)
			}
		})
	}
}

// TestReadRequestEncodesOffsetAboveTwoGiB checks that the read request the
// client puts on the wire carries the full 64-bit offset open/25.t reads at, so
// that once the open is allowed the read itself is served correctly.
func TestReadRequestEncodesOffsetAboveTwoGiB(t *testing.T) {
	const offset = 2147483649

	in := linux.FUSEReadIn{Fh: 7, Offset: offset, Size: 1}
	buf := make([]byte, in.SizeBytes())
	in.MarshalBytes(buf)

	var out linux.FUSEReadIn
	out.UnmarshalBytes(buf)
	if out.Offset != offset {
		t.Fatalf("round-tripped FUSEReadIn.Offset = %d, want %d", out.Offset, offset)
	}
	if out.Size != 1 {
		t.Fatalf("round-tripped FUSEReadIn.Size = %d, want 1", out.Size)
	}

	// The open that precedes the read must be allowed: open/25.t reopens a
	// file of 2 GiB + 2 bytes, and the sentry sets O_LARGEFILE on every open.
	if err := checkLargeFileOpen(linux.O_RDONLY|linux.O_LARGEFILE, offset+1); err != nil {
		t.Fatalf("checkLargeFileOpen for the open/25.t reopen: %v", err)
	}
}

// TestOpenSpecialFileType covers pjdfstest open/06.t, open/17.t and open/24.t.
// Opening a FIFO or a socket on a FUSE mount previously fell through to the
// "unknown file type" branch and returned EINVAL. Linux opens a FIFO with the
// kernel's own pipe implementation and fails an open of a socket with ENXIO.
func TestOpenSpecialFileType(t *testing.T) {
	for _, test := range []struct {
		name        string
		fileType    linux.FileMode
		wantHandled bool
		wantErr     *errors.Error
	}{
		{name: "fifo", fileType: linux.S_IFIFO, wantHandled: true, wantErr: nil},
		{name: "socket", fileType: linux.S_IFSOCK, wantHandled: true, wantErr: linuxerr.ENXIO},
		{name: "regular", fileType: linux.S_IFREG, wantHandled: false, wantErr: nil},
		{name: "directory", fileType: linux.S_IFDIR, wantHandled: false, wantErr: nil},
		{name: "symlink", fileType: linux.S_IFLNK, wantHandled: false, wantErr: nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			handled, err := openSpecialFileType(test.fileType)
			if handled != test.wantHandled {
				t.Fatalf("openSpecialFileType(%#o) handled = %v, want %v", test.fileType, handled, test.wantHandled)
			}
			if test.wantErr == nil {
				if err != nil {
					t.Fatalf("openSpecialFileType(%#o) err = %v, want nil", test.fileType, err)
				}
				return
			}
			if !linuxerr.Equals(test.wantErr, err) {
				t.Fatalf("openSpecialFileType(%#o) err = %v, want %v", test.fileType, err, test.wantErr)
			}
		})
	}
}

// TestNamedPipeIsPerInode checks that every opener of one FIFO inode gets the
// same pipe, as they do on Linux, where the pipe hangs off the inode.
func TestNamedPipeIsPerInode(t *testing.T) {
	var i inode
	first := i.namedPipe()
	if first == nil {
		t.Fatal("namedPipe returned nil")
	}
	if second := i.namedPipe(); second != first {
		t.Fatalf("namedPipe returned a second pipe %p, want %p", second, first)
	}

	var other inode
	if otherPipe := other.namedPipe(); otherPipe == first {
		t.Fatal("two inodes share one pipe")
	}
}

// TestStatFromFUSEAttrReportsMask covers the fsx copy_file_range failure.
// copy_file_range(2) reads STATX_TYPE out of the returned mask to check that
// both ends are regular files, and rejects the call with EINVAL when the mask
// does not carry it. The FUSE client used to leave Mask at zero and to fill
// Mode only when the caller asked for STATX_MODE, so every copy_file_range on
// the mount failed after fsx's probe had already concluded it was supported.
func TestStatFromFUSEAttrReportsMask(t *testing.T) {
	attr := linux.FUSEAttr{
		Ino:       42,
		Size:      4096,
		Blocks:    8,
		Atime:     1,
		Mtime:     2,
		Ctime:     3,
		AtimeNsec: 10,
		MtimeNsec: 20,
		CtimeNsec: 30,
		Mode:      linux.S_IFREG | 0644,
		Nlink:     1,
		UID:       1000,
		GID:       1000,
		BlkSize:   4096,
	}
	stat := statFromFUSEAttr(attr, 7)

	wantMask := uint32(linux.STATX_TYPE | linux.STATX_MODE | linux.STATX_NLINK |
		linux.STATX_UID | linux.STATX_GID | linux.STATX_ATIME |
		linux.STATX_MTIME | linux.STATX_CTIME | linux.STATX_INO |
		linux.STATX_SIZE | linux.STATX_BLOCKS)
	if stat.Mask != wantMask {
		t.Errorf("Mask = %#x, want %#x", stat.Mask, wantMask)
	}
	if stat.Mask&linux.STATX_TYPE == 0 {
		t.Error("STATX_TYPE missing from Mask; copy_file_range would fail with EINVAL")
	}
	if got, want := uint32(stat.Mode)&linux.S_IFMT, uint32(linux.S_IFREG); got != want {
		t.Errorf("Mode file type = %#o, want %#o", got, want)
	}
	if stat.Ino != attr.Ino {
		t.Errorf("Ino = %d, want %d", stat.Ino, attr.Ino)
	}
	if stat.Size != attr.Size {
		t.Errorf("Size = %d, want %d", stat.Size, attr.Size)
	}
	if stat.Nlink != attr.Nlink {
		t.Errorf("Nlink = %d, want %d", stat.Nlink, attr.Nlink)
	}
	if stat.Mtime.Sec != int64(attr.Mtime) || stat.Mtime.Nsec != attr.MtimeNsec {
		t.Errorf("Mtime = %+v, want %d.%d", stat.Mtime, attr.Mtime, attr.MtimeNsec)
	}
}

// TestClearSUIDAndSGIDOnWrite covers pjdfstest chmod/12.t, "verify SUID/SGID
// bit behaviour". A write by a user who does not own the file clears the setuid
// bit, and clears the setgid bit when the file is group-executable. These are
// the three modes the suite creates and the mode it expects to read back after
// the write; the FUSE write path now applies this and sends the new mode to the
// server.
func TestClearSUIDAndSGIDOnWrite(t *testing.T) {
	for _, test := range []struct {
		name string
		old  uint32
		want uint32
	}{
		{name: "setuid", old: 04777, want: 0777},
		{name: "setgid group-executable", old: 02777, want: 0777},
		{name: "setuid and setgid", old: 06777, want: 0777},
		{name: "setgid not group-executable", old: 02767, want: 02767},
		{name: "no special bits", old: 0644, want: 0644},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := vfs.ClearSUIDAndSGID(test.old); got != test.want {
				t.Fatalf("ClearSUIDAndSGID(%#o) = %#o, want %#o", test.old, got, test.want)
			}
		})
	}
}

// TestTruncateDirectoryIsEISDIR covers pjdfstest truncate/09.t and
// ftruncate/09.t, both of which run `truncate <dir> 123`. Linux rejects this in
// do_sys_truncate() before the filesystem sees it; the request used to reach
// the server, which answered EPERM.
func TestTruncateDirectoryIsEISDIR(t *testing.T) {
	for _, test := range []struct {
		name string
		mode linux.FileMode
		mask uint32
		want *errors.Error
	}{
		{name: "size on a directory", mode: linux.S_IFDIR | 0755, mask: linux.STATX_SIZE, want: linuxerr.EISDIR},
		{name: "size on a regular file", mode: linux.S_IFREG | 0644, mask: linux.STATX_SIZE, want: nil},
		{name: "mode on a directory", mode: linux.S_IFDIR | 0755, mask: linux.STATX_MODE, want: nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := checkSetStatType(test.mask, test.mode)
			if test.want == nil {
				if got != nil {
					t.Fatalf("got %v, want nil", got)
				}
				return
			}
			if !linuxerr.Equals(test.want, got) {
				t.Fatalf("got %v, want %v", got, test.want)
			}
		})
	}
}

/*
 * JuiceFS, Copyright 2018 Juicedata, Inc.
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

package sync

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"os"
	"path"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode"

	"github.com/juicedata/juicefs/pkg/object"
	"github.com/juicedata/juicefs/pkg/utils"
	"github.com/juju/ratelimit"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/vimeo/go-util/crc32combine"
)

// The max number of key per listing request
const (
	maxResults      = 1000
	defaultPartSize = 5 << 20
	bufferSize      = 32 << 10
	maxBlock        = defaultPartSize * 2
	markDeleteSrc   = -1
	markDeleteDst   = -2
	markCopyPerms   = -3
	markChecksum    = -4
)

var (
	handled                 *utils.Bar
	pending                 *utils.Bar
	copied, copiedBytes     *utils.Bar
	checked, checkedBytes   *utils.Bar
	skipped, skippedBytes   *utils.Bar
	excluded, excludedBytes *utils.Bar
	extra, extraBytes       *utils.Bar
	deleted, failed         *utils.Bar
	listedPrefix            *utils.Bar
	concurrent              chan int
	limiter                 *ratelimit.Bucket
	totalHandled            atomic.Int64
)
var crcTable = crc32.MakeTable(crc32.Castagnoli)
var logger = utils.GetLogger("juicefs")

func incrTotal(n int64) {
	totalHandled.Add(n)
}

func incrHandled(n int) {
	old := totalHandled.Swap(0)
	handled.IncrTotal(old)
	handled.IncrBy(n)
}

type chksumReader struct {
	io.Reader
	chksum uint32
	cal    bool
}

func (r *chksumReader) Read(p []byte) (n int, err error) {
	n, err = r.Reader.Read(p)
	if r.cal {
		r.chksum = crc32.Update(r.chksum, crcTable, p[:n])
	}
	return
}

type chksumWithSz struct {
	chksum uint32
	size   int64
}

// human readable bytes size
func formatSize(bytes int64) string {
	units := [7]string{" ", "K", "M", "G", "T", "P", "E"}
	if bytes < 1024 {
		return fmt.Sprintf("%v B", bytes)
	}
	z := 0
	v := float64(bytes)
	for v > 1024.0 {
		z++
		v /= 1024.0
	}
	return fmt.Sprintf("%.2f %siB", v, units[z])
}

// ListAll on all the keys that starts at marker from object storage.
func ListAll(store object.ObjectStorage, prefix, start, end string, followLink bool) (<-chan object.Object, error) {
	startTime := time.Now()
	logger.Debugf("Iterating objects from %s with prefix %s start %q", store, prefix, start)

	out := make(chan object.Object, maxResults*10)

	// As the result of object storage's List method doesn't include the marker key,
	// we try List the marker key separately.
	if start != "" && strings.HasPrefix(start, prefix) {
		if obj, err := store.Head(start); err == nil {
			logger.Debugf("Found start key: %s from %s in %s", start, store, time.Since(startTime))
			out <- obj
		}
	}

	if ch, err := store.ListAll(prefix, start, followLink); err == nil {
		go func() {
			for obj := range ch {
				if obj != nil && end != "" && obj.Key() > end {
					break
				}
				out <- obj
			}
			close(out)
		}()
		return out, nil
	} else if !errors.Is(err, utils.ENOTSUP) {
		return nil, err
	}

	marker := start
	logger.Debugf("Listing objects from %s marker %q", store, marker)

	objs, hasMore, nextToken, err := store.List(prefix, marker, "", "", maxResults, followLink)
	if errors.Is(err, utils.ENOTSUP) {
		return object.ListAllWithDelimiter(store, prefix, start, end, followLink)
	}
	if err != nil {
		logger.Errorf("Can't list %s: %s", store, err.Error())
		return nil, err
	}
	logger.Debugf("Found %d object from %s in %s", len(objs), store, time.Since(startTime))
	go func() {
		lastkey := ""
		first := true
	END:
		for {
			for _, obj := range objs {
				key := obj.Key()
				if !first && key <= lastkey {
					logger.Errorf("The keys are out of order: marker %q, last %q current %q", marker, lastkey, key)
					out <- nil
					break END
				}
				if end != "" && key > end {
					break END
				}
				lastkey = key
				// logger.Debugf("key: %s", key)
				out <- obj
				first = false
			}
			if !hasMore {
				break
			}

			marker = lastkey
			startTime = time.Now()
			logger.Debugf("Continue listing objects from %s marker %q", store, marker)
			var nextToken2 string
			objs, hasMore, nextToken2, err = store.List(prefix, marker, nextToken, "", maxResults, followLink)
			count := 0
			for err != nil && count < 3 {
				logger.Warnf("Fail to list: %s, retry again", err.Error())
				// slow down
				time.Sleep(time.Millisecond * 100)

				objs, hasMore, nextToken2, err = store.List(prefix, marker, nextToken, "", maxResults, followLink)
				count++
			}
			logger.Debugf("Found %d object from %s in %s", len(objs), store, time.Since(startTime))
			if err != nil {
				// Telling that the listing has failed
				out <- nil
				logger.Errorf("Fail to list after %s: %s", marker, err.Error())
				break
			}
			nextToken = nextToken2
			if len(objs) > 0 && objs[0].Key() == marker {
				// workaround from a object store that is not compatible to S3.
				objs = objs[1:]
			}
		}
		close(out)
	}()
	return out, nil
}

var bufPool = sync.Pool{
	New: func() interface{} {
		buf := make([]byte, bufferSize)
		return &buf
	},
}

func shouldRetry(err error) bool {
	if err == nil || errors.Is(err, utils.ErrSkipped) || errors.Is(err, utils.ErrExtlink) {
		return false
	}

	var eno syscall.Errno
	if errors.As(err, &eno) {
		switch eno {
		case syscall.EAGAIN, syscall.EINTR, syscall.EBUSY, syscall.ETIMEDOUT, syscall.EIO:
			return true
		default:
			return false
		}
	}
	return true
}

func try(n int, f func() error) (err error) {
	for i := 0; i < n; i++ {
		err = f()
		if !shouldRetry(err) {
			return
		}
		logger.Debugf("Try %d failed: %s", i+1, err)
		time.Sleep(time.Second * time.Duration(i*i))
	}
	return
}

func deleteObj(storage object.ObjectStorage, key string, dry bool) {
	if dry {
		logger.Debugf("Will delete %s from %s", key, storage)
		deleted.Increment()
		return
	}
	start := time.Now()
	if err := try(3, func() error { return storage.Delete(key) }); err == nil {
		deleted.Increment()
		logger.Debugf("Deleted %s from %s in %s", key, storage, time.Since(start))
	} else {
		failed.Increment()
		logger.Errorf("Failed to delete %s from %s in %s: %s", key, storage, time.Since(start), err)
	}
}

func needCopyPerms(o1, o2 object.Object) bool {
	f1 := o1.(object.File)
	f2 := o2.(object.File)
	return f2.Mode() != f1.Mode() || f2.Owner() != f1.Owner() || f2.Group() != f1.Group()
}

func copyPerms(dst object.ObjectStorage, obj object.Object, config *Config) {
	start := time.Now()
	key := obj.Key()
	fi := obj.(object.File)
	if !fi.IsSymlink() || !config.Links {
		// chmod needs to be executed after chown, because chown will change setuid setgid to be invalid.
		if err := dst.(object.FileSystem).Chown(key, fi.Owner(), fi.Group()); err != nil {
			logger.Warnf("Chown %s to (%s,%s): %s", key, fi.Owner(), fi.Group(), err)
		}
		if err := dst.(object.FileSystem).Chmod(key, fi.Mode()); err != nil {
			logger.Warnf("Chmod %s to %o: %s", key, fi.Mode(), err)
		}
	}
	logger.Debugf("Copied permissions (%s:%s:%s) for %s in %s", fi.Owner(), fi.Group(), fi.Mode(), key, time.Since(start))
}

func calPartChksum(objStor object.ObjectStorage, key string, abort chan struct{}, offset, length int64) (uint32, error) {
	if limiter != nil {
		limiter.Wait(length)
	}
	select {
	case <-abort:
		return 0, fmt.Errorf("aborted")
	case concurrent <- 1:
		defer func() {
			<-concurrent
		}()
	}
	in, err := objStor.Get(key, offset, length)
	if err != nil {
		return 0, fmt.Errorf("dest get: %s", err)
	}
	defer in.Close()

	buf := bufPool.Get().(*[]byte)
	defer bufPool.Put(buf)
	var chksum uint32
	for left := int(length); left > 0; left -= bufferSize {
		bs := bufferSize
		if left < bufferSize {
			bs = left
		}
		*buf = (*buf)[:bs]
		if _, err = io.ReadFull(in, *buf); err != nil {
			return 0, fmt.Errorf("dest read: %s", err)
		}
		chksum = crc32.Update(chksum, crcTable, *buf)
	}
	return chksum, nil
}

func calObjChksum(objStor object.ObjectStorage, key string, abort chan struct{}, obj object.Object) (uint32, error) {
	var err error
	var chksum uint32
	if obj.Size() < maxBlock {
		return calPartChksum(objStor, key, abort, 0, obj.Size())
	}
	n := int((obj.Size()-1)/defaultPartSize) + 1
	errs := make(chan error, n)
	chksums := make([]chksumWithSz, n)
	for i := 0; i < n; i++ {
		go func(num int) {
			sz := int64(defaultPartSize)
			if num == n-1 {
				sz = obj.Size() - int64(num)*defaultPartSize
			}
			chksum, err := calPartChksum(objStor, key, abort, int64(num)*defaultPartSize, sz)
			chksums[num] = chksumWithSz{chksum, sz}
			errs <- err
		}(i)
	}
	for i := 0; i < n; i++ {
		if err = <-errs; err != nil {
			close(abort)
			break
		}
	}
	if err != nil {
		return 0, err
	}
	chksum = chksums[0].chksum
	for i := 1; i < n; i++ {
		chksum = crc32combine.CRC32Combine(crc32.Castagnoli, chksum, chksums[i].chksum, chksums[i].size)
	}
	return chksum, nil
}

func compObjPartBinary(src, dst object.ObjectStorage, key string, abort chan struct{}, offset, length int64) error {
	if limiter != nil {
		limiter.Wait(length)
	}
	select {
	case <-abort:
		return fmt.Errorf("aborted")
	case concurrent <- 1:
		defer func() {
			<-concurrent
		}()
	}
	in, err := src.Get(key, offset, length)
	if err != nil {
		return fmt.Errorf("src get: %s", err)
	}
	defer in.Close()
	in2, err := dst.Get(key, offset, length)
	if err != nil {
		return fmt.Errorf("dest get: %s", err)
	}
	defer in2.Close()

	buf := bufPool.Get().(*[]byte)
	defer bufPool.Put(buf)
	buf2 := bufPool.Get().(*[]byte)
	defer bufPool.Put(buf2)
	for left := int(length); left > 0; left -= bufferSize {
		bs := bufferSize
		if left < bufferSize {
			bs = left
		}
		*buf = (*buf)[:bs]
		*buf2 = (*buf2)[:bs]
		if _, err = io.ReadFull(in, *buf); err != nil {
			return fmt.Errorf("src read: %s", err)
		}
		if _, err = io.ReadFull(in2, *buf2); err != nil {
			return fmt.Errorf("dest read: %s", err)
		}
		if !bytes.Equal(*buf, *buf2) {
			return fmt.Errorf("bytes not equal")
		}
	}
	return nil
}

func compObjBinary(src, dst object.ObjectStorage, key string, abort chan struct{}, obj object.Object) (bool, error) {
	var err error
	if obj.Size() < maxBlock {
		err = compObjPartBinary(src, dst, key, abort, 0, obj.Size())
	} else {
		n := int((obj.Size()-1)/defaultPartSize) + 1
		errs := make(chan error, n)
		for i := 0; i < n; i++ {
			go func(num int) {
				sz := int64(defaultPartSize)
				if num == n-1 {
					sz = obj.Size() - int64(num)*defaultPartSize
				}
				errs <- compObjPartBinary(src, dst, key, abort, int64(num)*defaultPartSize, sz)
			}(i)
		}
		for i := 0; i < n; i++ {
			if err = <-errs; err != nil {
				close(abort)
				break
			}
		}
	}
	equal := false
	if err != nil && err.Error() == "bytes not equal" {
		err = nil
	} else {
		equal = err == nil
	}
	return equal, err
}

func doCheckSum(src, dst object.ObjectStorage, key string, srcChksumPtr *uint32, obj object.Object, config *Config, equal *bool) error {
	if obj.IsSymlink() && config.Links && (config.CheckAll || config.CheckNew) {
		var srcLink, dstLink string
		var err error
		if s, ok := src.(object.SupportSymlink); ok {
			if srcLink, err = s.Readlink(key); err != nil {
				return err
			}
		}
		if s, ok := dst.(object.SupportSymlink); ok {
			if dstLink, err = s.Readlink(key); err != nil {
				return err
			}
		}
		*equal = srcLink == dstLink && srcLink != "" && dstLink != ""
		return nil
	}
	abort := make(chan struct{})
	var err error
	if srcChksumPtr != nil {
		var srcChksum uint32
		var dstChksum uint32
		srcChksum = *srcChksumPtr
		dstChksum, err = calObjChksum(dst, key, abort, obj)
		if err == nil {
			*equal = srcChksum == dstChksum
		} else {
			*equal = false
		}
	} else {
		*equal, err = compObjBinary(src, dst, key, abort, obj)
	}
	return err
}

func checkSum(src, dst object.ObjectStorage, key string, srcChksum *uint32, obj object.Object, config *Config) (bool, error) {
	start := time.Now()
	var equal bool
	err := try(3, func() error { return doCheckSum(src, dst, key, srcChksum, obj, config, &equal) })
	if err == nil {
		checked.Increment()
		checkedBytes.IncrInt64(obj.Size())
		if equal {
			logger.Debugf("Checked %s OK (and equal) in %s,", key, time.Since(start))
		} else {
			logger.Warnf("Checked %s OK (but NOT equal) in %s,", key, time.Since(start))
		}
	} else {
		logger.Errorf("Failed to check %s in %s: %s", key, time.Since(start), err)
	}
	return equal, err
}

var fastStreamRead = map[string]struct{}{"file": {}, "hdfs": {}, "jfs": {}, "gluster": {}}
var streamWrite = map[string]struct{}{"file": {}, "hdfs": {}, "sftp": {}, "gs": {}, "wasb": {}, "ceph": {}, "swift": {}, "webdav": {}, "jfs": {}, "gluster": {}}
var readInMem = map[string]struct{}{"mem": {}, "etcd": {}, "redis": {}, "tikv": {}, "mysql": {}, "postgres": {}, "sqlite3": {}}

func inMap(obj object.ObjectStorage, m map[string]struct{}) bool {
	_, ok := m[strings.Split(obj.String(), "://")[0]]
	return ok
}

func doCopySingle(src, dst object.ObjectStorage, key string, size int64, calChksum bool) (uint32, error) {
	if size > maxBlock && !inMap(dst, readInMem) && !inMap(src, fastStreamRead) {
		var err error
		var in io.Reader
		downer := newParallelDownloader(src, key, size, downloadBufSize, concurrent)
		defer downer.Close()
		if inMap(dst, streamWrite) {
			in = downer
		} else {
			var f *os.File
			// download the object into disk
			if f, err = os.CreateTemp("", "rep"); err != nil {
				logger.Warnf("create temp file: %s", err)
				return doCopySingle0(src, dst, key, size, calChksum)
			}
			_ = os.Remove(f.Name()) // will be deleted after Close()
			defer f.Close()
			buf := bufPool.Get().(*[]byte)
			defer bufPool.Put(buf)
			// hide f.ReadFrom to avoid discarding buf
			if _, err = io.CopyBuffer(struct{ io.Writer }{f}, downer, *buf); err == nil {
				_, err = f.Seek(0, 0)
				in = f
			}
		}
		r := &chksumReader{in, 0, calChksum}
		if err == nil {
			err = dst.Put(key, r)
		}
		if err != nil {
			if _, e := src.Head(key); os.IsNotExist(e) {
				logger.Debugf("Head src %s: %s", key, err)
				err = utils.ErrSkipped
			}
		}
		return r.chksum, err
	}
	return doCopySingle0(src, dst, key, size, calChksum)
}

func doCopySingle0(src, dst object.ObjectStorage, key string, size int64, calChksum bool) (uint32, error) {
	concurrent <- 1
	defer func() {
		<-concurrent
	}()
	var in io.ReadCloser
	var err error
	if size == 0 {
		if key == "" && !object.IsFileSystem(dst) {
			ps := strings.SplitN(dst.String(), "/", 4)
			if len(ps) == 4 && ps[3] == "" {
				logger.Warnf("empty key is not support by %s, ignore it", dst)
				return 0, nil
			}
		}
		if object.IsFileSystem(src) {
			// for check permissions
			r, err := src.Get(key, 0, -1)
			if err != nil {
				return 0, err
			}
			_ = r.Close()
		}
		in = io.NopCloser(bytes.NewReader(nil))
	} else {
		in, err = src.Get(key, 0, size)
		if err != nil {
			if _, e := src.Head(key); os.IsNotExist(e) {
				logger.Debugf("Head src %s: %s", key, err)
				err = utils.ErrSkipped
			}
			return 0, err
		}
	}
	r := &chksumReader{in, 0, calChksum}
	defer in.Close()
	err = dst.Put(key, &withProgress{r})
	return r.chksum, err
}

type withProgress struct {
	r io.Reader
}

func (w *withProgress) Read(b []byte) (int, error) {
	if limiter != nil {
		limiter.Wait(int64(len(b)))
	}
	n, err := w.r.Read(b)
	copiedBytes.IncrInt64(int64(n))
	return n, err
}

func dynAlloc(size int) []byte {
	zeros := utils.PowerOf2(size)
	b := *dynPools[zeros].Get().(*[]byte)
	if cap(b) < size {
		panic(fmt.Sprintf("%d < %d", cap(b), size))
	}
	return b[:size]
}

func dynFree(b []byte) {
	dynPools[utils.PowerOf2(cap(b))].Put(&b)
}

var dynPools []*sync.Pool

func init() {
	dynPools = make([]*sync.Pool, 33) // 1 - 8G
	for i := 0; i < 33; i++ {
		func(bits int) {
			dynPools[i] = &sync.Pool{
				New: func() interface{} {
					b := make([]byte, 1<<bits)
					return &b
				},
			}
		}(i)
	}
}

func doUploadPart(src, dst object.ObjectStorage, srckey string, off, size int64, key, uploadID string, num int, calChksum bool) (*object.Part, uint32, error) {
	if limiter != nil {
		limiter.Wait(size)
	}
	start := time.Now()
	sz := size
	data := dynAlloc(int(size))
	defer dynFree(data)
	var part *object.Part
	var chksum uint32
	err := try(3, func() error {
		in, err := src.Get(srckey, off, sz)
		if err != nil {
			return err
		}
		defer in.Close()
		r := &chksumReader{in, 0, calChksum}
		if _, err = io.ReadFull(r, data); err != nil {
			return err
		}
		chksum = r.chksum
		// PartNumber starts from 1
		part, err = dst.UploadPart(key, uploadID, num+1, data)
		return err
	})
	if err != nil {
		logger.Warnf("Failed to copy data of %s part %d: %s", key, num, err)
		return nil, 0, fmt.Errorf("part %d: %s", num, err)
	}
	logger.Debugf("Copied data of %s part %d in %s", key, num, time.Since(start))
	copiedBytes.IncrInt64(sz)
	return part, chksum, nil
}

func choosePartSize(upload *object.MultipartUpload, size int64) int64 {
	partSize := int64(upload.MinPartSize)
	if partSize == 0 {
		partSize = defaultPartSize
	}
	if size > partSize*int64(upload.MaxCount) {
		partSize = size / int64(upload.MaxCount)
		partSize = ((partSize-1)>>20 + 1) << 20 // align to MB
	}
	return partSize
}

func doCopyRange(src, dst object.ObjectStorage, key string, off, size int64, upload *object.MultipartUpload, num int, abort chan struct{}, calChksum bool) (*object.Part, uint32, error) {
	select {
	case <-abort:
		return nil, 0, fmt.Errorf("aborted")
	case concurrent <- 1:
		defer func() {
			<-concurrent
		}()
	}

	limits := dst.Limits()
	if size <= 32<<20 || !limits.IsSupportUploadPartCopy {
		return doUploadPart(src, dst, key, off, size, key, upload.UploadID, num, calChksum)
	}

	tmpkey := fmt.Sprintf("%s.part%d", key, num)
	var up *object.MultipartUpload
	var err error
	err = try(3, func() error {
		up, err = dst.CreateMultipartUpload(tmpkey)
		return err
	})
	if err != nil {
		return nil, 0, fmt.Errorf("range(%d,%d): %s", off, size, err)
	}

	partSize := choosePartSize(up, size)
	n := int((size-1)/partSize) + 1
	logger.Debugf("Copying data of %s (range: %d,%d) as %d parts (size: %d): %s", key, off, size, n, partSize, up.UploadID)
	parts := make([]*object.Part, n)
	var tmpChksum uint32
	first := true

	for i := 0; i < n; i++ {
		sz := partSize
		if i == n-1 {
			sz = size - int64(i)*partSize
		}
		select {
		case <-abort:
			dst.AbortUpload(tmpkey, up.UploadID)
			return nil, 0, fmt.Errorf("aborted")
		default:
		}
		var chksum uint32
		parts[i], chksum, err = doUploadPart(src, dst, key, off+int64(i)*partSize, sz, tmpkey, up.UploadID, i, calChksum)
		if err != nil {
			dst.AbortUpload(tmpkey, up.UploadID)
			return nil, 0, fmt.Errorf("range(%d,%d): %s", off, size, err)
		}
		if calChksum {
			if first {
				tmpChksum = chksum
				first = false
			} else {
				tmpChksum = crc32combine.CRC32Combine(crc32.Castagnoli, tmpChksum, chksum, sz)
			}
		}
	}

	err = try(3, func() error { return dst.CompleteUpload(tmpkey, up.UploadID, parts) })
	if err != nil {
		dst.AbortUpload(tmpkey, up.UploadID)
		return nil, 0, fmt.Errorf("multipart: %s", err)
	}
	var part *object.Part
	err = try(3, func() error {
		part, err = dst.UploadPartCopy(key, upload.UploadID, num+1, tmpkey, 0, size)
		return err
	})
	_ = dst.Delete(tmpkey)
	return part, tmpChksum, err
}

func doCopyMultiple(src, dst object.ObjectStorage, key string, size int64, upload *object.MultipartUpload, calChksum bool) (uint32, error) {
	limits := dst.Limits()
	if size > limits.MaxPartSize*int64(upload.MaxCount) {
		return 0, fmt.Errorf("object size %d is too large to copy", size)
	}

	partSize := choosePartSize(upload, size)
	n := int((size-1)/partSize) + 1
	logger.Debugf("Copying data of %s as %d parts (size: %d): %s", key, n, partSize, upload.UploadID)
	abort := make(chan struct{})
	parts := make([]*object.Part, n)
	errs := make(chan error, n)
	chksums := make([]chksumWithSz, n)
	var err error

	for i := 0; i < n; i++ {
		go func(num int) {
			sz := partSize
			if num == n-1 {
				sz = size - int64(num)*partSize
			}
			var copyErr error
			var chksum uint32
			parts[num], chksum, copyErr = doCopyRange(src, dst, key, int64(num)*partSize, sz, upload, num, abort, calChksum)
			chksums[num] = chksumWithSz{chksum, sz}
			errs <- copyErr
		}(i)
	}

	for i := 0; i < n; i++ {
		if err = <-errs; err != nil {
			close(abort)
			break
		}
	}
	if err == nil {
		err = try(3, func() error { return dst.CompleteUpload(key, upload.UploadID, parts) })
	}
	if err != nil {
		dst.AbortUpload(key, upload.UploadID)
		return 0, fmt.Errorf("multipart: %s", err)
	}
	var chksum uint32
	if calChksum {
		chksum = chksums[0].chksum
		for i := 1; i < n; i++ {
			chksum = crc32combine.CRC32Combine(crc32.Castagnoli, chksum, chksums[i].chksum, chksums[i].size)
		}
	}

	return chksum, nil
}

func InitForCopyData() {
	concurrent = make(chan int, 10)
	progress := utils.NewProgress(true)
	copied = progress.AddCountSpinner("Copied objects")
	copiedBytes = progress.AddByteSpinner("Copied bytes")
}

func CopyData(src, dst object.ObjectStorage, key string, size int64, calChksum bool) (uint32, error) {
	start := time.Now()
	var err error
	var srcChksum uint32
	if size < maxBlock {
		err = try(3, func() (err error) {
			srcChksum, err = doCopySingle(src, dst, key, size, calChksum)
			return
		})
	} else {
		var upload *object.MultipartUpload
		if upload, err = dst.CreateMultipartUpload(key); err == nil {
			srcChksum, err = doCopyMultiple(src, dst, key, size, upload, calChksum)
		} else if err == utils.ENOTSUP {
			err = try(3, func() (err error) {
				srcChksum, err = doCopySingle(src, dst, key, size, calChksum)
				return
			})
		} else { // other error retry
			if err = try(2, func() error {
				upload, err = dst.CreateMultipartUpload(key)
				return err
			}); err == nil {
				srcChksum, err = doCopyMultiple(src, dst, key, size, upload, calChksum)
			}
		}
	}
	if err == nil {
		logger.Debugf("Copied data of %s (%d bytes) in %s", key, size, time.Since(start))
	} else {
		logger.Errorf("Failed to copy data of %s in %s: %s", key, time.Since(start), err)
	}
	return srcChksum, err
}

func worker(tasks <-chan object.Object, src, dst object.ObjectStorage, config *Config) {
	for obj := range tasks {
		key := obj.Key()
		switch obj.Size() {
		case markDeleteSrc:
			deleteObj(src, key, config.Dry)
		case markDeleteDst:
			deleteObj(dst, key, config.Dry)
		case markCopyPerms:
			if config.Dry {
				logger.Debugf("Will copy permissions for %s", key)
			} else {
				copyPerms(dst, withoutSize(obj), config)
			}
			copied.Increment()
		case markChecksum:
			if config.Dry {
				logger.Debugf("Will compare checksum for %s", key)
				checked.Increment()
				break
			}
			obj = withoutSize(obj)
			if equal, err := checkSum(src, dst, key, nil, obj, config); err != nil {
				failed.Increment()
				break
			} else if equal {
				if config.DeleteSrc {
					if obj.IsDir() {
						srcDelayDelMu.Lock()
						srcDelayDel = append(srcDelayDel, key)
						srcDelayDelMu.Unlock()
					} else {
						deleteObj(src, key, false)
					}
				} else if config.Perms && (!obj.IsSymlink() || !config.Links) {
					if o, e := dst.Head(key); e == nil {
						if needCopyPerms(obj, o) {
							copyPerms(dst, obj, config)
							copied.Increment()
						} else {
							skipped.Increment()
							skippedBytes.IncrInt64(obj.Size())
						}
					} else {
						logger.Warnf("Failed to head object %s: %s", key, e)
						failed.Increment()
					}
				} else {
					skipped.Increment()
					skippedBytes.IncrInt64(obj.Size())
				}
				break
			}
			// checkSum not equal, copy the object
			fallthrough
		default:
			if config.Dry {
				logger.Debugf("Will copy %s (%d bytes)", obj.Key(), obj.Size())
				copied.Increment()
				copiedBytes.IncrInt64(obj.Size())
				break
			}
			var err error
			var srcChksum uint32

			if config.Links && obj.IsSymlink() {
				if err = copyLink(src, dst, key); err != nil {
					logger.Errorf("copy link failed: %s", err)
				}
			} else {
				srcChksum, err = CopyData(src, dst, key, obj.Size(), config.CheckAll || config.CheckNew)
			}
			if errors.Is(err, utils.ErrExtlink) {
				logger.Warnf("Skip external link %s: %s", key, err)
				err = utils.ErrSkipped
			}

			if err == nil && config.CheckChange {
				err = checkChange(src, dst, obj, key, config)
			}

			if err == nil && (config.CheckAll || config.CheckNew) {
				var equal bool
				if equal, err = checkSum(src, dst, key, &srcChksum, obj, config); err == nil && !equal {
					err = fmt.Errorf("checksums of copied object %s don't match", key)
				}
			}
			if err == nil {
				if mc, ok := dst.(object.MtimeChanger); ok {
					if err = mc.Chtimes(obj.Key(), obj.Mtime()); err != nil && !errors.Is(err, utils.ENOTSUP) {
						logger.Warnf("Update mtime of %s: %s", key, err)
					}
				}
				if config.Perms {
					copyPerms(dst, obj, config)
				}
				copied.Increment()
			} else if errors.Is(err, utils.ErrSkipped) {
				skipped.Increment()
			} else {
				failed.Increment()
				logger.Errorf("Failed to copy object %s: %s", key, err)
			}
		}
		incrHandled(1)
	}
}

func checkChange(src, dst object.ObjectStorage, obj object.Object, key string, config *Config) error {
	if obj == nil || config.Links && obj.IsSymlink() {
		return nil // ignore symlink
	}
	if cur, err := src.Head(key); err == nil {
		if !config.CheckAll && !config.CheckNew {
			checked.Increment()
			checkedBytes.IncrInt64(obj.Size())
		}
		equal := cur.Size() == obj.Size()
		if equal && !cur.Mtime().Equal(obj.Mtime()) {
			// Head of an object may not return the millisecond part of mtime as List
			equal = cur.Mtime().Unix() == obj.Mtime().Unix() && cur.Mtime().UnixMilli()%1000 == 0
		}
		if !equal {
			return fmt.Errorf("%s changed during sync. Original: size=%d, mtime=%s; Current: size=%d, mtime=%s",
				cur.Key(), obj.Size(), obj.Mtime(), cur.Size(), cur.Mtime())
		}
		if dstObj, err := dst.Head(key); err == nil {
			if cur.Size() != dstObj.Size() {
				return fmt.Errorf("copied %s size mismatch: original=%d, current=%d", key, obj.Size(), dstObj.Size())
			}
			return nil
		} else {
			return fmt.Errorf("check %s in %s: %s", key, dst, err)
		}
	} else if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("object %s was removed during sync", key)
	} else {
		return fmt.Errorf("check %s in %s: %s", key, src, err)
	}
}

func copyLink(src object.ObjectStorage, dst object.ObjectStorage, key string) error {
	if p, err := src.(object.SupportSymlink).Readlink(key); err != nil {
		return err
	} else {
		if err := dst.Delete(key); err != nil {
			logger.Debugf("Deleted %s from %s ", key, dst)
			return err
		}
		// TODO: use relative path based on option
		return dst.(object.SupportSymlink).Symlink(p, key)
	}
}

type objWithSize struct {
	object.Object
	nsize int64
}

func (o *objWithSize) Size() int64 {
	return o.nsize
}

type fileWithSize struct {
	object.File
	nsize int64
}

func (o *fileWithSize) Size() int64 {
	return o.nsize
}

func withSize(o object.Object, nsize int64) object.Object {
	if f, ok := o.(object.File); ok {
		return &fileWithSize{f, nsize}
	}
	return &objWithSize{o, nsize}
}

func withoutSize(o object.Object) object.Object {
	switch w := o.(type) {
	case *objWithSize:
		return w.Object
	case *fileWithSize:
		return w.File
	}
	return o
}

var dstDelayDelMu sync.Mutex
var dstDelayDel []string
var srcDelayDelMu sync.Mutex
var srcDelayDel []string

func handleExtraObject(tasks chan<- object.Object, dstobj object.Object, config *Config) bool {
	incrTotal(1)
	if !config.DeleteDst || !config.Dirs && dstobj.IsDir() || config.Limit == 0 {
		logger.Debug("Ignore extra object", dstobj.Key())
		extra.Increment()
		extraBytes.IncrInt64(dstobj.Size())
		return false
	}
	config.Limit--
	if dstobj.IsDir() {
		dstDelayDelMu.Lock()
		dstDelayDel = append(dstDelayDel, dstobj.Key())
		dstDelayDelMu.Unlock()
	} else {
		tasks <- withSize(dstobj, markDeleteDst)
	}
	return config.Limit == 0
}

func startSingleProducer(tasks chan<- object.Object, src, dst object.ObjectStorage, prefix string, config *Config) error {
	start, end := config.Start, config.End
	logger.Debugf("maxResults: %d, defaultPartSize: %d, maxBlock: %d", maxResults, defaultPartSize, maxBlock)

	srckeys, err := ListAll(src, prefix, start, end, !config.Links)
	if err != nil {
		return fmt.Errorf("list %s: %s", src, err)
	}

	var dstkeys <-chan object.Object
	if config.ForceUpdate {
		t := make(chan object.Object)
		close(t)
		dstkeys = t
	} else {
		dstkeys, err = ListAll(dst, prefix, start, end, !config.Links)
		if err != nil {
			return fmt.Errorf("list %s: %s", dst, err)
		}
	}
	return produce(tasks, srckeys, dstkeys, config)
}

func produce(tasks chan<- object.Object, srckeys, dstkeys <-chan object.Object, config *Config) error {
	srckeys = filter(srckeys, config.rules, config)
	dstkeys = filter(dstkeys, config.rules, config)
	var dstobj object.Object
	var (
		skip, skipBytes int64
		lastUpdate      time.Time
	)
	flushProgress := func() {
		skipped.IncrInt64(skip)
		skippedBytes.IncrInt64(skipBytes)
		incrHandled(int(skip))
		skip, skipBytes = 0, 0
	}
	defer flushProgress()
	skipIt := func(obj object.Object) {
		skip++
		skipBytes += obj.Size()
		if skip > 100 || time.Since(lastUpdate) > time.Millisecond*100 {
			lastUpdate = time.Now()
			flushProgress()
		}
	}
	for obj := range srckeys {
		if obj == nil {
			return fmt.Errorf("listing failed, stop syncing, waiting for pending ones")
		}
		if !config.Dirs && obj.IsDir() {
			logger.Debug("Ignore directory ", obj.Key())
			continue
		}
		if config.Limit >= 0 {
			if config.Limit == 0 {
				return nil
			}
			config.Limit--
		}
		incrTotal(1)

		if dstobj != nil && obj.Key() > dstobj.Key() {
			if handleExtraObject(tasks, dstobj, config) {
				return nil
			}
			dstobj = nil
		}
		if dstobj == nil {
			for dstobj = range dstkeys {
				if dstobj == nil {
					return fmt.Errorf("listing failed, stop syncing, waiting for pending ones")
				}
				if obj.Key() <= dstobj.Key() {
					break
				}
				if handleExtraObject(tasks, dstobj, config) {
					return nil
				}
				dstobj = nil
			}
		}

		// FIXME: there is a race when source is modified during coping
		if dstobj == nil || obj.Key() < dstobj.Key() {
			if config.Existing {
				skipIt(obj)
				continue
			}
			tasks <- obj
		} else { // obj.key == dstobj.key
			if config.IgnoreExisting {
				skipIt(obj)
				dstobj = nil
				continue
			}
			if config.ForceUpdate ||
				(config.Update && obj.Mtime().Unix() > dstobj.Mtime().Unix()) ||
				(!config.Update && obj.Size() != dstobj.Size()) {
				tasks <- obj
			} else if config.Update && obj.Mtime().Unix() < dstobj.Mtime().Unix() {
				skipIt(obj)
			} else if config.CheckAll { // two objects are likely the same
				tasks <- withSize(obj, markChecksum)
			} else if config.DeleteSrc {
				if obj.IsDir() {
					srcDelayDelMu.Lock()
					srcDelayDel = append(srcDelayDel, obj.Key())
					srcDelayDelMu.Unlock()
				} else {
					tasks <- withSize(obj, markDeleteSrc)
				}
			} else if config.Perms && needCopyPerms(obj, dstobj) {
				tasks <- withSize(obj, markCopyPerms)
			} else {
				skipIt(obj)
			}
			dstobj = nil
		}
	}
	if config.DeleteDst {
		if dstobj != nil {
			if handleExtraObject(tasks, dstobj, config) {
				return nil
			}
		}
		for dstobj = range dstkeys {
			if dstobj == nil {
				return fmt.Errorf("listing failed, stop syncing, waiting for pending ones")
			}
			if handleExtraObject(tasks, dstobj, config) {
				return nil
			}
		}
	}
	return nil
}

type rule struct {
	pattern string
	include bool
}

func parseRule(name, p string) rule {
	if runtime.GOOS == "windows" {
		p = strings.Replace(p, "\\", "/", -1)
	}
	return rule{pattern: p, include: name == "-include"}
}

func parseIncludeRules(args []string) (rules []rule) {
	l := len(args)
	for i, a := range args {
		if strings.HasPrefix(a, "--") {
			a = a[1:]
		}
		if l-1 > i && (a == "-include" || a == "-exclude") {
			if _, err := path.Match(args[i+1], "xxxx"); err != nil {
				logger.Warnf("ignore invalid pattern: %s %s", a, args[i+1])
				continue
			}
			rules = append(rules, parseRule(a, args[i+1]))
		} else if strings.HasPrefix(a, "-include=") || strings.HasPrefix(a, "-exclude=") {
			if s := strings.Split(a, "="); len(s) == 2 && s[1] != "" {
				if _, err := path.Match(s[1], "xxxx"); err != nil {
					logger.Warnf("ignore invalid pattern: %s", a)
					continue
				}
				rules = append(rules, parseRule(s[0], s[1]))
			}
		}
	}
	return
}

func filterKey(o object.Object, now time.Time, rules []rule, config *Config) bool {
	var ok bool = true
	if !o.IsDir() && !o.IsSymlink() {
		ok = o.Size() >= int64(config.MinSize) && o.Size() <= int64(config.MaxSize)
		if ok && config.MaxAge > 0 {
			ok = o.Mtime().After(now.Add(-config.MaxAge))
		}
		if ok && config.MinAge > 0 {
			ok = o.Mtime().Before(now.Add(-config.MinAge))
		}
		if ok && !config.StartTime.IsZero() {
			ok = o.Mtime().After(config.StartTime)
		}
		if ok && !config.EndTime.IsZero() {
			ok = o.Mtime().Before(config.EndTime)
		}
	}
	if ok {
		if config.MatchFullPath {
			ok = matchFullPath(rules, o.Key())
		} else {
			ok = matchLeveledPath(rules, o.Key())
		}
	}
	return ok
}

func filter(keys <-chan object.Object, rules []rule, config *Config) <-chan object.Object {
	r := make(chan object.Object)
	now := time.Now()
	go func() {
		for o := range keys {
			if o == nil {
				// Telling that the listing has failed
				r <- nil
				break
			}
			if filterKey(o, now, rules, config) {
				r <- o
			} else {
				logger.Debugf("exclude %s size: %d, mtime: %s", o.Key(), o.Size(), o.Mtime())
				excluded.Increment()
				excludedBytes.IncrInt64(o.Size())
			}
		}
		close(r)
	}()
	return r
}

func matchTwoStar(p string, s []string) bool {
	if len(s) == 0 {
		return p == "*"
	}
	idx := strings.Index(p, "**")
	if idx == -1 {
		ok, _ := path.Match(p, strings.Join(s, "/"))
		return ok
	}
	ok, _ := path.Match(p[:idx+1], s[0])
	if !ok {
		return false
	}
	for i := 0; i <= len(s); i++ {
		tp := p[idx+1:]
		if i == 0 {
			tp = p[:idx] + p[idx+1:]
		}
		if matchTwoStar(tp, s[i:]) {
			return true
		}
	}
	return false
}

func matchPrefix(p, s []string) bool {
	if len(p) == 0 || len(s) == 0 {
		return len(p) == len(s)
	}
	first := p[0]
	n := len(s)
	switch {
	case first == "***":
		return true
	case strings.Contains(first, "**"):
		for i := 1; i <= n; i++ {
			if matchTwoStar(first, s[:i]) && matchPrefix(p[1:], s[i:]) {
				return true
			}
		}
		return false
	default:
		ok, _ := path.Match(first, s[0])
		return ok && matchPrefix(p[1:], s[1:])
	}
}

func matchSuffix(p, s []string) bool {
	if len(p) == 0 {
		return true
	}
	last := p[len(p)-1]
	if len(s) == 0 {
		return len(p) == 1 && (last == "***" || last == "**")
	}
	prefix := p[:len(p)-1]
	n := len(s)
	switch {
	case last == "***":
		for i := 0; i < n; i++ {
			if matchSuffix(prefix, s[:i]) {
				return true
			}
		}
		return false
	case strings.Contains(last, "**"):
		for i := 0; i < n; i++ {
			if matchTwoStar(last, s[i:]) && matchSuffix(prefix, s[:i]) {
				return true
			}
		}
		return false
	default:
		ok, _ := path.Match(last, s[n-1])
		return ok && matchSuffix(prefix, s[:n-1])
	}
}

func matchFullPath(rules []rule, key string) bool {
	ps := strings.Split(key, "/")
	for _, rule := range rules {
		p := strings.Split(rule.pattern, "/")
		var ok bool
		if p[0] == "" {
			if ps[0] != "" {
				p = p[1:]
			}
			ok = matchPrefix(p, ps)
		} else {
			ok = matchSuffix(p, ps)
		}
		if ok {
			if rule.include {
				break // try next level
			} else {
				return false
			}
		}
	}
	return true
}

// Consistent with rsync behavior, the matching order is adjusted according to the order of the "include" and "exclude" options
func matchLeveledPath(rules []rule, key string) bool {
	parts := strings.Split(key, "/")
	for i := range parts {
		if parts[i] == "" {
			continue
		}
		for _, rule := range rules {
			ps := parts[:i+1]
			p := strings.Split(rule.pattern, "/")
			if i < len(parts)-1 && (p[len(p)-1] == "" || p[len(p)-1] == "***") {
				ps = append(append([]string{}, ps...), "") // don't overwrite parts
			}
			var ok bool
			if p[0] == "" {
				if ps[0] != "" {
					p = p[1:]
				}
				ok = matchPrefix(p, ps)
			} else {
				ok = matchSuffix(p, ps)
			}
			if ok {
				if rule.include {
					break // try next level
				} else {
					return false
				}
			}
		}
	}
	return true
}

func listCommonPrefix(store object.ObjectStorage, prefix string, cp chan object.Object, followLink bool) (chan object.Object, error) {
	var total []object.Object
	var objs []object.Object
	var err error
	var nextToken string
	var marker string
	var hasMore bool
	var thisListMaxResults int64 = maxResults
	if strings.HasPrefix(store.String(), "file://") || strings.HasPrefix(store.String(), "nfs://") ||
		strings.HasPrefix(store.String(), "gluster://") || strings.HasPrefix(store.String(), "jfs://") ||
		strings.HasPrefix(store.String(), "hdfs://") || strings.HasPrefix(store.String(), "webdav://") {
		thisListMaxResults = math.MaxInt64
	}
	for {
		objs, hasMore, nextToken, err = store.List(prefix, marker, nextToken, "/", thisListMaxResults, followLink)
		if err != nil {
			return nil, err
		}
		if len(objs) > 0 {
			total = append(total, objs...)
			marker = objs[len(objs)-1].Key()
		}
		if !hasMore {
			break
		}
	}
	srckeys := make(chan object.Object, 1000)
	go func() {
		defer close(srckeys)
		for _, o := range total {
			if o.IsDir() && o.Key() > prefix {
				if cp != nil {
					cp <- o
				}
			} else {
				srckeys <- o
			}
		}
	}()
	return srckeys, nil
}

func produceFromList(tasks chan<- object.Object, src, dst object.ObjectStorage, config *Config) error {
	f, err := os.Open(config.FilesFrom)
	if err != nil {
		return fmt.Errorf("open %s: %s", config.FilesFrom, err)
	}
	defer f.Close()

	prefixs := make(chan string, config.Threads)
	var wg sync.WaitGroup
	wg.Add(config.Threads)
	for i := 0; i < config.Threads; i++ {
		go func() {
			defer wg.Done()
			for key := range prefixs {
				if !strings.HasSuffix(key, "/") {
					if err := produceSingleObject(tasks, src, dst, key, config); err == nil {
						listedPrefix.Increment()
						continue
					} else if errors.Is(err, ignoreDir) {
						key += "/"
					}
				}
				logger.Debugf("start listing prefix %s", key)
				err = startProducer(tasks, src, dst, key, config.ListDepth, config)
				if err != nil {
					logger.Errorf("list prefix %s: %s", key, err)
					failed.Increment()
				}
				listedPrefix.Increment()
			}
		}()
	}

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		key := scanner.Text()
		if key == "" {
			continue
		}
		trimKey := strings.TrimRightFunc(key, unicode.IsSpace)
		if trimKey != key {
			logger.Infof("found a prefix with a space character:%q", key)
		}
		prefixs <- trimKey
	}
	close(prefixs)

	wg.Wait()
	listedPrefix.Done()
	return nil
}

var ignoreDir = errors.New("ignore dir")

func produceSingleObject(tasks chan<- object.Object, src, dst object.ObjectStorage, key string, config *Config) error {
	obj, err := src.Head(key)
	if err == nil && (!obj.IsDir() || obj.IsSymlink() && config.Links || obj.IsDir() && config.Dirs && strings.HasSuffix(key, "/")) {
		var srckeys = make(chan object.Object, 1)
		srckeys <- obj
		close(srckeys)
		if dobj, e := dst.Head(key); e == nil || os.IsNotExist(e) {
			var dstkeys = make(chan object.Object, 1)
			if dobj != nil {
				dstkeys <- dobj
			}
			close(dstkeys)
			logger.Debugf("produce single key %s", key)
			_ = produce(tasks, srckeys, dstkeys, config)
			return nil
		} else {
			logger.Warnf("head %s from %s: %s", key, dst, e)
			err = e
		}
	} else if err != nil {
		logger.Warnf("head %s from %s: %s", key, src, err)
	} else {
		err = ignoreDir
	}
	return err
}

func startProducer(tasks chan<- object.Object, src, dst object.ObjectStorage, prefix string, listDepth int, config *Config) error {
	config.concurrentList <- 1
	defer func() {
		<-config.concurrentList
	}()
	if config.Limit == 1 && len(config.rules) == 0 {
		if produceSingleObject(tasks, src, dst, prefix, config) == nil {
			return nil
		}
	}
	if config.ListThreads <= 1 || listDepth <= 0 {
		return startSingleProducer(tasks, src, dst, prefix, config)
	}

	commonPrefix := make(chan object.Object, 1000)
	done := make(chan bool)
	go func() {
		defer close(done)
		var mu sync.Mutex
		processing := make(map[string]bool)
		var wg sync.WaitGroup
		defer wg.Wait()
		for c := range commonPrefix {
			mu.Lock()
			if processing[c.Key()] {
				mu.Unlock()
				continue
			}
			processing[c.Key()] = true
			mu.Unlock()

			if len(config.rules) > 0 && !matchLeveledPath(config.rules, c.Key()) {
				logger.Infof("exclude prefix %s", c.Key())
				continue
			}
			if c.Key() < config.Start {
				logger.Infof("ignore prefix %s", c.Key())
				continue
			}
			if config.End != "" && c.Key() > config.End {
				logger.Infof("ignore prefix %s", c.Key())
				continue
			}
			wg.Add(1)
			go func(prefix string) {
				defer wg.Done()
				err := startProducer(tasks, src, dst, prefix, listDepth-1, config)
				if err != nil {
					logger.Errorf("list prefix %s: %s", prefix, err)
					failed.Increment()
				}
			}(c.Key())
		}
	}()

	srckeys, err := listCommonPrefix(src, prefix, commonPrefix, !config.Links)
	if err == utils.ENOTSUP {
		return startSingleProducer(tasks, src, dst, prefix, config)
	} else if err != nil {
		return fmt.Errorf("list %s with delimiter: %s", src, err)
	}
	var dcp chan object.Object
	if config.DeleteDst {
		dcp = commonPrefix // search common prefix in dst
	}
	var dstkeys <-chan object.Object
	if config.ForceUpdate {
		t := make(chan object.Object)
		close(t)
		dstkeys = t
	} else {
		dstkeys, err = listCommonPrefix(dst, prefix, dcp, !config.Links)
		if err == utils.ENOTSUP {
			return startSingleProducer(tasks, src, dst, prefix, config)
		} else if err != nil {
			return fmt.Errorf("list %s with delimiter: %s", dst, err)
		}
	}
	// sync returned objects
	if err := produce(tasks, srckeys, dstkeys, config); err != nil {
		return err
	}
	// consume all the keys from dst
	for range dstkeys {
	}
	close(commonPrefix)

	<-config.concurrentList
	<-done
	config.concurrentList <- 1
	return nil
}

// Sync syncs all the keys between to object storage
func Sync(src, dst object.ObjectStorage, config *Config) error {
	if strings.HasPrefix(src.String(), "file://") && strings.HasPrefix(dst.String(), "file://") {
		major, minor := utils.GetKernelVersion()
		// copy_file_range() system call first appeared in Linux 4.5, and reworked in 5.3
		// Go requires kernel >= 5.3 to use copy_file_range(), see:
		// https://github.com/golang/go/blob/go1.17.11/src/internal/poll/copy_file_range_linux.go#L58-L66
		if major > 5 || (major == 5 && minor >= 3) {
			d1 := utils.GetDev(src.String()[7:]) // remove prefix "file://"
			d2 := utils.GetDev(dst.String()[7:])
			if d1 != -1 && d1 == d2 {
				object.TryCFR = true
			}
		}
	}

	if config.Inplace {
		object.PutInplace = true
	}

	var bufferSize = 10240
	if config.Manager != "" {
		// No support for work-stealing, so workers shouldnot buffer tasks to prevent piling up in their own queues, which could cause imbalance among workers.
		bufferSize = 0
	}
	tasks := make(chan object.Object, bufferSize)
	wg := sync.WaitGroup{}
	concurrent = make(chan int, config.Threads)
	if config.BWLimit > 0 {
		bps := float64(config.BWLimit*1e6/8) * 0.85 // 15% overhead
		limiter = ratelimit.NewBucketWithRate(bps, int64(bps)/10)
	}

	progress := utils.NewProgress(config.Verbose || config.Quiet || config.Manager != "")
	handled = progress.AddCountBar("Scanned objects", 0)
	excluded = progress.AddCountSpinner("Excluded objects")
	excludedBytes = progress.AddByteSpinner("Excluded bytes")
	skipped = progress.AddCountSpinner("Skipped objects")
	skippedBytes = progress.AddByteSpinner("Skipped bytes")
	extra = progress.AddCountSpinner("Extra objects")
	extraBytes = progress.AddByteSpinner("Extra bytes")
	pending = progress.AddCountSpinner("Pending objects")
	copied = progress.AddCountSpinner("Copied objects")
	copiedBytes = progress.AddByteSpinner("Copied bytes")
	if config.CheckAll || config.CheckNew || config.CheckChange {
		checked = progress.AddCountSpinner("Checked objects")
		checkedBytes = progress.AddByteSpinner("Checked bytes")
	}
	if config.DeleteSrc || config.DeleteDst {
		deleted = progress.AddCountSpinner("Deleted objects")
	}

	syncExitFunc := func() error {
		if config.Manager == "" {
			pending.SetCurrent(0)
			incrHandled(0)
			total := handled.GetTotal()
			progress.Done()

			msg := fmt.Sprintf("Found: %d, excluded: %d (%s), skipped: %d (%s), copied: %d (%s), extra: %d (%s)", total,
				excluded.Current(), formatSize(excludedBytes.Current()),
				skipped.Current(), formatSize(skippedBytes.Current()),
				copied.Current(), formatSize(copiedBytes.Current()),
				extra.Current(), formatSize(extraBytes.Current()))
			if checked != nil {
				msg += fmt.Sprintf(", checked: %d (%s)", checked.Current(), formatSize(checkedBytes.Current()))
			}
			if deleted != nil {
				msg += fmt.Sprintf(", deleted: %d", deleted.Current())
			}
			if failed != nil {
				msg += fmt.Sprintf(", failed: %d", failed.Current())
			}
			if total-handled.Current()-extra.Current() > 0 {
				msg += fmt.Sprintf(", lost: %d", total-handled.Current())
			}
			logger.Info(msg)

			if failed != nil {
				if n := failed.Current(); n > 0 || total > handled.Current()+extra.Current() {
					return fmt.Errorf("failed to handle %d objects", n+total-handled.Current())
				}
			}
		} else {
			sendStats(config.Manager)
			for len(srcDelayDel) > 0 {
				sendStats(config.Manager)
			}
			logger.Infof("This worker process has already completed its tasks")
		}
		return nil
	}

	if !config.Dry {
		failed = progress.AddCountSpinner("Failed objects")
		if config.MaxFailure > 0 {
			go func() {
				for {
					if failed.Current() >= config.MaxFailure {
						logger.Infof("the maximum error limit of %d was reached, stop now", config.MaxFailure)
						_ = syncExitFunc()
						os.Exit(1)
					}
					time.Sleep(time.Millisecond * 100)
				}
			}()
		}
	}

	if config.Manager == "" && config.FilesFrom != "" {
		listedPrefix = progress.AddCountSpinner("Prefix")
	}

	go func() {
		for {
			pending.SetCurrent(int64(len(tasks)))
			time.Sleep(time.Millisecond * 100)
		}
	}()

	initSyncMetrics(config)
	for i := 0; i < config.Threads; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			worker(tasks, src, dst, config)
		}()
	}

	if len(config.Exclude) > 0 {
		config.rules = parseIncludeRules(os.Args)
	}

	if config.Manager == "" {
		if len(config.Workers) > 0 {
			addr, err := startManager(config, tasks)
			if err != nil {
				return err
			}
			launchWorker(addr, config, &wg)
		}
		logger.Infof("Syncing from %s to %s", src, dst)
		if config.Start != "" {
			logger.Infof("first key: %q", config.Start)
		}
		if config.End != "" {
			logger.Infof("last key: %q", config.End)
		}
		config.concurrentList = make(chan int, config.ListThreads)
		var err error
		if config.FilesFrom != "" {
			err = produceFromList(tasks, src, dst, config)
		} else {
			err = startProducer(tasks, src, dst, "", config.ListDepth, config)
		}
		if err != nil {
			return err
		}
		close(tasks)
	} else {
		go fetchJobs(tasks, config)
		go func() {
			for {
				sendStats(config.Manager)
				time.Sleep(time.Second)
			}
		}()
	}
	wg.Wait()

	if config.Manager == "" {
		delayDelFunc := func(storage object.ObjectStorage, keys []string) {
			if len(keys) > 0 {
				logger.Infof("delete %d dirs from %s", len(keys), storage)
				sort.Strings(keys)
			}
			for i := len(keys) - 1; i >= 0; i-- {
				incrHandled(1)
				deleteObj(storage, keys[i], config.Dry)
			}
		}
		delWg := sync.WaitGroup{}

		delWg.Add(1)
		go func() {
			delayDelFunc(src, srcDelayDel)
			delWg.Done()
		}()
		delWg.Add(1)
		go func() {
			delayDelFunc(dst, dstDelayDel)
			delWg.Done()
		}()
		delWg.Wait()
	}
	return syncExitFunc()
}

func initSyncMetrics(config *Config) {
	if config.Registerer != nil {
		config.Registerer.MustRegister(
			prometheus.NewCounterFunc(prometheus.CounterOpts{
				Name: "scanned",
				Help: "Scanned objects",
			}, func() float64 {
				return float64(handled.Total())
			}),
			prometheus.NewCounterFunc(prometheus.CounterOpts{
				Name: "excluded",
				Help: "Excluded objects",
			}, func() float64 {
				return float64(excluded.Current())
			}),
			prometheus.NewCounterFunc(prometheus.CounterOpts{
				Name: "excluded_bytes",
				Help: "Excluded bytes",
			}, func() float64 {
				return float64(copied.Current())
			}),
			prometheus.NewCounterFunc(prometheus.CounterOpts{
				Name: "extra",
				Help: "Extra objects",
			}, func() float64 {
				return float64(excluded.Current())
			}),
			prometheus.NewCounterFunc(prometheus.CounterOpts{
				Name: "extra_bytes",
				Help: "Extra bytes",
			}, func() float64 {
				return float64(copied.Current())
			}),
			prometheus.NewCounterFunc(prometheus.CounterOpts{
				Name: "handled",
				Help: "Handled objects",
			}, func() float64 {
				return float64(handled.Current())
			}),
			prometheus.NewGaugeFunc(prometheus.GaugeOpts{
				Name: "pending",
				Help: "Pending objects",
			}, func() float64 {
				return float64(pending.Current())
			}),
			prometheus.NewCounterFunc(prometheus.CounterOpts{
				Name: "copied",
				Help: "Copied objects",
			}, func() float64 {
				return float64(copied.Current())
			}),
			prometheus.NewCounterFunc(prometheus.CounterOpts{
				Name: "copied_bytes",
				Help: "Copied bytes",
			}, func() float64 {
				return float64(copiedBytes.Current())
			}),
			prometheus.NewCounterFunc(prometheus.CounterOpts{
				Name: "skipped",
				Help: "Skipped objects",
			}, func() float64 {
				return float64(skipped.Current())
			}),
			prometheus.NewCounterFunc(prometheus.CounterOpts{
				Name: "skipped_bytes",
				Help: "Skipped bytes",
			}, func() float64 {
				return float64(skippedBytes.Current())
			}),
		)
		if failed != nil {
			config.Registerer.MustRegister(prometheus.NewCounterFunc(prometheus.CounterOpts{
				Name: "failed",
				Help: "Failed objects",
			}, func() float64 {
				return float64(failed.Current())
			}))
		}
		if deleted != nil {
			config.Registerer.MustRegister(prometheus.NewCounterFunc(prometheus.CounterOpts{
				Name: "deleted",
				Help: "Deleted objects",
			}, func() float64 {
				return float64(deleted.Current())
			}))
		}
		if checked != nil && checkedBytes != nil {
			config.Registerer.MustRegister(
				prometheus.NewCounterFunc(prometheus.CounterOpts{
					Name: "checked",
					Help: "Checked objects",
				}, func() float64 {
					return float64(checked.Current())
				}),
				prometheus.NewCounterFunc(prometheus.CounterOpts{
					Name: "checked_bytes",
					Help: "Checked bytes",
				}, func() float64 {
					return float64(checkedBytes.Current())
				}))
		}
		if listedPrefix != nil {
			config.Registerer.MustRegister(prometheus.NewCounterFunc(prometheus.CounterOpts{
				Name: "Prefix",
				Help: "listed prefix",
			}, func() float64 {
				return float64(listedPrefix.Current())
			}))
		}
	}
}

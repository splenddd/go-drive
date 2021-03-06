package drive_util

import (
	"go-drive/common/errors"
	"go-drive/common/i18n"
	"go-drive/common/task"
	"go-drive/common/types"
	"go-drive/common/utils"
	"io"
	"io/ioutil"
	"net/http"
	"net/http/httputil"
	url2 "net/url"
	"os"
	"path"
	"strconv"
)

func GetIEntry(entry types.IEntry, test func(iEntry types.IEntry) bool) types.IEntry {
	if entry == nil {
		return nil
	}
	for {
		if test != nil && test(entry) {
			return entry
		}
		if wrapper, ok := entry.(types.IEntryWrapper); ok {
			entry = wrapper.GetIEntry()
		} else {
			break
		}
	}
	if test != nil {
		return nil
	}
	return entry
}

func Copy(dst io.Writer, src io.Reader, ctx types.TaskCtx) (written int64, err error) {
	buf := make([]byte, 32*1024)
	for {
		if ctx.Canceled() {
			return written, task.ErrorCanceled
		}
		w, ee := io.CopyBuffer(dst, src, buf)
		if ee != nil {
			break
		}
		if w == 0 {
			break
		}
		written += w
		ctx.Progress(w, false)
	}
	return
}

func CopyReaderToTempFile(reader io.Reader, ctx types.TaskCtx, tempDir string) (*os.File, error) {
	file, e := ioutil.TempFile(tempDir, "drive-copy")
	if e != nil {
		return nil, e
	}
	_, e = Copy(file, reader, ctx)
	if e != nil {
		_ = file.Close()
		_ = os.Remove(file.Name())
		return nil, e
	}
	_, e = file.Seek(0, 0)
	if e != nil {
		_ = file.Close()
		_ = os.Remove(file.Name())
		return nil, e
	}
	return file, nil
}

func GetIContentReader(content types.IContent) (io.ReadCloser, error) {
	u, e := content.GetURL()
	if e == nil {
		return GetURL(u.URL, u.Header)
	}
	return content.GetReader()
}

func CopyIContentToTempFile(content types.IContent, ctx types.TaskCtx, tempDir string) (*os.File, error) {
	reader, e := GetIContentReader(content)
	if e != nil {
		return nil, e
	}
	return CopyReaderToTempFile(reader, ctx, tempDir)
}

func DownloadIContent(content types.IContent, w http.ResponseWriter, req *http.Request, forceProxy bool) error {
	u, e := content.GetURL()
	if e == nil {
		if u.Proxy || forceProxy || u.Header != nil {
			dest, e := url2.Parse(u.URL)
			if e != nil {
				return e
			}
			proxy := httputil.ReverseProxy{Director: func(r *http.Request) {
				r.URL = dest
				r.Host = dest.Host
				r.Header.Del("Referer")
				r.Header.Del("Authorization")
				if u.Header != nil {
					for k, v := range u.Header {
						r.Header.Set(k, v)
					}
				}
			}}

			defer func() {
				if i := recover(); i != nil && i != http.ErrAbortHandler {
					panic(i)
				}
			}()

			proxy.ServeHTTP(w, req)
			return nil
		} else {
			w.WriteHeader(http.StatusFound)
			w.Header().Set("Location", u.URL)
		}
		return nil
	}
	if !err.IsUnsupportedError(e) {
		return e
	}
	reader, e := content.GetReader()
	if e != nil {
		return e
	}
	defer func() { _ = reader.Close() }()
	readSeeker, ok := reader.(io.ReadSeeker)
	if ok {
		http.ServeContent(
			w, req, content.Name(),
			utils.Time(content.ModTime()),
			readSeeker)
		return nil
	}

	w.Header().Set("Content-Length", strconv.FormatInt(content.Size(), 10))
	if req.Method != http.MethodHead {
		_, e = io.Copy(w, reader)
	}
	return e
}

// region copy all

type EntryNode struct {
	types.IEntry
	children []EntryNode
}

type DoCopy = func(from types.IEntry, driveTo types.IDrive, to string, ctx types.TaskCtx) error
type CopyCallback = func(entry types.IEntry, allProcessed bool, ctx types.TaskCtx) error

func buildEntriesTree(entry types.IEntry, ctx types.TaskCtx, bytesProgress bool) (EntryNode, error) {
	if ctx.Canceled() {
		return EntryNode{}, task.ErrorCanceled
	}
	if bytesProgress {
		if entry.Type().IsFile() {
			ctx.Total(entry.Size(), false)
		}
	} else {
		ctx.Total(1, false)
	}
	r := EntryNode{entry, nil}
	if entry.Type().IsFile() {
		return r, nil
	}
	entries, e := entry.Drive().List(entry.Path())
	if e != nil {
		return r, e
	}
	children := make([]EntryNode, len(entries))
	for i, e := range entries {
		node, ee := buildEntriesTree(e, ctx, bytesProgress)
		if ee != nil {
			return r, ee
		}
		children[i] = node
	}
	r.children = children
	return r, nil
}

func BuildEntriesTree(root types.IEntry, ctx types.TaskCtx, bytesProgress bool) (EntryNode, error) {
	if ctx == nil {
		ctx = task.DummyContext()
	}
	return buildEntriesTree(root, ctx, bytesProgress)
}

func flattenEntriesTree(root EntryNode, result []EntryNode) []EntryNode {
	result = append(result, root)
	if root.children != nil {
		for _, e := range root.children {
			result = flattenEntriesTree(e, result)
		}
	}
	return result
}

func FlattenEntriesTree(root EntryNode) []EntryNode {
	result := make([]EntryNode, 0)
	return flattenEntriesTree(root, result)
}

func copyAll(entry EntryNode, driveTo types.IDrive, to string, override bool,
	ctx types.TaskCtx, newParent bool, doCopy DoCopy, after CopyCallback) (bool, error) {
	if ctx.Canceled() {
		return false, task.ErrorCanceled
	}
	var dstType types.EntryType
	dstExists := false
	if newParent {
		dstExists = false
	} else {
		dst, e := driveTo.Get(to)
		if e != nil && !err.IsNotFoundError(e) {
			return false, e
		}
		dstExists = e == nil
		if dstExists {
			dstType = dst.Type()
		}
	}

	allProcessed := true
	if entry.Type().IsDir() {
		dirCreate := false
		if dstExists {
			if dstType.IsFile() {
				return false, err.NewNotAllowedMessageError(i18n.T("drive.copy_type_mismatch1", entry.Path(), to))
			}
		} else {
			_, e := driveTo.MakeDir(to)
			if e != nil {
				return false, e
			}
			dirCreate = true
		}
		if entry.children != nil {
			for _, e := range entry.children {
				r, ee := copyAll(e, driveTo, utils.CleanPath(path.Join(to, utils.PathBase(e.Path()))), override, ctx, dirCreate, doCopy, after)
				if ee != nil {
					return false, ee
				}
				if !r {
					allProcessed = false
				}
			}
		}
	}

	if entry.Type().IsFile() {
		if dstExists {
			if dstType.IsDir() {
				return false, err.NewNotAllowedMessageError(i18n.T("drive.copy_type_mismatch2", entry.Path(), to))
			}
			if !override {
				// skip
				return false, nil
			}
		}

		if e := doCopy(entry.IEntry, driveTo, to, ctx); e != nil {
			return false, e
		}
	}
	if e := after(entry, allProcessed, ctx); e != nil {
		return false, e
	}
	return allProcessed, nil
}

func CopyAll(entry types.IEntry, driveTo types.IDrive, to string, override bool,
	ctx types.TaskCtx, doCopy DoCopy, after CopyCallback) error {
	tree, e := BuildEntriesTree(entry, ctx, true)
	if e != nil {
		return e
	}
	if after == nil {
		after = func(entry types.IEntry, fullProcessed bool, ctx types.TaskCtx) error { return nil }
	}
	_, e = copyAll(tree, driveTo, to, override, ctx, false, doCopy, after)
	return e
}

func CopyEntry(from types.IEntry, driveTo types.IDrive, to string, override bool, ctx types.TaskCtx, tempDir string) error {
	content, ok := from.(types.IContent)
	if !ok {
		return err.NewNotAllowedMessageError(i18n.T("drive.file_not_readable", from.Path()))
	}
	file, e := CopyIContentToTempFile(content, task.DummyContext(), tempDir)
	if e != nil {
		return e
	}
	defer func() {
		_ = file.Close()
		_ = os.Remove(file.Name())
	}()
	_, e = driveTo.Save(to, from.Size(), override, file, ctx)
	return e
}

// endregion

type progressReader struct {
	r   io.Reader
	ctx types.TaskCtx
}

func (p *progressReader) Read(b []byte) (n int, err error) {
	read, e := p.r.Read(b)
	if e == nil || e == io.EOF {
		p.ctx.Progress(int64(read), false)
	}
	return read, e
}

func ProgressReader(reader io.Reader, ctx types.TaskCtx) io.Reader {
	return &progressReader{r: reader, ctx: ctx}
}

func GetURL(u string, header types.SM) (io.ReadCloser, error) {
	req, e := http.NewRequest("GET", u, nil)
	if e != nil {
		return nil, e
	}
	if header != nil {
		for k, v := range header {
			req.Header.Set(k, v)
		}
	}
	resp, e := http.DefaultClient.Do(req)
	if e != nil {
		return nil, e
	}
	if resp.StatusCode != 200 {
		_ = resp.Body.Close()
		return nil, err.NewRemoteApiError(resp.StatusCode, i18n.T("util.request_failed", strconv.Itoa(resp.StatusCode)))
	}
	return resp.Body, nil
}

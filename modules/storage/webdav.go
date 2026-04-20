// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package storage

import (
	"context"
	"crypto/tls"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"code.gitea.io/gitea/modules/setting"
)

// 大文件分块上传阈值（50MB），超过此大小的文件将分块上传以避免 413 Payload Too Large
const webdavChunkSize = 50 * 1024 * 1024

var _ ObjectStorage = &WebDAVStorage{}

// WebDAVStorage represents a WebDAV storage
type WebDAVStorage struct {
	ctx    context.Context
	config *setting.WebDAVStorageConfig
	client *http.Client

	basePath string // normalized path part of config.URL, ends with "/"
}

// NewWebDAVStorage returns a WebDAV storage
func NewWebDAVStorage(ctx context.Context, cfg *setting.Storage) (ObjectStorage, error) {
	webdavCfg := &cfg.WebDAVConfig

	baseURL, err := url.Parse(webdavCfg.URL)
	if err != nil {
		return nil, fmt.Errorf("invalid WebDAV URL: %w", err)
	}

	basePath := strings.TrimSuffix(baseURL.Path, "/") + "/"
	if basePath == "" {
		basePath = "/"
	}

	// 连接超时：仅限制建立 TCP 连接的时间，不限制 body 传输
	// 这样大文件上传不会因传输时间长而被中断
	connectTimeout := time.Duration(webdavCfg.Timeout) * time.Second
	if connectTimeout == 0 {
		connectTimeout = 30 * time.Second // 默认 30s 连接超时
	}

	tlsCfg := &tls.Config{}
	if webdavCfg.InsecureSkipVerify {
		tlsCfg.InsecureSkipVerify = true
	}

	return &WebDAVStorage{
		ctx:      ctx,
		config:   webdavCfg,
		basePath: basePath,
		client: &http.Client{
			Timeout: 0, // 禁止全局请求超时：body 传输时间不受限制，由 caller 的 context 控制
			Transport: &http.Transport{
				DisableCompression: true,
				TLSClientConfig:    tlsCfg,
				DialContext: (&net.Dialer{
					Timeout: connectTimeout,
				}).DialContext,
			},
		},
	}, nil
}

func (s *WebDAVStorage) getFullPath(p string) string {
	p = strings.TrimPrefix(p, "/")
	return strings.TrimSuffix(s.config.URL, "/") + "/" + p
}

func (s *WebDAVStorage) doRequest(method, p string, body io.Reader, contentLength int64) (*http.Response, error) {
	return s.doRequestWithHeaders(method, p, body, contentLength, nil)
}

func (s *WebDAVStorage) doRequestWithHeaders(method, p string, body io.Reader, contentLength int64, headers http.Header) (*http.Response, error) {
	fullPath := s.getFullPath(p)

	req, err := http.NewRequestWithContext(s.ctx, method, fullPath, body)
	if err != nil {
		return nil, err
	}

	if contentLength >= 0 {
		req.ContentLength = contentLength
	}

	if s.config.Username != "" {
		req.SetBasicAuth(s.config.Username, s.config.Password)
	}

	for k, vals := range headers {
		for _, v := range vals {
			req.Header.Add(k, v)
		}
	}

	return s.client.Do(req)
}

type webdavObject struct {
	storage *WebDAVStorage
	path    string

	size    int64
	modTime time.Time
	isDir   bool

	body   io.ReadCloser
	offset int64
}

func newWebDAVObject(storage *WebDAVStorage, p string) *webdavObject {
	return &webdavObject{
		storage: storage,
		path:    p,
		size:    -1,
	}
}

func (w *webdavObject) fillFromResponse(resp *http.Response) {
	if w.size < 0 {
		switch resp.StatusCode {
		case http.StatusOK:
			if cl := resp.Header.Get("Content-Length"); cl != "" {
				if v, err := strconv.ParseInt(cl, 10, 64); err == nil {
					w.size = v
				}
			}
		case http.StatusPartialContent:
			if total, ok := parseContentRangeTotal(resp.Header.Get("Content-Range")); ok {
				w.size = total
			}
		}
	}

	if w.modTime.IsZero() {
		if lm := resp.Header.Get("Last-Modified"); lm != "" {
			if t, err := time.Parse(http.TimeFormat, lm); err == nil {
				w.modTime = t
			}
		}
	}
}

func (w *webdavObject) openAt(offset int64) error {
	_ = w.Close()

	headers := http.Header{}
	// Avoid getting compressed responses which breaks byte ranges.
	headers.Set("Accept-Encoding", "identity")
	if offset > 0 {
		headers.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}

	resp, err := w.storage.doRequestWithHeaders(http.MethodGet, w.path, nil, -1, headers)
	if err != nil {
		return err
	}

	ok := resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusPartialContent
	if !ok {
		resp.Body.Close()
		return fmt.Errorf("failed to open %s: %s", w.path, resp.Status)
	}

	// If the server ignored Range and returned 200, discard bytes to reach desired offset.
	if offset > 0 && resp.StatusCode == http.StatusOK {
		if _, err := io.CopyN(io.Discard, resp.Body, offset); err != nil {
			resp.Body.Close()
			return fmt.Errorf("failed to seek to %d in %s: %w", offset, w.path, err)
		}
	}

	w.body = resp.Body
	w.offset = offset
	w.fillFromResponse(resp)
	return nil
}

func (w *webdavObject) Read(p []byte) (int, error) {
	if w.size >= 0 && w.offset >= w.size {
		return 0, io.EOF
	}
	if w.body == nil {
		if err := w.openAt(w.offset); err != nil {
			return 0, err
		}
	}
	n, err := w.body.Read(p)
	w.offset += int64(n)
	return n, err
}

func (w *webdavObject) Seek(offset int64, whence int) (int64, error) {
	var newOffset int64
	switch whence {
	case io.SeekStart:
		newOffset = offset
	case io.SeekCurrent:
		newOffset = w.offset + offset
	case io.SeekEnd:
		if w.size < 0 {
			if _, err := w.Stat(); err != nil {
				return 0, err
			}
		}
		newOffset = w.size + offset
	default:
		return 0, fmt.Errorf("invalid whence: %d", whence)
	}

	if newOffset < 0 {
		return 0, fmt.Errorf("invalid offset: %d", newOffset)
	}

	_ = w.Close()
	w.offset = newOffset
	return newOffset, nil
}

func (w *webdavObject) Close() error {
	if w.body == nil {
		return nil
	}
	err := w.body.Close()
	w.body = nil
	return err
}

func (w *webdavObject) Stat() (os.FileInfo, error) {
	if w.size >= 0 {
		return &webdavFileInfo{
			name:    path.Base(w.path),
			size:    w.size,
			mode:    os.FileMode(0644),
			modTime: w.modTime,
			isDir:   w.isDir,
		}, nil
	}

	fi, err := w.storage.Stat(w.path)
	if err != nil {
		return nil, err
	}
	w.size = fi.Size()
	w.modTime = fi.ModTime()
	w.isDir = fi.IsDir()
	return fi, nil
}

// Open opens a file for reading
func (s *WebDAVStorage) Open(p string) (Object, error) {
	obj := newWebDAVObject(s, p)
	if err := obj.openAt(0); err != nil {
		return nil, err
	}
	return obj, nil
}

// Save saves a file to WebDAV storage using streaming upload.
// For files larger than webdavChunkSize, uses chunked upload to avoid 413 Payload Too Large errors.
func (s *WebDAVStorage) Save(p string, r io.Reader, size int64) (int64, error) {
	// 文件大小未知或小于阈值，直接上传
	if size < 0 || size <= webdavChunkSize {
		return s.saveDirect(p, r, size)
	}

	// 大文件分块上传
	return s.saveChunked(p, r, size)
}

// saveDirect 直接上传整个文件
func (s *WebDAVStorage) saveDirect(p string, r io.Reader, size int64) (int64, error) {
	var contentLength int64 = -1
	if size >= 0 {
		contentLength = size
	}

	pr, pw := io.Pipe()
	respCh := make(chan *http.Response, 1)
	errCh := make(chan error, 1)

	go func() {
		resp, err := s.doRequest(http.MethodPut, p, pr, contentLength)
		if err != nil {
			errCh <- err
			return
		}
		respCh <- resp
	}()

	written, copyErr := io.Copy(pw, r)
	pw.CloseWithError(copyErr)

	var resp *http.Response
	select {
	case err := <-errCh:
		return 0, fmt.Errorf("failed to upload %s: %w", p, err)
	case resp = <-respCh:
	}

	if resp != nil {
		defer resp.Body.Close()
	}

	if resp != nil && resp.StatusCode != http.StatusOK &&
		resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent {
		return 0, fmt.Errorf("failed to upload %s: %s", p, resp.Status)
	}

	if copyErr != nil && copyErr != io.EOF {
		return 0, fmt.Errorf("failed to read data to upload: %w", copyErr)
	}

	return written, nil
}

// saveChunked 将大文件分块上传：逐块读取，每块独立 PUT 到远程
// 使用 Content-Range 头追加写入，兼容支持范围写入的 WebDAV 服务器
func (s *WebDAVStorage) saveChunked(p string, r io.Reader, size int64) (int64, error) {
	var totalWritten int64
	buf := make([]byte, 32*1024) // 读缓冲

	for offset := int64(0); offset < size; {
		remaining := size - offset
		chunkLen := int64(webdavChunkSize)
		if remaining < chunkLen {
			chunkLen = remaining
		}

		// 使用 io.LimitReader 读取一个 chunk
		lr := io.LimitReader(r, chunkLen)

		pr, pw := io.Pipe()
		respCh := make(chan *http.Response, 1)
		errCh := make(chan error, 1)

		// 判断是否需要使用 Content-Range
		// 第一个 chunk 用普通 PUT，后续 chunk 用 Content-Range 追加
		var headers http.Header
		if offset > 0 {
			headers = http.Header{}
			headers.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, offset+chunkLen-1, size))
		}

		go func() {
			resp, err := s.doRequestWithHeaders(http.MethodPut, p, pr, chunkLen, headers)
			if err != nil {
				errCh <- err
				return
			}
			respCh <- resp
		}()

		written, copyErr := io.CopyBuffer(pw, lr, buf)
		pw.CloseWithError(copyErr)

		var resp *http.Response
		select {
		case err := <-errCh:
			return totalWritten, fmt.Errorf("failed to upload chunk at offset %d of %s: %w", offset, p, err)
		case resp = <-respCh:
		}

		if resp != nil {
			resp.Body.Close()
		}

		if resp != nil && resp.StatusCode != http.StatusOK &&
			resp.StatusCode != http.StatusCreated &&
			resp.StatusCode != http.StatusNoContent &&
			// 部分服务器对 Content-Range 返回 206 Partial Content
			resp.StatusCode != http.StatusPartialContent {
			return totalWritten, fmt.Errorf("failed to upload chunk at offset %d of %s: %s", offset, p, resp.Status)
		}

		if copyErr != nil && copyErr != io.EOF {
			return totalWritten, fmt.Errorf("failed to read chunk data: %w", copyErr)
		}

		totalWritten += written
		offset += chunkLen
	}

	return totalWritten, nil
}

// Stat returns file info
func (s *WebDAVStorage) Stat(p string) (os.FileInfo, error) {
	req, err := http.NewRequestWithContext(s.ctx, "PROPFIND", s.getFullPath(p), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Depth", "0")
	req.Header.Set("Content-Type", "text/xml")
	if s.config.Username != "" {
		req.SetBasicAuth(s.config.Username, s.config.Password)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, os.ErrNotExist
	}
	if resp.StatusCode != http.StatusMultiStatus {
		return nil, fmt.Errorf("failed to stat %s: %s", p, resp.Status)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	entries, err := parseWebDAVMultiStatus(body)
	if err != nil {
		return nil, fmt.Errorf("failed to parse PROPFIND response: %w", err)
	}

	targetPath := normalizeStoragePath(p)
	reqPrefix := path.Dir(targetPath)
	if reqPrefix == "." {
		reqPrefix = ""
	}

	for _, e := range entries {
		entryPath, err := s.hrefToStoragePath(e.HRef, reqPrefix)
		if err != nil {
			continue
		}
		entryPath = normalizeStoragePath(entryPath)
		if entryPath != targetPath {
			continue
		}

		return &webdavFileInfo{
			name:    path.Base(targetPath),
			size:    e.Size,
			mode:    os.FileMode(0644),
			modTime: e.ModTime,
			isDir:   e.IsDir,
		}, nil
	}

	return nil, os.ErrNotExist
}

// Delete deletes a file
func (s *WebDAVStorage) Delete(p string) error {
	resp, err := s.doRequest(http.MethodDelete, p, nil, -1)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// 404 is acceptable - file already doesn't exist
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("failed to delete %s: %s", p, resp.Status)
	}

	return nil
}

// ServeDirectURL is not supported for WebDAV
func (s *WebDAVStorage) ServeDirectURL(p, name, method string, opt *ServeDirectOptions) (*url.URL, error) {
	return nil, ErrURLNotSupported
}

// IterateObjects iterates across objects in WebDAV storage using PROPFIND
func (s *WebDAVStorage) IterateObjects(prefix string, fn func(string, Object) error) error {
	prefix = normalizeStoragePath(prefix)

	visited := map[string]struct{}{}
	queue := []string{prefix}

	for len(queue) > 0 {
		dir := queue[0]
		queue = queue[1:]

		if _, ok := visited[dir]; ok {
			continue
		}
		visited[dir] = struct{}{}

		reqPath := ""
		if dir != "" {
			reqPath = dir + "/"
		}

		entries, err := s.propfind(reqPath, "1")
		if err != nil {
			return err
		}

		for _, e := range entries {
			entryPath, err := s.hrefToStoragePath(e.HRef, reqPath)
			if err != nil {
				continue
			}

			entryPath = strings.TrimPrefix(entryPath, "/")
			if entryPath == "" {
				continue
			}

			entryDir := strings.TrimSuffix(entryPath, "/")
			if entryDir == dir {
				continue // skip the directory itself
			}

			if e.IsDir {
				queue = append(queue, entryDir)
				continue
			}

			obj := newWebDAVObject(s, entryPath)
			obj.size = e.Size
			obj.modTime = e.ModTime
			obj.isDir = false

			if err := fn(entryPath, obj); err != nil {
				_ = obj.Close()
				return err
			}
			_ = obj.Close()
		}
	}

	return nil
}

// MkdirAll 使用 MKCOL 递归创建目录路径，类似 os.MkdirAll
func (s *WebDAVStorage) MkdirAll(p string) error {
	p = normalizeStoragePath(p)
	if p == "" {
		return nil
	}

	// 逐级创建路径中的每个目录
	parts := strings.Split(p, "/")
	currentPath := ""
	for _, part := range parts {
		if part == "" {
			continue
		}
		if currentPath != "" {
			currentPath += "/"
		}
		currentPath += part

		resp, err := s.doRequest("MKCOL", currentPath+"/", nil, -1)
		if err != nil {
			return fmt.Errorf("MKCOL %s: %w", currentPath, err)
		}
		resp.Body.Close()

		// 201 Created = 新建成功，405 Method Not Allowed = 目录已存在
		if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusMethodNotAllowed {
			return fmt.Errorf("MKCOL %s: %s", currentPath, resp.Status)
		}
	}
	return nil
}

type webdavPropfindEntry struct {
	HRef    string
	Size    int64
	ModTime time.Time
	IsDir   bool
}

func (s *WebDAVStorage) propfind(p, depth string) ([]webdavPropfindEntry, error) {
	req, err := http.NewRequestWithContext(s.ctx, "PROPFIND", s.getFullPath(p), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Depth", depth)
	req.Header.Set("Content-Type", "text/xml")
	if s.config.Username != "" {
		req.SetBasicAuth(s.config.Username, s.config.Password)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, os.ErrNotExist
	}
	if resp.StatusCode != http.StatusMultiStatus {
		return nil, fmt.Errorf("PROPFIND failed: %s", resp.Status)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	return parseWebDAVMultiStatus(body)
}

func parseWebDAVMultiStatus(body []byte) ([]webdavPropfindEntry, error) {
	var multiResp struct {
		Responses []struct {
			HRef     string `xml:"href"`
			PropStat []struct {
				Status string `xml:"status"`
				Prop   struct {
					GetLastModified  string `xml:"getlastmodified"`
					GetContentLength string `xml:"getcontentlength"`
					ResourceType     struct {
						Collection *struct{} `xml:"collection"`
					} `xml:"resourcetype"`
				} `xml:"propstat>prop"`
			} `xml:"propstat"`
		} `xml:"response"`
	}

	if err := xml.Unmarshal(body, &multiResp); err != nil {
		return nil, err
	}

	ret := make([]webdavPropfindEntry, 0, len(multiResp.Responses))
	for _, resp := range multiResp.Responses {
		for _, ps := range resp.PropStat {
			if !isPropStatStatusOK(ps.Status) {
				continue
			}

			var size int64
			if ps.Prop.GetContentLength != "" {
				if v, err := strconv.ParseInt(strings.TrimSpace(ps.Prop.GetContentLength), 10, 64); err == nil {
					size = v
				}
			}

			var modTime time.Time
			if t, ok := parseWebDAVModTime(ps.Prop.GetLastModified); ok {
				modTime = t
			}

			ret = append(ret, webdavPropfindEntry{
				HRef:    resp.HRef,
				Size:    size,
				ModTime: modTime,
				IsDir:   ps.Prop.ResourceType.Collection != nil,
			})
			break
		}
	}

	return ret, nil
}

func isPropStatStatusOK(status string) bool {
	// Example values:
	// * "HTTP/1.1 200 OK"
	// * "200 OK"
	fields := strings.Fields(status)
	for _, f := range fields {
		if code, err := strconv.Atoi(f); err == nil {
			return code == http.StatusOK
		}
	}
	return false
}

func parseWebDAVModTime(modified string) (time.Time, bool) {
	modified = strings.TrimSpace(modified)
	if modified == "" {
		return time.Time{}, false
	}
	for _, format := range []string{
		http.TimeFormat,
		time.RFC1123,
		time.RFC1123Z,
		"Mon, 02 Jan 2006 15:04:05 MST",
		time.RFC3339,
	} {
		if t, err := time.Parse(format, modified); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func normalizeStoragePath(p string) string {
	p = strings.TrimPrefix(p, "./")
	p = strings.TrimPrefix(p, "/")
	p = strings.TrimSuffix(p, "/")
	return p
}

func (s *WebDAVStorage) hrefToStoragePath(href, requestPrefix string) (string, error) {
	u, err := url.Parse(href)
	if err != nil {
		return "", err
	}

	hrefPath := u.Path
	if hrefPath == "" {
		hrefPath = href
	}
	if decoded, err := url.PathUnescape(hrefPath); err == nil {
		hrefPath = decoded
	}

	if strings.HasPrefix(hrefPath, s.basePath) {
		return strings.TrimPrefix(hrefPath, s.basePath), nil
	}
	if baseNoSlash := strings.TrimSuffix(s.basePath, "/"); baseNoSlash != "" && strings.HasPrefix(hrefPath, baseNoSlash) {
		hrefPath = strings.TrimPrefix(hrefPath, baseNoSlash)
		hrefPath = strings.TrimPrefix(hrefPath, "/")
		return hrefPath, nil
	}

	// Some WebDAV servers return relative HREFs for PROPFIND.
	hrefPath = strings.TrimPrefix(hrefPath, "/")
	hrefPath = strings.TrimPrefix(hrefPath, "./")
	reqPrefix := normalizeStoragePath(requestPrefix)
	if reqPrefix != "" {
		if hrefPath == reqPrefix || strings.HasPrefix(hrefPath, reqPrefix+"/") {
			return hrefPath, nil
		}
		return path.Join(reqPrefix, hrefPath), nil
	}
	return hrefPath, nil
}

func parseContentRangeTotal(contentRange string) (int64, bool) {
	// Examples:
	// * "bytes 0-1023/146515"
	// * "bytes 1024-2047/146515"
	// * "bytes */146515"
	parts := strings.Split(contentRange, "/")
	if len(parts) != 2 {
		return 0, false
	}
	totalStr := strings.TrimSpace(parts[1])
	if totalStr == "" || totalStr == "*" {
		return 0, false
	}
	total, err := strconv.ParseInt(totalStr, 10, 64)
	return total, err == nil
}

// webdavFileInfo implements os.FileInfo for WebDAV responses
type webdavFileInfo struct {
	name    string
	size    int64
	mode    os.FileMode
	modTime time.Time
	isDir   bool
}

func (fi *webdavFileInfo) Name() string       { return fi.name }
func (fi *webdavFileInfo) Size() int64        { return fi.size }
func (fi *webdavFileInfo) Mode() os.FileMode  { return fi.mode }
func (fi *webdavFileInfo) ModTime() time.Time { return fi.modTime }
func (fi *webdavFileInfo) IsDir() bool        { return fi.isDir }
func (fi *webdavFileInfo) Sys() any           { return nil }

func init() {
	RegisterStorageType(setting.WebDAVStorageType, NewWebDAVStorage)
}

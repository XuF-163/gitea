// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package storage

import (
	"bytes"
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
	"strings"
	"time"

	"code.gitea.io/gitea/modules/setting"
)

var _ ObjectStorage = &WebDAVStorage{}

// WebDAVStorage represents a WebDAV storage
type WebDAVStorage struct {
	ctx    context.Context
	config *setting.WebDAVStorageConfig
	client *http.Client
}

// NewWebDAVStorage returns a WebDAV storage
func NewWebDAVStorage(ctx context.Context, cfg *setting.Storage) (ObjectStorage, error) {
	webdavCfg := &cfg.WebDAVConfig

	if _, err := url.Parse(webdavCfg.URL); err != nil {
		return nil, fmt.Errorf("invalid WebDAV URL: %w", err)
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
		ctx:    ctx,
		config: webdavCfg,
		client: &http.Client{
			Timeout: 0, // 禁止全局请求超时；body 传输时间不受限制，由 caller 的 context 控制
			Transport: &http.Transport{
				TLSClientConfig: tlsCfg,
				DialContext: (&net.Dialer{
					Timeout: connectTimeout,
				}).DialContext,
			},
		},
	}, nil
}

func (s *WebDAVStorage) getFullPath(p string) string {
	return strings.TrimSuffix(s.config.URL, "/") + "/" + p
}

func (s *WebDAVStorage) doRequest(method, p string, body io.Reader, contentLength int64) (*http.Response, error) {
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

	return s.client.Do(req)
}

// webdavObject wraps an io.ReadCloser to implement the Object interface
type webdavObject struct {
	*bytes.Reader
	closer io.Closer
}

// Close closes the reader
func (w *webdavObject) Close() error {
	return w.closer.Close()
}

// Stat returns file info for the object
func (w *webdavObject) Stat() (os.FileInfo, error) {
	return &webdavFileInfo{
		name:    "webdav-object",
		size:    int64(w.Reader.Len()),
		mode:    os.FileMode(0644),
		modTime: time.Now(),
		isDir:   false,
	}, nil
}

// Open opens a file for reading
func (s *WebDAVStorage) Open(p string) (Object, error) {
	resp, err := s.doRequest(http.MethodGet, p, nil, -1)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to open %s: %s", p, resp.Status)
	}

	// 流式读取，避免全量加载到内存
	buf := new(bytes.Buffer)
	if _, err := io.Copy(buf, resp.Body); err != nil {
		return nil, fmt.Errorf("failed to read body: %w", err)
	}

	reader := bytes.NewReader(buf.Bytes())
	return &webdavObject{
		Reader: reader,
		closer: io.NopCloser(reader),
	}, nil
}

// Save saves a file to WebDAV storage using streaming upload.
// contentLength is used for the HTTP Content-Length header.
// A negative contentLength will skip setting Content-Length (chunked transfer encoding).
// Large file uploads are supported: body transfer has no time limit,
// only connection establishment is subject to the configured timeout.
// Caller's context controls overall operation timeout.
func (s *WebDAVStorage) Save(p string, r io.Reader, size int64) (int64, error) {
	var contentLength int64 = -1
	if size >= 0 {
		contentLength = size
	}

	// 流式上传：io.Pipe 避免全量读入内存
	// goroutine 持有读端，caller 持有写端
	pr, pw := io.Pipe()

	respCh := make(chan *http.Response, 1)
	errCh := make(chan error, 1)

	go func() {
		resp, err := s.doRequest(http.MethodPut, p, pr, contentLength)
		if err != nil {
			errCh <- err
			return
		}
		// 确保 goroutine 退出前关闭 response body，无论是否读取
		respCh <- resp
	}()

	// 将 caller 的数据流接入 PipeWriter，HTTP goroutine 会自动消费
	written, copyErr := io.Copy(pw, r)
	pw.CloseWithError(copyErr) // 通知读取端数据已全部写入（或出错了）

	// 等待 HTTP goroutine 完成
	var resp *http.Response
	select {
	case err := <-errCh:
		return 0, fmt.Errorf("failed to upload %s: %w", p, err)
	case resp = <-respCh:
	}

	// goroutine 已结束，resp.Body 现在可以安全关闭
	if resp != nil {
		defer resp.Body.Close()
	}

	// 检查 HTTP 状态码
	if resp != nil && resp.StatusCode != http.StatusOK &&
		resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent {
		return 0, fmt.Errorf("failed to upload %s: %s", p, resp.Status)
	}

	if copyErr != nil && copyErr != io.EOF {
		return 0, fmt.Errorf("failed to read data to upload: %w", copyErr)
	}

	return written, nil
}

// Stat returns file info
func (s *WebDAVStorage) Stat(p string) (os.FileInfo, error) {
	resp, err := s.doRequest("PROPFIND", p, nil, -1)
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

	// 解析 WebDAV PROPFIND 响应，兼容不同服务端实现
	var result struct {
		XMLName  xml.Name `xml:"DAV:response"`
		HRef     string   `xml:"href"`
		PropStat []struct {
			Status string `xml:"status"`
			Prop   struct {
				GetLastModified  string `xml:"getlastmodified"`
				GetContentLength string `xml:"getcontentlength"`
				GetContentType   string `xml:"getcontenttype"`
				ResourceType     struct {
					Collection any `xml:"collection"` // 空标签表示是目录，非空表示是文件
				} `xml:"resourcetype"`
			} `xml:"propstat>prop"`
		} `xml:"propstat"`
	}

	if err := xml.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("failed to parse PROPFIND response: %w", err)
	}

	for _, ps := range result.PropStat {
		if ps.Status != "HTTP/1.1 200 OK" && ps.Status != "200 OK" {
			continue
		}

		var size int64
		if ps.Prop.GetContentLength != "" {
			fmt.Sscanf(ps.Prop.GetContentLength, "%d", &size)
		}

		var modTime time.Time
		// 尝试多种常见日期格式
		modified := ps.Prop.GetLastModified
		if modified != "" {
			for _, format := range []string{
				time.RFC1123,
				time.RFC1123Z,
				"Mon, 02 Jan 2006 15:04:05 MST",
				time.RFC3339,
			} {
				if modTime, err = time.Parse(format, modified); err == nil {
					break
				}
			}
		}

		// 通过 ResourceType.Collection 是否有值来判断是否为目录
		isDir := ps.Prop.ResourceType.Collection != nil

		return &webdavFileInfo{
			name:    path.Base(p),
			size:    size,
			mode:    os.FileMode(0644),
			modTime: modTime,
			isDir:   isDir,
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
	if prefix == "" {
		prefix = "."
	}

	// 使用 PROPFIND Depth:1 获取目录中的所有文件
	depth := "1"
	if prefix != "." {
		depth = "1"
	}

	fullPath := s.getFullPath(prefix)
	req, err := http.NewRequestWithContext(s.ctx, "PROPFIND", fullPath, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Depth", depth)

	if s.config.Username != "" {
		req.SetBasicAuth(s.config.Username, s.config.Password)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMultiStatus {
		return fmt.Errorf("PROPFIND failed: %s", resp.Status)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	// 解析多状态响应
	var multiResp struct {
		Responses []struct {
			HRef     string `xml:"href"`
			PropStat []struct {
				Status string `xml:"status"`
				Prop   struct {
					GetContentLength string `xml:"getcontentlength"`
					ResourceType     struct {
						Collection any `xml:"collection"`
					} `xml:"resourcetype"`
				} `xml:"propstat>prop"`
			} `xml:"propstat"`
		} `xml:"response"`
	}

	if err := xml.Unmarshal(body, &multiResp); err != nil {
		return fmt.Errorf("failed to parse PROPFIND response: %w", err)
	}

	basePrefix := strings.TrimPrefix(prefix, "./")

	for _, resp := range multiResp.Responses {
		// 找到成功的 propstat
		var isDir bool
		var size int64
		for _, ps := range resp.PropStat {
			if ps.Status == "HTTP/1.1 200 OK" || ps.Status == "200 OK" {
				isDir = ps.Prop.ResourceType.Collection != nil
				if ps.Prop.GetContentLength != "" {
					fmt.Sscanf(ps.Prop.GetContentLength, "%d", &size)
				}
				break
			}
		}

		// 跳过目录和自身
		if isDir {
			continue
		}

		// 提取相对路径
		objPath := resp.HRef
		// 移除 baseURL 前缀
		if strings.HasPrefix(objPath, s.config.URL) {
			objPath = strings.TrimPrefix(objPath, s.config.URL)
		}
		objPath = strings.TrimPrefix(objPath, "/")
		objPath = strings.TrimPrefix(objPath, basePrefix)
		objPath = strings.TrimPrefix(objPath, "/")

		if objPath == "" {
			continue
		}

		obj := &webdavObjectSimple{
			storage: s,
			path:    objPath,
			size:    size,
		}

		if err := fn(objPath, obj); err != nil {
			return err
		}
	}

	return nil
}

// webdavObjectSimple 是一个轻量级的 Object 实现，用于 IterateObjects
type webdavObjectSimple struct {
	storage *WebDAVStorage
	path    string
	size    int64
}

func (w *webdavObjectSimple) Read(p []byte) (int, error) {
	// 需要先 Open
	return 0, fmt.Errorf("must call Open first")
}

func (w *webdavObjectSimple) Seek(offset int64, whence int) (int64, error) {
	return 0, fmt.Errorf("must call Open first")
}

func (w *webdavObjectSimple) Close() error {
	return nil
}

func (w *webdavObjectSimple) Stat() (os.FileInfo, error) {
	return &webdavFileInfo{
		name:    path.Base(w.path),
		size:    w.size,
		mode:    os.FileMode(0644),
		modTime: time.Now(),
		isDir:   false,
	}, nil
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

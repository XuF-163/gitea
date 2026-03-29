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
	ctx      context.Context
	config   *setting.WebDAVStorageConfig
	endpoint *url.URL
	client   *http.Client
	basePath string
}

// NewWebDAVStorage returns a WebDAV storage
func NewWebDAVStorage(ctx context.Context, cfg *setting.Storage) (ObjectStorage, error) {
	webdavCfg := &cfg.WebDAVConfig

	endpoint, err := url.Parse(webdavCfg.URL)
	if err != nil {
		return nil, fmt.Errorf("invalid WebDAV URL: %w", err)
	}

	timeout := time.Duration(webdavCfg.Timeout) * time.Second
	if timeout == 0 {
		timeout = 30 * time.Second
	}

	return &WebDAVStorage{
		ctx:      ctx,
		config:   webdavCfg,
		endpoint: endpoint,
		client: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{},
			},
		},
		basePath: strings.TrimSuffix(endpoint.Path, "/"),
	}, nil
}

func (s *WebDAVStorage) getFullPath(p string) string {
	return s.endpoint.Scheme + "://" + s.endpoint.Host + path.Join(s.basePath, p)
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

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	reader := bytes.NewReader(data)
	return &webdavObject{
		Reader: reader,
		closer: io.NopCloser(reader),
	}, nil
}

// Save saves a file to WebDAV storage
func (s *WebDAVStorage) Save(p string, r io.Reader, size int64) (int64, error) {
	// Read all data into memory first (for size calculation)
	data, err := io.ReadAll(r)
	if err != nil {
		return 0, fmt.Errorf("failed to read data: %w", err)
	}

	resp, err := s.doRequest(http.MethodPut, p, bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return 0, fmt.Errorf("failed to upload %s: %w", p, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent {
		return 0, fmt.Errorf("failed to upload %s: %s", p, resp.Status)
	}

	return int64(len(data)), nil
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

	// Parse WebDAV PROPFIND response
	var result struct {
		XMLName  xml.Name `xml:"DAV:response"`
		HRef     string  `xml:"href"`
		PropStat []struct {
			Status string `xml:"status"`
			Prop   struct {
				GetLastModified  string `xml:"getlastmodified"`
				GetContentLength string `xml:"getcontentlength"`
				GetContentType   string `xml:"getcontenttype"`
				ResourceType     string `xml:"resourcetype"`
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
		if ps.Prop.GetLastModified != "" {
			modTime, _ = time.Parse(time.RFC1123, ps.Prop.GetLastModified)
		}

		isDir := ps.Prop.ResourceType == "<DAV:collection/>" || ps.Prop.ResourceType == "collection"

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

// IterateObjects iterates across objects in WebDAV storage (basic implementation)
func (s *WebDAVStorage) IterateObjects(_ string, _ func(string, Object) error) error {
	// WebDAV doesn't support directory listing in a simple way
	// This is a stub implementation that does nothing
	// For full support, PROPFIND with Depth: infinity would be needed
	return nil
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

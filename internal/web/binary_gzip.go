package web

import (
	"bytes"
	"compress/gzip"
	"os"
	"sync"
)

// selfGzipCache keeps one compressed copy of the hub binary. Install
// traffic is bursty and the file is ~15MB; compressing it on every
// request would make the first byte even later on an already slow tunnel.
type selfGzipCache struct {
	mu      sync.Mutex
	path    string
	size    int64
	modUnix int64
	payload []byte
}

var gzippedSelf selfGzipCache

func cachedSelfGzip(path string, info os.FileInfo) ([]byte, error) {
	mod := info.ModTime().UnixNano()
	gzippedSelf.mu.Lock()
	defer gzippedSelf.mu.Unlock()
	if gzippedSelf.payload != nil && gzippedSelf.path == path && gzippedSelf.size == info.Size() && gzippedSelf.modUnix == mod {
		return gzippedSelf.payload, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	writer, err := gzip.NewWriterLevel(&buf, gzip.DefaultCompression)
	if err != nil {
		return nil, err
	}
	if _, err := writer.Write(raw); err != nil {
		_ = writer.Close()
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	gzippedSelf.path = path
	gzippedSelf.size = info.Size()
	gzippedSelf.modUnix = mod
	gzippedSelf.payload = buf.Bytes()
	return gzippedSelf.payload, nil
}

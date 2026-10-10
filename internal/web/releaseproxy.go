package web

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// releaseBaseURL is the public distribution the hub proxies for platforms
// it cannot serve itself. Cross-platform agents upgrade through the same
// authenticated /dl/homer endpoint; the hub streams the matching GitHub
// Release archive so a machine never receives a binary it cannot execute
// (2026-10-10 Mac incident: a linux/amd64 hub binary reached a darwin/arm64
// agent and bricked it with an exec format error).
const releaseBaseURL = "https://github.com/zzjcool/homer-cli/releases/latest/download"

// releaseCacheTTL bounds how long a proxied release archive stays in memory.
const releaseCacheTTL = 10 * time.Minute

type releaseCacheEntry struct {
	body      []byte
	fetchedAt time.Time
}

// serveReleaseBinary answers /dl/homer[.gz]?goos=&goarch= for a platform
// different from the hub's own: it fetches homer_<goos>_<goarch>.tar.gz from
// GitHub Releases, extracts the homer binary, and streams it (optionally
// gzipped). The GitHub download is public, so the request itself stays behind
// the hub's auth gate; only the hub's outbound fetch is unauthenticated.
func (s *Server) serveReleaseBinary(w http.ResponseWriter, r *http.Request, goos, goarch string, compressed bool) {
	if !validReleasePlatform(goos, goarch) {
		writeError(w, http.StatusBadRequest, "bad-request",
			fmt.Sprintf("不支持的平台 %s/%s（hub 只能提供自身平台或 GitHub Releases 发布的平台）", goos, goarch), nil)
		return
	}
	platform := goos + "/" + goarch
	entry, ok := s.releaseCache.Load(platform)
	cached, fresh := releaseCacheEntry{}, false
	if ok {
		if cached, fresh = entry.(releaseCacheEntry); !fresh {
			cached = releaseCacheEntry{}
		}
		fresh = time.Since(cached.fetchedAt) < releaseCacheTTL
	}
	if !fresh {
		binary, err := fetchReleaseBinary(r.Context(), goos, goarch)
		if err != nil {
			writeError(w, http.StatusBadGateway, "release-fetch",
				"从 GitHub Releases 获取对应平台的 homer 失败: "+err.Error(), nil)
			return
		}
		cached = releaseCacheEntry{body: binary, fetchedAt: time.Now()}
		s.releaseCache.Store(platform, cached)
	}

	// The platform marker lets the agent refuse a mismatched replacement
	// even if some intermediary rewrites the routing.
	w.Header().Set("X-Homer-Platform", platform)
	w.Header().Set("Cache-Control", "no-store")
	if compressed {
		var buf bytes.Buffer
		gzWriter := gzip.NewWriter(&buf)
		if _, err := gzWriter.Write(cached.body); err != nil {
			_ = gzWriter.Close()
			writeError(w, http.StatusInternalServerError, "internal", "压缩 homer 二进制失败: "+err.Error(), nil)
			return
		}
		if err := gzWriter.Close(); err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "压缩 homer 二进制失败: "+err.Error(), nil)
			return
		}
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Content-Length", fmt.Sprint(buf.Len()))
		_, _ = w.Write(buf.Bytes())
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", fmt.Sprint(len(cached.body)))
	_, _ = w.Write(cached.body)
}

// validReleasePlatform matches the platforms goreleaser builds (see
// .goreleaser.yaml: linux+darwin, amd64+arm64).
func validReleasePlatform(goos, goarch string) bool {
	switch goos {
	case "linux", "darwin":
	default:
		return false
	}
	switch goarch {
	case "amd64", "arm64":
	default:
		return false
	}
	return true
}

// fetchReleaseBinary downloads the archive for the platform and returns the
// extracted homer binary. It enforces a size cap so a hostile or broken
// upstream cannot exhaust hub memory.
func fetchReleaseBinary(ctx context.Context, goos, goarch string) ([]byte, error) {
	archiveURL := fmt.Sprintf("%s/homer_%s_%s.tar.gz", releaseBaseURL, goos, goarch)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, archiveURL, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 2 * time.Minute}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 1024))
		return nil, fmt.Errorf("HTTP %d %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	// The archive is ~5MB; 64MB is a generous ceiling against abuse.
	gzReader, err := gzip.NewReader(io.LimitReader(response.Body, 64<<20))
	if err != nil {
		return nil, fmt.Errorf("归档不是有效的 gzip: %w", err)
	}
	tarReader := tar.NewReader(gzReader)
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("归档里没有 homer 二进制")
		}
		if err != nil {
			return nil, err
		}
		if header.Typeflag != tar.TypeReg || header.Name != "homer" {
			continue
		}
		if header.Size > 64<<20 {
			return nil, fmt.Errorf("归档里的 homer 超过 64MB")
		}
		return io.ReadAll(io.LimitReader(tarReader, 64<<20))
	}
}

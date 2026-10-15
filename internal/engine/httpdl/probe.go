// Package httpdl 实现 HTTP/HTTPS 多段并发下载。
//
// 设计要点：
//   - 先探测（HEAD，必要时回退到 GET + Range: bytes=0-0）确定总大小与是否支持 Range；
//   - 支持 Range 时按分片并发下载，每个 worker 独占一个分片连接；
//   - 分片内部以「分块」为粒度标记进度，任何时刻中断都能从第一个缺口续传；
//   - 不支持 Range 或大小未知时自动降级为单流顺序下载，而不是报错退出。
package httpdl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
)

// ProbeResult 是对下载源的探测结果。
type ProbeResult struct {
	// URL 是探测时实际使用的地址（可能经过重定向后的最终地址）。
	URL string
	// TotalSize 是内容总长度；Content-Length 缺失时为 engine.SizeUnknown 的取值语义（-1）。
	TotalSize int64
	// AcceptRanges 表示服务端确认支持 Range 请求。
	AcceptRanges bool
	// Filename 是从 Content-Disposition 或 URL 推断出的建议文件名。
	Filename string
	// ETag / LastModified 用于判断远端内容是否变化（变化则已有断点作废）。
	ETag         string
	LastModified string
	// ContentType 是响应声明的媒体类型。
	ContentType string
}

// ErrNoContentLength 表示服务端既未提供 Content-Length 也未提供可解析的 Content-Range。
var ErrNoContentLength = errors.New("httpdl: 服务端未提供内容长度")

// Probe 探测下载源。它会依次尝试 HEAD 与 GET + Range，以尽量准确地判断
// 总大小与是否支持分段下载。
func Probe(ctx context.Context, client *http.Client, rawURL string, header http.Header) (*ProbeResult, error) {
	if client == nil {
		client = http.DefaultClient
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("httpdl: 非法的下载地址 %q: %w", rawURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("httpdl: 不支持的协议 %q", u.Scheme)
	}

	// 第一轮：HEAD。部分服务器不支持 HEAD，失败则直接进入第二轮。
	if res, err := head(ctx, client, rawURL, header); err == nil {
		probe := probeFromResponse(rawURL, res)
		_ = res.Body.Close()
		if probe.TotalSize > 0 {
			// HEAD 声明了长度：再用一次 1 字节 Range 确认分段能力。
			if rangesOK, size, err := probeRange(ctx, client, rawURL, header); err == nil {
				probe.AcceptRanges = rangesOK
				if size > 0 {
					probe.TotalSize = size
				}
				return probe, nil
			}
			return probe, nil
		}
	}

	// 第二轮：GET + Range: bytes=0-0，既拿长度也验证分段能力。
	rangesOK, size, err := probeRange(ctx, client, rawURL, header)
	if err != nil {
		// 第三轮：既不能 HEAD 也不接受 Range，退化为一次普通 GET 探测。
		res, getErr := getHead(ctx, client, rawURL, header)
		if getErr != nil {
			return nil, fmt.Errorf("httpdl: 探测失败: %w", getErr)
		}
		defer res.Body.Close()
		probe := probeFromResponse(rawURL, res)
		probe.AcceptRanges = false
		if probe.TotalSize <= 0 {
			// 无法得知长度：交由调用方走单流模式，这里不视为致命错误。
			probe.TotalSize = -1
		}
		return probe, nil
	}

	probe := &ProbeResult{URL: rawURL, AcceptRanges: rangesOK, TotalSize: size}
	if u2, err := url.Parse(rawURL); err == nil {
		probe.Filename = filenameFromURL(u2)
	}
	if !rangesOK {
		// 服务端忽略 Range 并返回 200：长度可能仍然可从 Content-Length 得到。
		res, err := head(ctx, client, rawURL, header)
		if err == nil {
			defer res.Body.Close()
			fromHead := probeFromResponse(rawURL, res)
			if fromHead.TotalSize > 0 {
				probe.TotalSize = fromHead.TotalSize
			}
			if probe.Filename == "" {
				probe.Filename = fromHead.Filename
			}
			probe.ETag = fromHead.ETag
			probe.LastModified = fromHead.LastModified
			probe.ContentType = fromHead.ContentType
		}
	}
	if probe.TotalSize <= 0 {
		probe.TotalSize = -1
	}
	return probe, nil
}

func head(ctx context.Context, client *http.Client, rawURL string, header http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, rawURL, nil)
	if err != nil {
		return nil, err
	}
	applyHeader(req, header)
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode == http.StatusMethodNotAllowed || res.StatusCode == http.StatusNotImplemented {
		_ = res.Body.Close()
		return nil, fmt.Errorf("httpdl: 服务端不支持 HEAD（%d）", res.StatusCode)
	}
	if res.StatusCode >= 400 {
		_ = res.Body.Close()
		return nil, fmt.Errorf("httpdl: HEAD 返回状态码 %d", res.StatusCode)
	}
	return res, nil
}

// getHead 发起一次不带 Range 的 GET，仅用于读取响应头，随即关闭响应体。
func getHead(ctx context.Context, client *http.Client, rawURL string, header http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	applyHeader(req, header)
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode >= 400 {
		_ = res.Body.Close()
		return nil, fmt.Errorf("httpdl: GET 返回状态码 %d", res.StatusCode)
	}
	return res, nil
}

// probeRange 用 bytes=0-0 请求判断是否支持 Range，并尽力解析出总长度。
func probeRange(ctx context.Context, client *http.Client, rawURL string, header http.Header) (acceptRanges bool, totalSize int64, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return false, -1, err
	}
	applyHeader(req, header)
	req.Header.Set("Range", "bytes=0-0")

	res, err := client.Do(req)
	if err != nil {
		return false, -1, err
	}
	defer func() {
		// 只需要响应头，丢弃 1 字节内容并复用连接。
		_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 4096))
		_ = res.Body.Close()
	}()

	switch res.StatusCode {
	case http.StatusPartialContent:
		size := parseContentRangeTotal(res.Header.Get("Content-Range"))
		if size <= 0 {
			size = parseContentLength(res.Header.Get("Content-Length"))
		}
		return true, size, nil
	case http.StatusOK:
		// 服务端忽略了 Range：不支持分段，但 Content-Length 仍可用。
		return false, parseContentLength(res.Header.Get("Content-Length")), nil
	default:
		return false, -1, fmt.Errorf("httpdl: Range 探测返回状态码 %d", res.StatusCode)
	}
}

func probeFromResponse(rawURL string, res *http.Response) *ProbeResult {
	probe := &ProbeResult{
		URL:          rawURL,
		TotalSize:    parseContentLength(res.Header.Get("Content-Length")),
		AcceptRanges: strings.EqualFold(strings.TrimSpace(res.Header.Get("Accept-Ranges")), "bytes"),
		ETag:         res.Header.Get("ETag"),
		LastModified: res.Header.Get("Last-Modified"),
		ContentType:  res.Header.Get("Content-Type"),
	}
	if name := filenameFromDisposition(res.Header.Get("Content-Disposition")); name != "" {
		probe.Filename = name
	} else if u, err := url.Parse(rawURL); err == nil {
		probe.Filename = filenameFromURL(u)
	}
	return probe
}

func applyHeader(req *http.Request, header http.Header) {
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", "p2pdl/1.0")
	}
}

// parseContentLength 解析 Content-Length，非法或缺失时返回 -1。
func parseContentLength(v string) int64 {
	v = strings.TrimSpace(v)
	if v == "" {
		return -1
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return -1
	}
	return n
}

// parseContentRangeTotal 从 "bytes 0-0/12345" 中解析出总长度；"*" 表示未知。
func parseContentRangeTotal(v string) int64 {
	_, after, ok := strings.Cut(v, "/")
	if !ok {
		return -1
	}
	after = strings.TrimSpace(after)
	if after == "*" {
		return -1
	}
	n, err := strconv.ParseInt(after, 10, 64)
	if err != nil || n < 0 {
		return -1
	}
	return n
}

func filenameFromDisposition(v string) string {
	if v == "" {
		return ""
	}
	_, params, err := mime.ParseMediaType(v)
	if err != nil {
		return ""
	}
	return sanitizeFilename(params["filename"])
}

func filenameFromURL(u *url.URL) string {
	base := path.Base(u.Path)
	if base == "." || base == "/" || base == "" {
		return ""
	}
	return sanitizeFilename(base)
}

// sanitizeFilename 去掉路径分隔符与控制字符，避免服务端提供的名字影响本地路径。
func sanitizeFilename(name string) string {
	name = strings.TrimSpace(name)
	name = strings.Trim(name, `"'`)
	if name == "" {
		return ""
	}
	name = strings.Map(func(r rune) rune {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|', 0:
			return '_'
		}
		if r < 32 {
			return -1
		}
		return r
	}, name)
	name = strings.Trim(name, ". ")
	if name == "" || name == "." || name == ".." {
		return ""
	}
	return name
}

package limiter

import "sync"

// DefaultBufferSize 是默认的读取缓冲区大小（32KB）。
//
// 取值权衡：更小的缓冲意味着更细的限速粒度与更低的单连接内存占用；
// 更大的缓冲意味着更少的系统调用与更高的吞吐。32KB 在两者间取平衡，
// 并且与磁盘/网络栈的常见块大小对齐。
const DefaultBufferSize = 32 << 10

// BufferPool 复用固定大小的缓冲区，避免大文件下载时的高频分配。
//
// 大文件下载会在数小时内完成数十万次读取，若不复用缓冲区，仅 GC 压力就会
// 让内存占用出现锯齿状峰值（每个 32KB 缓冲在逃逸到 goroutine 后不再是小对象）。
type BufferPool struct {
	size int
	pool sync.Pool
}

// NewBufferPool 创建指定块大小的缓冲池。size <= 0 时使用 DefaultBufferSize。
func NewBufferPool(size int) *BufferPool {
	if size <= 0 {
		size = DefaultBufferSize
	}
	p := &BufferPool{size: size}
	p.pool.New = func() any {
		b := make([]byte, size)
		return &b
	}
	return p
}

// Get 取出一个长度为 Size() 的缓冲区。使用完必须 Put 归还。
func (p *BufferPool) Get() []byte {
	ptr := p.pool.Get().(*[]byte)
	return (*ptr)[:p.size]
}

// Put 归还缓冲区。长度不匹配的切片会被忽略（防御误用外部切片）。
func (p *BufferPool) Put(b []byte) {
	if cap(b) < p.size {
		return
	}
	b = b[:p.size]
	p.pool.Put(&b)
}

// Size 返回池中缓冲区的大小。
func (p *BufferPool) Size() int { return p.size }

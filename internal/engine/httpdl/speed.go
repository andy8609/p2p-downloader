package httpdl

import (
	"sync"
	"time"
)

// sampleInterval 是速度采样的最小间隔，避免样本数量随数据块数量爆炸。
const sampleInterval = 200 * time.Millisecond

type speedSample struct {
	at    time.Time
	total int64
}

// speedMeter 用滑动窗口估算瞬时速度（字节/秒）。
//
// 相比「累计字节 / 总耗时」，滑动窗口能反映当前速度：连接数变化、被限速、
// peer 减少等情况都能及时体现，这对 GUI 的速度曲线与限速验证都是必要的。
type speedMeter struct {
	mu      sync.Mutex
	window  time.Duration
	samples []speedSample
	total   int64
}

func newSpeedMeter(window time.Duration) *speedMeter {
	if window <= 0 {
		window = 5 * time.Second
	}
	return &speedMeter{window: window, samples: make([]speedSample, 0, 64)}
}

// add 记录新增字节数。
func (m *speedMeter) add(n int64) {
	if n <= 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	m.total += n
	now := time.Now()
	if len(m.samples) > 0 {
		last := m.samples[len(m.samples)-1]
		if now.Sub(last.at) < sampleInterval {
			// 同一采样窗口内：累加到最新样本上，不新增样本。
			m.samples[len(m.samples)-1].total = m.total
			return
		}
	}
	m.samples = append(m.samples, speedSample{at: now, total: m.total})
	m.trimLocked(now)
}

// trimLocked 丢弃窗口外的样本，但保留一个窗口外的基准样本，
// 使窗口边界处的速率计算仍然连续。
func (m *speedMeter) trimLocked(now time.Time) {
	cutoff := now.Add(-m.window)
	firstKeep := 0
	for i, s := range m.samples {
		if s.at.Before(cutoff) {
			firstKeep = i
			continue
		}
		break
	}
	if firstKeep > 0 {
		m.samples = append(m.samples[:0], m.samples[firstKeep:]...)
	}
}

// rate 返回滑动窗口内的平均速度（字节/秒）。样本不足时返回 0。
func (m *speedMeter) rate() float64 {
	m.mu.Lock()
	defer m.mu.Unlock()

	if len(m.samples) < 2 {
		return 0
	}
	first := m.samples[0]
	last := m.samples[len(m.samples)-1]
	elapsed := last.at.Sub(first.at).Seconds()
	if elapsed <= 0 {
		return 0
	}
	delta := last.total - first.total
	if delta <= 0 {
		return 0
	}
	return float64(delta) / elapsed
}

// totalBytes 返回累计记录的字节数。
func (m *speedMeter) totalBytes() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.total
}

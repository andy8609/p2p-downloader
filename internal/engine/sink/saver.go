package sink

import (
	"errors"

	"p2pdl/internal/engine"
)

// Store 是保存任务元数据的最小接口，由 store.FileStore 直接满足。
type Store interface {
	Save(t *engine.Task) error
}

// Syncer 是需要在写元数据之前先落盘的数据目标，由 *FileSink 满足。
type Syncer interface {
	Sync() error
}

// ProgressSaver 实现 httpdl.ProgressSaver，并在保存元数据前先 fsync 数据文件。
//
// 顺序至关重要：
//
//	数据 fsync（Sync）→ 元数据原子落盘（Store.Save）
//
// 反过来（先写元数据后落数据）会在断电时留下「位图声称完成、磁盘上却是空洞」的
// 坏数据；本实现保证这种状态不可能出现。
type ProgressSaver struct {
	Store Store
	Sink  Syncer
}

// Save 先落盘数据，再原子保存元数据。
func (p *ProgressSaver) Save(t *engine.Task) error {
	if p.Store == nil {
		return errors.New("sink: 元数据存储未配置")
	}
	if p.Sink != nil {
		if err := p.Sink.Sync(); err != nil {
			return err
		}
	}
	return p.Store.Save(t)
}

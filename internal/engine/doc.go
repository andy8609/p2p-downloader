// Package engine 是下载引擎的统一抽象层。
//
// 分层约定（详见 docs/architecture.md）：
//
//	engine/            统一任务模型、状态机、调度器与 Downloader 接口（T8）
//	engine/httpdl/     HTTP/HTTPS 多段并发下载实现（T4、T5）
//	engine/bittorrent/ BitTorrent / 磁力链下载实现（T7）
//	engine/sink/       分段落盘、预分配与断点恢复（T5）
//	engine/limiter/    全局与单任务限速、缓冲区池（T6）
//	engine/verify/     完整性校验与坏段重下（T9）
//
// 本包只依赖标准库与 internal 下的同级包，不依赖 GUI 层，
// 以保证引擎可被 CLI 独立驱动与测试。
package engine

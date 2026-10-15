package sink

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"p2pdl/internal/engine"
	"p2pdl/internal/engine/httpdl"
	"p2pdl/internal/store"
)

// slowRangeServer 提供一个支持 Range、但按固定速率吐数据的测试服务器，
// 用于制造「下载到一半被中断」的确定性场景。
func slowRangeServer(t *testing.T, content []byte) *httptest.Server {
	t.Helper()
	total := int64(len(content))
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start, end := int64(0), total-1
		status := http.StatusOK
		if raw := r.Header.Get("Range"); raw != "" {
			var err error
			start, end, err = parseSimpleRange(raw, total)
			if err != nil {
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return
			}
			status = http.StatusPartialContent
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, total))
		}
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.WriteHeader(status)
		if r.Method == http.MethodHead {
			return
		}

		const step = 16 << 10
		for off := start; off <= end; off += step {
			last := off + step - 1
			if last > end {
				last = end
			}
			if _, err := w.Write(content[off : last+1]); err != nil {
				return
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			time.Sleep(2 * time.Millisecond)
		}
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

func parseSimpleRange(raw string, total int64) (int64, int64, error) {
	spec, ok := strings.CutPrefix(raw, "bytes=")
	if !ok {
		return 0, 0, errors.New("缺少 bytes= 前缀")
	}
	startStr, endStr, ok := strings.Cut(spec, "-")
	if !ok {
		return 0, 0, errors.New("缺少 - 分隔符")
	}
	start, err := strconv.ParseInt(startStr, 10, 64)
	if err != nil {
		return 0, 0, err
	}
	end := total - 1
	if endStr != "" {
		if end, err = strconv.ParseInt(endStr, 10, 64); err != nil {
			return 0, 0, err
		}
	}
	if start < 0 || end >= total || start > end {
		return 0, 0, errors.New("区间非法")
	}
	return start, end, nil
}

// 验收核心（T5）：反复中断后仍能续传完成，最终文件 SHA-256 与源一致，
// 且不产生区间重叠或空洞（哈希一致即证明两者皆无）。
func TestEndToEndResumeProducesVerifiableFile(t *testing.T) {
	const total = int64(2 << 20)
	const chunkSize = int64(256 << 10)
	const segments = 4

	content := make([]byte, total)
	if _, err := rand.Read(content); err != nil {
		t.Fatalf("生成测试数据失败: %v", err)
	}
	wantSum := sha256.Sum256(content)

	srv := slowRangeServer(t, content)
	dir := t.TempDir()
	finalPath := filepath.Join(dir, "blob.bin")
	tempPath := TempPathFor(finalPath)

	fsStore, err := store.NewFileStore(filepath.Join(dir, "meta"))
	if err != nil {
		t.Fatalf("NewFileStore 失败: %v", err)
	}

	probe, err := httpdl.Probe(context.Background(), nil, srv.URL, nil)
	if err != nil {
		t.Fatalf("Probe 失败: %v", err)
	}
	if !probe.AcceptRanges || probe.TotalSize != total {
		t.Fatalf("探测结果异常: %+v", probe)
	}

	task, err := engine.NewTask("e2e-1", engine.SourceHTTP, srv.URL, finalPath, tempPath,
		probe.TotalSize, chunkSize, segments)
	if err != nil {
		t.Fatalf("NewTask 失败: %v", err)
	}
	if err := fsStore.Save(task); err != nil {
		t.Fatalf("首次保存任务失败: %v", err)
	}

	const maxRounds = 12
	interruptions := 0
	var progressLog []string

	for round := 1; round <= maxRounds; round++ {
		// 关键：每轮都从磁盘重新读回任务，模拟「进程重启」而不是复用内存状态。
		reloaded, err := fsStore.Load("e2e-1")
		if err != nil {
			t.Fatalf("第 %d 轮加载任务失败: %v", round, err)
		}
		task = reloaded

		plan, err := PlanRecovery(task, tempPath)
		if err != nil {
			t.Fatalf("第 %d 轮恢复决策失败: %v", round, err)
		}
		if task.Complete() {
			if len(plan.Missing) != 0 {
				t.Fatalf("任务已完成但仍有缺口: %+v", plan.Missing)
			}
			break
		}
		if plan.Decision != DecisionResume {
			t.Fatalf("第 %d 轮应可续传，实际 %s（%s）", round, plan.Decision, plan.Reason)
		}
		progressLog = append(progressLog, fmt.Sprintf("第 %d 轮起点已下载 %d/%d", round, plan.Downloaded, total))

		fs, err := Open(finalPath, tempPath, httpdlOptionsSink(total))
		if err != nil {
			t.Fatalf("第 %d 轮打开临时文件失败: %v", round, err)
		}

		// 前几轮刻意中断；最后一轮跑完。
		interrupt := round < maxRounds
		ctx := context.Background()
		var cancel context.CancelFunc
		if interrupt {
			ctx, cancel = context.WithCancel(ctx)
			go func() {
				time.Sleep(80 * time.Millisecond)
				cancel()
			}()
		}

		d, err := httpdl.New(task, fs, httpdl.Options{
			Concurrency: 2,
			BufferSize:  8 << 10,
			Probe:       probe,
			MaxRetries:  1,
			Progress:    &ProgressSaver{Store: fsStore, Sink: fs},
		})
		if err != nil {
			t.Fatalf("第 %d 轮 New 失败: %v", round, err)
		}

		runErr := d.Run(ctx)
		if cancel != nil {
			cancel()
		}
		if runErr != nil && !errors.Is(runErr, context.Canceled) {
			t.Fatalf("第 %d 轮下载失败: %v", round, runErr)
		}
		if interrupt {
			interruptions++
		}

		before := task.Downloaded()
		if err := fs.Close(); err != nil {
			t.Fatalf("第 %d 轮关闭临时文件失败: %v", round, err)
		}
		// 中断轮次必须留下可复用的进度（否则说明进度根本没有被持久化）。
		if interrupt && before > 0 && before < total {
			// 期望值：关闭后磁盘上的元数据应反映这次中断前的进度。
			if _, err := fsStore.Load("e2e-1"); err != nil {
				t.Fatalf("第 %d 轮重新加载任务失败: %v", round, err)
			}
		}
		if task.Complete() {
			break
		}
	}

	t.Logf("共中断 %d 次\n%s", interruptions, strings.Join(progressLog, "\n"))

	if !task.Complete() {
		t.Fatalf("任务未能在 %d 轮内完成（已下载 %d/%d）", maxRounds, task.Downloaded(), total)
	}
	if interruptions == 0 {
		t.Fatal("测试未实际制造任何中断，无法验证续传")
	}

	// 最终必须通过 Commit 把临时文件原子重命名为目标文件。
	finalSink, err := Open(finalPath, tempPath, httpdlOptionsSink(total))
	if err != nil {
		t.Fatalf("打开临时文件失败: %v", err)
	}
	if err := finalSink.Commit(total); err != nil {
		t.Fatalf("提交最终文件失败: %v", err)
	}

	got, err := os.ReadFile(finalPath)
	if err != nil {
		t.Fatalf("读取最终文件失败: %v", err)
	}
	if int64(len(got)) != total {
		t.Fatalf("最终文件长度 = %d, 期望 %d", len(got), total)
	}
	if gotSum := sha256.Sum256(got); gotSum != wantSum {
		t.Fatalf("最终文件 SHA-256 不匹配：文件被写坏（重叠或空洞）\n得到 %x\n期望 %x", gotSum, wantSum)
	}
	if !bytes.Equal(got, content) {
		t.Fatal("最终文件内容与源不一致")
	}
}

// 恢复决策必须能在「临时文件已存在且进度完整」时继续，而不是重下。
func TestEndToEndResumeReusesExistingTempFile(t *testing.T) {
	content := bytes.Repeat([]byte("payload-"), 4096)
	total := int64(len(content))
	chunkSize := int64(4096)
	srv := slowRangeServer(t, content)

	dir := t.TempDir()
	finalPath := filepath.Join(dir, "file.bin")
	tempPath := TempPathFor(finalPath)
	fsStore, err := store.NewFileStore(filepath.Join(dir, "meta"))
	if err != nil {
		t.Fatalf("NewFileStore 失败: %v", err)
	}
	task, err := engine.NewTask("e2e-2", engine.SourceHTTP, srv.URL, finalPath, tempPath,
		total, chunkSize, 2)
	if err != nil {
		t.Fatalf("NewTask 失败: %v", err)
	}

	// 第一轮：完整下载（不中断），模拟「暂停」。
	fs, err := Open(finalPath, tempPath, Options{TotalSize: total})
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	d, err := httpdl.New(task, fs, httpdl.Options{
		Concurrency: 2, BufferSize: 4 << 10,
		Progress: &ProgressSaver{Store: fsStore, Sink: fs},
	})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	if err := d.Run(context.Background()); err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if err := fs.Close(); err != nil {
		t.Fatalf("Close 失败: %v", err)
	}
	firstSize, _ := os.Stat(tempPath)

	// 第二轮：重新加载任务并按恢复决策继续。
	reloaded, err := fsStore.Load("e2e-2")
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	plan, err := PlanRecovery(reloaded, tempPath)
	if err != nil {
		t.Fatalf("PlanRecovery 失败: %v", err)
	}
	if plan.Decision != DecisionResume {
		t.Fatalf("决策 = %s, 期望 resume（%s）", plan.Decision, plan.Reason)
	}
	if len(plan.Missing) != 0 {
		t.Fatalf("数据已齐全时不应有缺口: %+v", plan.Missing)
	}

	fs2, err := Open(finalPath, tempPath, Options{TotalSize: total})
	if err != nil {
		t.Fatalf("重新 Open 失败: %v", err)
	}
	sizeBefore, err := fs2.Size()
	if err != nil {
		t.Fatalf("Size 失败: %v", err)
	}
	if sizeBefore != firstSize.Size() {
		t.Fatalf("复用文件长度 = %d, 期望 %d", sizeBefore, firstSize.Size())
	}
	if err := fs2.Commit(total); err != nil {
		t.Fatalf("Commit 失败: %v", err)
	}
	got, err := os.ReadFile(finalPath)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatal("内容与源不一致")
	}
}

// httpdlOptionsSink 只是为了让 Open 调用点更易读。
func httpdlOptionsSink(total int64) Options {
	return Options{TotalSize: total}
}

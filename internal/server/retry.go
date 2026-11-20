package server

import (
	"time"

	"github.com/rsfree/aivideomaker2api/internal/store"
	"github.com/rsfree/aivideomaker2api/internal/translate"
)

// scheduleRuntimeRetry 在"上游接了单、但跑完回 failed"之后自动重提一次。
//
// 三个刻意的约束：
//
//  1. **只重提一次**（`retry.runtime_retries`，默认 1）。这类失败站点只回
//     `status=failed` 不给原因，其中混着运行时内容审核 —— 那种重试必然再失败，
//     还会多烧一次额度。一次是"救抖动"，两次以上就是"放大失败"。
//  2. **官方通路绝不自动重提**。官方每单都扣积分，自动重提等于替调用方再花一次钱。
//  3. **复用同一条记录**，而不是新建。调用方手里只有原来的 id，新建一条等于
//     "结果永远看不到"。代价是记录会短暂从 failed 退回 queued，所以 Retries /
//     RetryOf 必须留痕：一次调用可能对应两条上游任务，不留痕就没法解释账单。
//
// 重提用的参数来自 `WebParams`（首次提交时落盘的那份**实际发出去**的参数），
// 素材已转存到站点 CDN，不必再传一遍。
func (s *Server) scheduleRuntimeRetry(id string) {
	if s.cfg.RuntimeRetries <= 0 {
		return
	}
	var scheduled bool
	var oldTask string
	_, _ = s.store.Patch(id, func(e *store.Entry) {
		// 条件全部在**store 的锁内**判断：轮询与 reaper 会并发走到这里，
		// 放到锁外就能把同一条任务重提两次（那是真事故，不是抖动）。
		if e.Status != "failed" || e.TaskID == "" || e.Retries >= s.cfg.RuntimeRetries {
			return
		}
		// 提交期的失败有自己的换号重试路径（submit），不走这里。
		if e.SubmitErrorCode != "" {
			return
		}
		// 官方通路：每单扣钱，不自动重提。
		if e.Upstream == "official" {
			return
		}
		// 没有可复用的参数就别猜 —— 猜出来的请求比失败更糟。
		if len(e.WebParams) == 0 {
			return
		}
		oldTask = e.TaskID
		e.Retries++
		e.RetryOf = oldTask
		e.TaskID = ""
		e.Status = "queued"
		e.SubmittedAtMs = 0
		e.SourceURL = ""
		e.VideoURL = ""
		e.Usage = nil
		e.SubmitError = ""
		e.SubmitErrorCode = ""
		e.SubmitErrorHint = ""
		e.SubmitErrorQuote = ""
		scheduled = true
	})
	if !scheduled {
		return
	}
	s.cache.drop(id)
	s.log.Warn("runtime failure → scheduling one retry",
		"task", id, "upstream_task", oldTask, "delay", s.cfg.RuntimeBackoff.String())

	go func() {
		timer := time.NewTimer(s.cfg.RuntimeBackoff)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-s.stopCh:
			// 进程要退了：把记录还原成 failed，别留一条永远 queued 的幽灵。
			_, _ = s.store.Patch(id, func(e *store.Entry) {
				if e.Status == "queued" && e.TaskID == "" {
					e.Status = "failed"
					e.SubmitErrorCode = codeSubmitFailed
					e.SubmitError = "retry aborted: the service stopped before the retry was submitted"
				}
			})
			s.cache.drop(id)
			return
		}

		entry, ok := s.store.Get(id)
		// 期间被改过（人工重转、官方任务）就别插手。
		if !ok || entry.Status != "queued" || entry.TaskID != "" || len(entry.WebParams) == 0 {
			return
		}
		// 只重建"实际发出去的参数"，不做翻译：`Requested` 缺图生视频的 imageUrl 等字段，
		// 用它重翻会把图生视频变成文生视频（静默改变了用户的请求）。
		s.submitAsync(entry, &translate.Plan{WebParams: entry.WebParams})
	}()
}

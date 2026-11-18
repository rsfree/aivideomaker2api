package upstream

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// slotCeiling 是 tokens channel 的**物理**容量。
//
// 逻辑额度（opts.Capacity）可以随探测结果上调，而 channel 的容量创建后不能变。
// 把它开到一个远大于站点真实额度的值（站点最大是每号 4），上调就只是"往一个还有空位
// 的 channel 里塞许可"，不需要替换 channel —— 替换会在有人在旧 channel 上等待时把他们
// 永久挂住（这才是真会发生的 bug）。
const slotCeiling = 64

// ErrNoSlot 表示在等待预算内没有拿到上游槽位（对外 429 TaskQueueFull）。
var ErrNoSlot = errors.New("no upstream slot freed within the wait budget")

// QueueOptions 是闸门参数。
type QueueOptions struct {
	// Capacity 是同时在上游跑的任务数上限（= 账号的 maxQueueLength）。
	Capacity int
	// AcquireTimeout 是等空槽的总预算（默认 600s）。
	AcquireTimeout time.Duration
	// PollInterval 是盯梢轮询间隔（默认 10s）。
	PollInterval time.Duration
	// WatchTimeout 是盯到终态的总时限（默认 3600s）。
	WatchTimeout time.Duration
	// Logf 是可选的日志钩子。
	Logf func(format string, args ...any)
}

// Stats 是闸门快照（面板与 /healthz 用）。
type Stats struct {
	Capacity      int      `json:"capacity"`
	InUse         int      `json:"in_use"`
	RunningTasks  []string `json:"running_tasks"`
	ServedTotal   int64    `json:"served_total"`
	DelayedTotal  int64    `json:"delayed_total"`
	RejectedTotal int64    `json:"rejected_total"`
	WaitedMS      int64    `json:"last_waited_ms"`
}

// Queue 是**每账号一把**的提交闸门。
//
// 关键认知：**槽位从创建一直占到任务进入终态**，不是"创建请求返回就释放"。
// 所以直连转发在第 3 个并发请求就会开始报错（上游原话："The queue is full. The premium
// plan can only run 2 task at a time."）。
//
// 实现用一个**可用许可**的缓冲 channel：Acquire 取一个许可，Release 还一个。
// 阻塞等待就是 select，超时就是 timer —— 不需要额外的信号量库。
type Queue struct {
	tokens  chan struct{}
	opts    QueueOptions
	client  *Client
	held    atomic.Int32
	mu      sync.Mutex
	running map[string]time.Time
	waiting int

	served   int64
	delayed  int64
	rejected int64
	waitedMS int64
}

// NewQueue 构造闸门。client 为 nil 时 Submit/Watch 不可用（纯闸门测试用）。
func NewQueue(client *Client, opts QueueOptions) *Queue {
	if opts.Capacity <= 0 {
		opts.Capacity = 2
	}
	if opts.AcquireTimeout <= 0 {
		opts.AcquireTimeout = 600 * time.Second
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = 10 * time.Second
	}
	if opts.WatchTimeout <= 0 {
		opts.WatchTimeout = 3600 * time.Second
	}
	q := &Queue{
		tokens:  make(chan struct{}, slotCeiling),
		opts:    opts,
		client:  client,
		running: map[string]time.Time{},
	}
	for i := 0; i < opts.Capacity; i++ {
		q.tokens <- struct{}{}
	}
	return q
}

// Capacity 是当前额度。
func (q *Queue) Capacity() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.opts.Capacity
}

// InUse 是已占用的槽位数。
func (q *Queue) InUse() int { return int(q.held.Load()) }

// TryAcquire 非阻塞地占一个槽位。选号时用它判断"这个号现在有免费额度"。
func (q *Queue) TryAcquire() bool {
	select {
	case <-q.tokens:
		q.held.Add(1)
		return true
	default:
		return false
	}
}

// Acquire 阻塞等待一个槽位，直到 ctx 到期或 acquireTimeout。
//
// 直接给队列传 nil ctx 是不允许的；调用方一般用 context.Background() 加自己的预算。
func (q *Queue) Acquire(ctx context.Context) error {
	if q.TryAcquire() {
		q.noteServed(0)
		return nil
	}
	start := time.Now()
	q.mu.Lock()
	q.waiting++
	q.mu.Unlock()
	defer func() {
		q.mu.Lock()
		q.waiting--
		q.mu.Unlock()
	}()

	budget := q.opts.AcquireTimeout
	if budget <= 0 {
		budget = 600 * time.Second
	}
	timer := time.NewTimer(budget)
	defer timer.Stop()
	select {
	case <-q.tokens:
		q.held.Add(1)
		waited := time.Since(start)
		q.noteServed(waited)
		return nil
	case <-timer.C:
		q.mu.Lock()
		q.rejected++
		q.mu.Unlock()
		return ErrNoSlot
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (q *Queue) noteServed(waited time.Duration) {
	q.mu.Lock()
	q.served++
	q.waitedMS = waited.Milliseconds()
	if waited > time.Second {
		q.delayed++
	}
	q.mu.Unlock()
	if q.opts.Logf != nil && waited > time.Second {
		q.opts.Logf("[queue] 等待空槽 %s 后取得许可", waited.Round(time.Millisecond))
	}
}

// Release 还一个槽位。多还会被当场拦下（而不是把额度悄悄放大）——
// 额度被放大意味着下一批请求会把上游撞满。
//
// 判据是**持有计数**而不是 channel 的空位：channel 的物理容量比逻辑额度大（见 slotCeiling），
// 只看 channel 会让重复放槽悄悄成功。
func (q *Queue) Release() {
	if q.held.Add(-1) < 0 {
		q.held.Store(0)
		if q.opts.Logf != nil {
			q.opts.Logf("[queue] 多余的 Release 被忽略（说明有路径重复放槽）")
		}
		return
	}
	select {
	case q.tokens <- struct{}{}:
	default:
		if q.opts.Logf != nil {
			q.opts.Logf("[queue] 许可 channel 已满，Release 被丢弃")
		}
	}
}

// RaiseCapacity 把并发容量**只往上**调到 target，返回最终容量。
//
// 为什么只放宽、不收紧：
//   - 收紧会把已经在跑的任务卡在"额度之外"（它们跑完才释放，期间新任务全等）；
//   - 放宽的最坏后果是上游多接一两个任务 ⇒ 它回 `queue is full`，而我们已有退避重试兜底。
//     方向是安全的。
func (q *Queue) RaiseCapacity(target int) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	if target <= q.opts.Capacity {
		return q.opts.Capacity
	}
	delta := target - q.opts.Capacity
	q.opts.Capacity = target
	// 只补"当前可用"之外的许可：channel 里已有的许可算作已放宽。
	for i := 0; i < delta; i++ {
		select {
		case q.tokens <- struct{}{}:
		default:
			return q.opts.Capacity
		}
	}
	return q.opts.Capacity
}

// Stats 返回闸门快照。
func (q *Queue) Stats() Stats {
	q.mu.Lock()
	defer q.mu.Unlock()
	tasks := make([]string, 0, len(q.running))
	for id := range q.running {
		tasks = append(tasks, id)
	}
	return Stats{
		Capacity:      q.opts.Capacity,
		InUse:         int(q.held.Load()),
		RunningTasks:  tasks,
		ServedTotal:   q.served,
		DelayedTotal:  q.delayed,
		RejectedTotal: q.rejected,
		WaitedMS:      q.waitedMS,
	}
}

// Watch 起一个后台盯梢：轮询到终态后**才放槽位**。
//
// 调用前必须已经 Acquire 成功。盯梢失败也必须放槽位，否则闸门会永久卡死 ——
// 那是原项目实测过的故障模式（整条线停摆）。
func (q *Queue) Watch(taskID string) {
	q.mu.Lock()
	q.running[taskID] = time.Now()
	q.mu.Unlock()

	go func() {
		defer func() {
			q.mu.Lock()
			delete(q.running, taskID)
			q.mu.Unlock()
			q.Release()
			if r := recover(); r != nil && q.opts.Logf != nil {
				q.opts.Logf("[queue] 盯梢 panic（槽位已放）: %v", r)
			}
		}()
		if q.client == nil {
			return
		}
		status, done := q.client.WaitForTask(context.Background(), taskID,
			q.opts.WatchTimeout, q.opts.PollInterval, nil)
		if q.opts.Logf != nil {
			if done {
				q.opts.Logf("[queue] 任务进终态 %s status=%s —— 放槽位", taskID, status)
			} else {
				q.opts.Logf("[queue] 盯梢到达时限 %s —— 放槽位（任务记录留给调用方继续轮询）", taskID)
			}
		}
	}()
}

// Submit 是"占槽位 → 提交 → 盯梢"的便捷封装（测试与单账号模式用）。
func (q *Queue) Submit(ctx context.Context, params map[string]any, token string) (string, error) {
	if q.client == nil {
		return "", errors.New("queue has no upstream client")
	}
	if err := q.Acquire(ctx); err != nil {
		return "", err
	}
	taskID, err := q.client.Create(ctx, params, token)
	if err != nil {
		q.Release()
		return "", err
	}
	q.Watch(taskID)
	return taskID, nil
}

// Package monitor 是监控调度与执行引擎。
//
// 调度模型：单 goroutine 每秒扫一遍所有监控的 nextAt，到期的投递到 worker 池。
// 1000 个监控每秒最多做 1000 次时间比较，开销可以忽略；相比每个监控一个
// goroutine，内存占用和上下文切换都低一个数量级。
package monitor

import (
	"context"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dislazy/uptime-kuma/go_server/internal/checkers"
	"github.com/dislazy/uptime-kuma/go_server/internal/hub"
	"github.com/dislazy/uptime-kuma/go_server/internal/logx"
	"github.com/dislazy/uptime-kuma/go_server/internal/model"
	"github.com/dislazy/uptime-kuma/go_server/internal/store"
)

// Notifier 由 notify.Engine 实现。用接口是为了让 monitor 包不依赖具体渠道实现。
type Notifier interface {
	Notify(ctx context.Context, m *model.Monitor, beat model.Beat, prevStatus int)
	// NotifyCertificate 发送证书到期提醒（官方 Notification.send(config, msg)），
	// 返回是否至少一个渠道成功。
	NotifyCertificate(ctx context.Context, monitorID int64, msg string) bool
	// HasNotifications 快速判断某监控是否绑定了通知渠道（官方 fail-fast 优化）。
	HasNotifications(ctx context.Context, monitorID int64) bool
}

// Manager 管理所有监控的运行时状态。
type Manager struct {
	db     *store.DB
	mon    *store.MonitorStore
	beats  *store.HeartbeatStore
	stats  *store.StatsStore
	hub    *hub.Hub
	notify Notifier
	tls    *store.TLSInfoStore
	set    *store.Settings

	maxConcurrent int
	sem           chan struct{}

	mu     sync.RWMutex
	states map[int64]*state

	// maintFn 由维护窗口管理器注入，用于判断监控是否处于维护中
	maintFn func(monitorID int64) bool

	statsMu    sync.Mutex
	statsCache map[int64]cachedStats

	stopOnce sync.Once
	stop     chan struct{}
	wg       sync.WaitGroup
}

// state 是单个监控的运行时状态。
type state struct {
	m       *model.Monitor
	nextAt  time.Time
	running atomic.Bool

	prevStatus *int
	retries    int
	downCount  int
	lastBeatAt time.Time
	lastBeat   *model.Beat
}

type cachedStats struct {
	at   time.Time
	data map[string]any
}

// NewManager 构造调度器。
func NewManager(
	db *store.DB,
	mon *store.MonitorStore,
	beats *store.HeartbeatStore,
	stats *store.StatsStore,
	h *hub.Hub,
	n Notifier,
	tls *store.TLSInfoStore,
	set *store.Settings,
	maxConcurrent int,
) *Manager {
	if maxConcurrent <= 0 {
		maxConcurrent = 20
	}
	return &Manager{
		db:            db,
		mon:           mon,
		beats:         beats,
		stats:         stats,
		hub:           h,
		notify:        n,
		tls:           tls,
		set:           set,
		maxConcurrent: maxConcurrent,
		sem:           make(chan struct{}, maxConcurrent),
		states:        map[int64]*state{},
		statsCache:    map[int64]cachedStats{},
		stop:          make(chan struct{}),
	}
}

// Start 载入监控并启动调度循环。
func (mgr *Manager) Start(ctx context.Context) error {
	if err := mgr.Reload(ctx); err != nil {
		return err
	}

	mgr.wg.Add(1)
	go func() {
		defer mgr.wg.Done()
		mgr.loop(ctx)
	}()

	logx.Infof("monitor", "调度器已启动，共 %d 个监控，并发上限 %d", mgr.ActiveCount(), mgr.maxConcurrent)
	return nil
}

// Stop 停止调度。
func (mgr *Manager) Stop() {
	mgr.stopOnce.Do(func() { close(mgr.stop) })
	mgr.wg.Wait()
}

// Reload 从库里重新载入全部监控。
func (mgr *Manager) Reload(ctx context.Context) error {
	list, err := mgr.mon.List(ctx, true)
	if err != nil {
		return err
	}

	// 先找出还没接管的监控，然后在锁外查它们的历史状态。
	// 持锁做 N 次 DB 查询会卡住调度循环，启动时监控多的话很明显。
	mgr.mu.Lock()
	pending := make([]int64, 0, len(list))
	for _, mon := range list {
		if _, ok := mgr.states[mon.ID]; !ok {
			pending = append(pending, mon.ID)
		}
	}
	mgr.mu.Unlock()

	prev := make(map[int64]*int, len(pending))
	for _, id := range pending {
		prev[id] = mgr.restorePrevStatus(ctx, id)
	}

	now := time.Now()
	mgr.mu.Lock()
	defer mgr.mu.Unlock()

	seen := make(map[int64]struct{}, len(list))
	for _, mon := range list {
		seen[mon.ID] = struct{}{}
		if st, ok := mgr.states[mon.ID]; ok {
			st.m = mon
			continue
		}
		// 首次加载时把启动时间打散，避免所有监控同时开跑
		jitter := time.Duration(rand.Intn(3000)) * time.Millisecond
		mgr.states[mon.ID] = &state{m: mon, nextAt: now.Add(jitter), prevStatus: prev[mon.ID]}
	}

	for id := range mgr.states {
		if _, ok := seen[id]; !ok {
			delete(mgr.states, id)
		}
	}
	return nil
}

// Upsert 刷新单个监控（新增或编辑后调用）。
func (mgr *Manager) Upsert(ctx context.Context, id int64) error {
	mon, err := mgr.mon.Get(ctx, id)
	if err != nil {
		return err
	}
	if mon == nil {
		return nil
	}

	mgr.mu.Lock()
	defer mgr.mu.Unlock()

	if st, ok := mgr.states[id]; ok {
		st.m = mon
		return nil
	}
	// 首次接管这个监控时，用库里最后一条心跳恢复上一次状态。
	// 否则每次重启，当前处于 DOWN 的监控都会被当成"刚变成 DOWN"再告警一遍。
	mgr.states[id] = &state{
		m:          mon,
		nextAt:     time.Now(),
		prevStatus: mgr.restorePrevStatus(ctx, id),
	}
	return nil
}

// restorePrevStatus 读最近一条心跳的状态，作为状态对比的起点。
// 没有历史心跳时返回 nil（表示"还没检查过"）。
func (mgr *Manager) restorePrevStatus(ctx context.Context, monitorID int64) *int {
	beats, err := mgr.beats.Recent(ctx, monitorID, 1)
	if err != nil || len(beats) == 0 {
		return nil
	}
	st := beats[0].Status
	return &st
}

// Remove 移除监控。
func (mgr *Manager) Remove(id int64) {
	mgr.mu.Lock()
	delete(mgr.states, id)
	mgr.mu.Unlock()

	mgr.statsMu.Lock()
	delete(mgr.statsCache, id)
	mgr.statsMu.Unlock()
}

// ActiveCount 返回在跑的监控数。
func (mgr *Manager) ActiveCount() int {
	mgr.mu.RLock()
	defer mgr.mu.RUnlock()
	return len(mgr.states)
}

// RunNow 立即触发一次检查（前端点"测试"时用）。
func (mgr *Manager) RunNow(id int64) {
	mgr.mu.RLock()
	st := mgr.states[id]
	mgr.mu.RUnlock()
	if st == nil {
		return
	}

	if !st.running.CompareAndSwap(false, true) {
		return
	}
	mgr.wg.Add(1)
	go func() {
		defer mgr.wg.Done()
		mgr.execute(context.Background(), st)
	}()
}

// LastBeat 返回某监控最近一次心跳（内存副本）。
func (mgr *Manager) LastBeat(id int64) *model.Beat {
	mgr.mu.RLock()
	defer mgr.mu.RUnlock()
	st := mgr.states[id]
	if st == nil {
		return nil
	}
	return st.lastBeat
}

// ---------------- 调度循环 ----------------

func (mgr *Manager) loop(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-mgr.stop:
			return
		case <-ticker.C:
			mgr.dispatch(ctx)
		}
	}
}

func (mgr *Manager) dispatch(ctx context.Context) {
	now := time.Now()

	mgr.mu.RLock()
	due := make([]*state, 0, 8)
	for _, st := range mgr.states {
		if !st.m.Active {
			continue
		}
		if skipCheck(st.m.Type) {
			continue
		}
		if st.nextAt.After(now) {
			continue
		}
		due = append(due, st)
	}
	mgr.mu.RUnlock()

	for _, st := range due {
		if !st.running.CompareAndSwap(false, true) {
			continue
		}
		// 控制并发：信号量满了就等，避免一次性打爆网络
		select {
		case mgr.sem <- struct{}{}:
		case <-ctx.Done():
			st.running.Store(false)
			return
		}

		mgr.wg.Add(1)
		go func(s *state) {
			defer mgr.wg.Done()
			defer func() { <-mgr.sem }()
			mgr.execute(ctx, s)
		}(st)
	}
}

// skipCheck 返回该类型是否由外部事件驱动、不需要主动轮询。
func skipCheck(typ string) bool {
	switch typ {
	case "push", "manual":
		return true
	default:
		return false
	}
}

// ---------------- 单次执行 ----------------

// SetMaintenance 注入「该监控是否处于维护中」的判定函数。
// 由 maintenance.Manager 提供，用函数注入是为了避免包循环依赖。
func (mgr *Manager) SetMaintenance(fn func(monitorID int64) bool) {
	mgr.mu.Lock()
	mgr.maintFn = fn
	mgr.mu.Unlock()
}

// underMaintenance 沿 parent 链判断维护态：
// 分组处于维护中时，组内监控同样视为维护中（与官方 isUnderMaintenance 一致）。
func (mgr *Manager) underMaintenance(mon *model.Monitor) bool {
	mgr.mu.RLock()
	fn := mgr.maintFn
	mgr.mu.RUnlock()
	if fn == nil {
		return false
	}

	if fn(mon.ID) {
		return true
	}

	cur := mon
	seen := map[int64]bool{mon.ID: true}
	// 限制层数，防止 parent 数据成环时死循环
	for i := 0; i < 32; i++ {
		if cur.Parent == nil {
			return false
		}
		pid := *cur.Parent
		if pid <= 0 || seen[pid] {
			return false
		}
		seen[pid] = true

		if fn(pid) {
			return true
		}

		mgr.mu.RLock()
		pst, ok := mgr.states[pid]
		mgr.mu.RUnlock()
		if !ok {
			return false
		}
		cur = pst.m
	}
	return false
}

func (mgr *Manager) execute(ctx context.Context, st *state) {
	defer st.running.Store(false)

	mon := st.m
	start := time.Now()

	// 维护窗口内不真正发起检查，直接记一条 MAINTENANCE 心跳。
	// 官方同样是在 check 之前拦截，避免维护期间产生无意义的告警。
	if mgr.underMaintenance(mon) {
		mgr.record(ctx, st, checkers.Result{
			Status: model.StatusMaintenance,
			Msg:    "Monitor under maintenance",
		}, start)
		return
	}

	// 分组不是真实探测：状态由子监控的最新心跳汇总得到，
	// 所以不走 checkers 注册表，直接在这里算。
	if mon.Type == "group" {
		mgr.record(ctx, st, mgr.checkGroup(ctx, mon), start)
		return
	}

	checker, ok := checkers.Get(mon.Type)
	if !ok {
		mgr.record(ctx, st, checkers.Down("Monitor type %q is not supported by this build", mon.Type), start)
		return
	}

	// 代理解析（官方 monitor.js:654：HTTP 家族每次探测前 R.load proxy）。
	// 每次从库取最新配置，代理内容的修改无需重启/重载即可生效。
	mgr.attachProxy(ctx, mon)

	// 远程浏览器解析（官方 getRemoteBrowser：每次探测现查 remote_browser 表）。
	mgr.attachRemoteBrowser(ctx, mon)

	timeout := checkers.Timeout(mon)
	checkCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	res := checker.Check(checkCtx, mon)

	// upside_down：把结果反过来理解（例如"页面打不开才算正常"）
	if mon.UpsideDown {
		res.Status = model.FlipStatus(res.Status)
	}

	// 重试窗口：判定为故障但还有重试次数时，先记为 PENDING，避免误报
	if res.Status == model.StatusDown && mon.MaxRetries > 0 && st.retries < mon.MaxRetries {
		st.retries++
		res.Status = model.StatusPending
		res.Msg = sprintfRetry(res.Msg, st.retries, mon.MaxRetries)
		defer mgr.scheduleRetry(st, mon)
	}

	mgr.record(ctx, st, res, start)
}

func (mgr *Manager) scheduleRetry(st *state, mon *model.Monitor) {
	wait := mon.RetryInterval
	if wait <= 0 {
		wait = mon.Interval
	}
	st.nextAt = time.Now().Add(time.Duration(wait) * time.Second)
}

// attachProxy 为 HTTP 家族监控解析 proxy_id 对应的代理配置。
//
// 官方语义：只有 http / keyword / json-query 走代理（monitor.js:538 分支内），
// 其它类型即使配了 proxy_id 也直连；proxy_id 无效或代理被停用时直连。
func (mgr *Manager) attachProxy(ctx context.Context, mon *model.Monitor) {
	// 先清旧值：上一次检查注入的内容可能已过期（代理被删/停用）
	checkers.ClearProxy(mon)

	switch mon.Type {
	case "http", "keyword", "json-query":
	default:
		return
	}

	pid := mon.GetInt("proxy_id")
	if pid <= 0 {
		return
	}
	p, err := mgr.mon.ActiveProxyByID(ctx, pid)
	if err != nil {
		logx.Debugf("monitor", "监控 %d 的代理 %d 不可用，直连: %v", mon.ID, pid, err)
		return
	}
	checkers.SetProxy(mon, checkers.ProxyInfo{
		Protocol: p.Protocol,
		Host:     p.Host,
		Port:     p.Port,
		Auth:     p.Auth,
		Username: p.Username,
		Password: p.Password,
		Active:   p.Active,
	})
}

// attachRemoteBrowser 为 real-browser 监控解析 remote_browser 对应的 WS 端点。
//
// 官方语义：只有 real-browser 类型消费 remote_browser，每次探测现查表
// （getRemoteBrowser）。查不到时不注入，由 checker 报
// "Remote browser not found" 判 DOWN（官方同款抛错语义）。
func (mgr *Manager) attachRemoteBrowser(ctx context.Context, mon *model.Monitor) {
	checkers.ClearRemoteBrowser(mon)
	if mon.Type != "real-browser" {
		return
	}
	id := mon.GetInt("remote_browser")
	if id <= 0 {
		return
	}
	url, err := mgr.mon.RemoteBrowserURLByID(ctx, id)
	if err != nil {
		logx.Debugf("monitor", "监控 %d 的远程浏览器 %d 不可用: %v", mon.ID, id, err)
		return
	}
	checkers.SetRemoteBrowser(mon, url)
}

// record 落库 + 广播 + 通知。
func (mgr *Manager) record(ctx context.Context, st *state, res checkers.Result, startedAt time.Time) {
	mon := st.m

	now := time.Now()
	duration := 0
	if !st.lastBeatAt.IsZero() {
		duration = int(now.Sub(st.lastBeatAt).Seconds())
	}
	st.lastBeatAt = now

	// 状态没变化就清空重试计数
	if res.Status == model.StatusUp {
		st.retries = 0
	}

	important := false
	prevStatus := model.StatusUp
	if st.prevStatus != nil {
		prevStatus = *st.prevStatus
		important = prevStatus != res.Status
	} else {
		// 首次检查：新建的监控第一次跑就挂了，必须告警。
		// 之前这里留 false，导致"建完就 down"的监控一条通知都发不出去。
		// PENDING 是中间态（还在重试），不当成状态变化。
		important = res.Status != model.StatusPending
	}

	// down 计数用于展示"已连续失败 N 次"
	switch res.Status {
	case model.StatusDown:
		st.downCount++
	case model.StatusUp:
		st.downCount = 0
	}

	beat := model.Beat{
		MonitorID: mon.ID,
		Status:    res.Status,
		Msg:       res.Msg,
		Time:      store.FormatTime(now),
		Ping:      res.Ping,
		Duration:  duration,
		Important: important,
		DownCount: st.downCount,
	}

	st.prevStatus = &res.Status
	st.lastBeat = &beat
	if res.Status != model.StatusPending {
		st.nextAt = now.Add(time.Duration(mon.Interval) * time.Second)
	}

	// 落库（异步批量）
	mgr.beats.Enqueue(beat)

	// 聚合统计
	if res.Ping != nil {
		minuteTS := now.Unix() / 60 * 60
		if err := mgr.stats.UpsertMinutely(ctx, mon.ID, minuteTS, *res.Ping, res.Status); err != nil {
			logx.Debugf("monitor", "统计写入失败 monitor #%d: %v", mon.ID, err)
		}
	}
	mgr.invalidateStats(mon.ID)

	// dns 结果回写
	if res.DNSResult != "" {
		if err := mgr.mon.SetColumn(ctx, mon.ID, "dns_last_result", res.DNSResult); err != nil {
			logx.Debugf("monitor", "回写 dns_last_result 失败: %v", err)
		}
	}

	// TLS 证书信息（官方 readTlsInfo → handleTlsInfo 链路）
	if res.TLS != nil {
		mgr.handleTLSInfo(ctx, mon, res.TLS)
	}

	// 实时广播
	payload := beat.JSON()
	payload["stats"] = mgr.statsFor(ctx, mon.ID)
	mgr.hub.Broadcast("heartbeat", payload)

	if important && mgr.notify != nil {
		mgr.notify.Notify(ctx, mon, beat, prevStatus)
	}

	logx.Debugf("monitor", "[%s] %s %s (%dms)", mon.Name, model.StatusName(beat.Status), beat.Msg, elapsedMS(startedAt))
}

// ---------------- 统计 ----------------

func (mgr *Manager) statsFor(ctx context.Context, monitorID int64) map[string]any {
	const cacheTTL = 30 * time.Second

	mgr.statsMu.Lock()
	if c, ok := mgr.statsCache[monitorID]; ok && time.Since(c.at) < cacheTTL {
		mgr.statsMu.Unlock()
		return c.data
	}
	mgr.statsMu.Unlock()

	now := time.Now().Unix()
	data := map[string]any{}

	if w, err := mgr.stats.Query(ctx, "stat_minutely", monitorID, now-86400, now); err == nil {
		data["uptime24"] = round2(w.Percent())
		data["avgPing"] = round2(w.Ping)
	}
	if w, err := mgr.stats.Query(ctx, "stat_hourly", monitorID, now-720*3600, now); err == nil {
		data["uptime720"] = round2(w.Percent())
	}
	if w, err := mgr.stats.Query(ctx, "stat_daily", monitorID, now-365*86400, now); err == nil {
		data["uptime1y"] = round2(w.Percent())
	}

	mgr.statsMu.Lock()
	mgr.statsCache[monitorID] = cachedStats{at: time.Now(), data: data}
	mgr.statsMu.Unlock()

	return data
}

func (mgr *Manager) invalidateStats(monitorID int64) {
	mgr.statsMu.Lock()
	delete(mgr.statsCache, monitorID)
	mgr.statsMu.Unlock()
}

func round2(f float64) float64 {
	return float64(int(f*100+0.5)) / 100
}

func elapsedMS(t time.Time) int64 { return time.Since(t).Milliseconds() }

// Package logging 提供结构化日志。
//
// 依据用户偏好：日志只输出到文件，不接入 OTel / 链路追踪。
// 这里用 log/slog 输出 JSON，按天切分文件，并保留最近 N 天。
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Options 日志配置。
type Options struct {
	// Level 取值：debug | info | warn | error。
	Level string
	// Dir 日志目录。为空则只输出到 stdout。
	Dir string
	// FileName 日志文件名前缀，默认 Vault-Server。
	FileName string
	// KeepDays 保留天数，<=0 表示不清理。
	KeepDays int
	// AlsoConsole 是否同时输出到 stdout（便于前台调试）。
	AlsoConsole bool
	// SingleFilePath 固定文件日志的完整路径（如 <配置目录>/logs/vault-server.log）。
	//
	// 与 Dir 的按天切分无关：它额外追加一个**位置固定、便于运维直接找到**的文件，
	// 确保双击/计划任务等 stdout 无人接收的场景下，崩溃现场（含 panic 栈）不丢失。
	// 启用时始终同时写 stdout；文件打开失败只降级为仅 stdout 并记 WARN，不阻断启动。
	// 为空则不启用固定文件。
	SingleFilePath string
}

// singleFileMaxBytes 是固定文件日志的单文件体积上限（32MiB）。
//
// 体积控制策略（刻意保持朴素）：写入前若"当前大小 + 本次长度"将超过该阈值，
// 就把现有文件改名为 <path>.1 后重新打开，**只保留一个历史文件**。
const singleFileMaxBytes = 32 << 20

// Logger 是带关闭能力的日志句柄。
type Logger struct {
	*slog.Logger

	closer io.Closer
	// daily 按天切分的文件写入器（未启用文件日志时为 nil）。
	// 供 Days / PathForDay / ReadDay 为日志模块提供只读访问。
	daily *dailyRotator
}

// Close 关闭底层文件句柄。
func (l *Logger) Close() error {
	if l == nil || l.closer == nil {
		return nil
	}
	return l.closer.Close()
}

// New 构造 Logger。
func New(opt Options) (*Logger, error) {
	level, err := parseLevel(opt.Level)
	if err != nil {
		return nil, err
	}

	var dailyWriter io.Writer
	var daily *dailyRotator
	var closers []io.Closer

	if opt.Dir != "" {
		name := opt.FileName
		if name == "" {
			name = "Vault-Server"
		}
		w, err := newDailyRotator(opt.Dir, name, opt.KeepDays)
		if err != nil {
			return nil, err
		}
		dailyWriter = w
		daily = w
		closers = append(closers, w)
	}

	// 固定文件（可选）：打开失败不阻断启动，仅降级为 stdout 并在日志建好后补一条 WARN。
	var singleWriter io.Writer
	var singleFileErr error
	if strings.TrimSpace(opt.SingleFilePath) != "" {
		w, err := newSizeRotatingFile(opt.SingleFilePath, singleFileMaxBytes)
		if err != nil {
			singleFileErr = err
		} else {
			singleWriter = w
			closers = append(closers, w)
		}
	}

	// 写入顺序：固定文件优先（MultiWriter 遇到首个错误即停止，固定文件必须先写，
	// 以免被其它 writer 的偶发失败拖累——它是崩溃现场的唯一可靠落点）。
	var writers []io.Writer
	if singleWriter != nil {
		writers = append(writers, singleWriter)
	}
	// 固定文件启用时始终附带 stdout：即使 also_console=false，也要保证前台可见一份。
	if opt.AlsoConsole || opt.SingleFilePath != "" || dailyWriter == nil {
		writers = append(writers, os.Stdout)
	}
	if dailyWriter != nil {
		writers = append(writers, dailyWriter)
	}

	var out io.Writer = writers[0]
	if len(writers) > 1 {
		out = io.MultiWriter(writers...)
	}

	handler := slog.NewJSONHandler(out, &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			// 时间统一为毫秒时间戳，便于与 DB 中的 INTEGER 时间字段对齐。
			if a.Key == slog.TimeKey {
				if t, ok := a.Value.Any().(time.Time); ok {
					return slog.Int64("ts_ms", t.UnixMilli())
				}
			}
			return a
		},
	})

	logger := slog.New(handler)
	if singleFileErr != nil {
		logger.Warn("固定日志文件不可用，已降级为仅 stdout",
			"path", opt.SingleFilePath, "error", singleFileErr)
	}

	return &Logger{Logger: logger, closer: multiCloser(closers), daily: daily}, nil
}

// Days 返回存在日志文件的日期（YYYY-MM-DD，升序；未启用按天文件时为空切片）。
//
// 供日志模块做"按时间切分"：界面据此列出可查询的日期，避免只能看到当天。
func (l *Logger) Days() []string {
	if l == nil || l.daily == nil {
		return []string{}
	}
	return l.daily.days()
}

// PathForDay 返回指定日期日志文件的完整路径；day 为空表示当天。
// 未启用按天文件、或 day 不是合法日期时返回空串。
func (l *Logger) PathForDay(day string) string {
	if l == nil || l.daily == nil {
		return ""
	}
	return l.daily.pathForDay(day)
}

// ReadDay 读取指定日期日志文件末尾最多 maxBytes 字节；day 为空表示当天。
func (l *Logger) ReadDay(day string, maxBytes int64) ([]byte, error) {
	if l == nil || l.daily == nil {
		return nil, os.ErrNotExist
	}
	return l.daily.readDayTail(day, maxBytes)
}

// multiCloser 把多个 Closer 合成一个（保持关闭顺序）。
func multiCloser(closers []io.Closer) io.Closer {
	switch len(closers) {
	case 0:
		return nil
	case 1:
		return closers[0]
	default:
		return &closerGroup{closers: closers}
	}
}

type closerGroup struct{ closers []io.Closer }

func (c *closerGroup) Close() error {
	var firstErr error
	for _, cl := range c.closers {
		if err := cl.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func parseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("logging: 未知日志级别 %q", s)
	}
}

// dailyRotator 是按天切分的文件写入器，写满一天自动换文件并清理过期文件。
type dailyRotator struct {
	dir      string
	prefix   string
	keepDays int

	mu   sync.Mutex
	day  string
	file *os.File
}

func newDailyRotator(dir, prefix string, keepDays int) (*dailyRotator, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("logging: 创建日志目录失败: %w", err)
	}
	r := &dailyRotator{dir: dir, prefix: prefix, keepDays: keepDays}
	if err := r.rotate(time.Now()); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *dailyRotator) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	if now.Format("2006-01-02") != r.day {
		if err := r.rotate(now); err != nil {
			return 0, err
		}
	}
	return r.file.Write(p)
}

func (r *dailyRotator) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.file == nil {
		return nil
	}
	err := r.file.Close()
	r.file = nil
	return err
}

// pathForDay 返回指定日期日志文件的完整路径；day 为空表示当天。
// day 不是合法日期时返回空串（调用方据此拒绝，避免把参数当路径用）。
func (r *dailyRotator) pathForDay(day string) string {
	d, ok := parseDay(day)
	if !ok {
		return ""
	}
	return filepath.Join(r.dir, fmt.Sprintf("%s-%s.log", r.prefix, d))
}

// days 列出目录中已存在的按天日志文件日期（YYYY-MM-DD，升序）。
func (r *dailyRotator) days() []string {
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		return []string{}
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if day, ok := r.dayOf(e.Name()); ok {
			out = append(out, day)
		}
	}
	sort.Strings(out)
	return out
}

// dayOf 从日志文件名解析日期（YYYY-MM-DD）；命名不匹配返回 false。
func (r *dailyRotator) dayOf(name string) (string, bool) {
	if !strings.HasPrefix(name, r.prefix+"-") || !strings.HasSuffix(name, ".log") {
		return "", false
	}
	day := strings.TrimSuffix(strings.TrimPrefix(name, r.prefix+"-"), ".log")
	if _, err := time.ParseInLocation("2006-01-02", day, time.Local); err != nil {
		return "", false
	}
	return day, true
}

// parseDay 校验并规范化 YYYY-MM-DD；空串表示当天。
func parseDay(day string) (string, bool) {
	day = strings.TrimSpace(day)
	if day == "" {
		return time.Now().Format("2006-01-02"), true
	}
	if _, err := time.ParseInLocation("2006-01-02", day, time.Local); err != nil {
		return "", false
	}
	return day, true
}

// readDayTail 读取指定日期日志文件末尾最多 maxBytes 字节（日志模块查询用，尽力而为）。
func (r *dailyRotator) readDayTail(day string, maxBytes int64) ([]byte, error) {
	path := r.pathForDay(day)
	if path == "" {
		return nil, fmt.Errorf("logging: 非法的日志日期 %q", day)
	}
	if maxBytes <= 0 {
		maxBytes = 256 << 10
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := info.Size()
	offset := size - maxBytes
	if offset < 0 {
		offset = 0
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, err
	}
	buf := make([]byte, size-offset)
	if _, err := io.ReadFull(f, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// rotate 切换到指定日期对应的文件。调用方需持有锁（初始化除外）。
func (r *dailyRotator) rotate(t time.Time) error {
	day := t.Format("2006-01-02")
	if r.file != nil {
		_ = r.file.Close()
		r.file = nil
	}
	path := filepath.Join(r.dir, fmt.Sprintf("%s-%s.log", r.prefix, day))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("logging: 打开日志文件失败: %w", err)
	}
	r.file = f
	r.day = day
	r.cleanupLocked()
	return nil
}

// cleanupLocked 删除超出保留期的日志文件。
func (r *dailyRotator) cleanupLocked() {
	if r.keepDays <= 0 {
		return
	}
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		return
	}
	cutoff := time.Now().AddDate(0, 0, -r.keepDays)

	var candidates []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		dayStr, ok := r.dayOf(name)
		if !ok {
			continue
		}
		d, err := time.ParseInLocation("2006-01-02", dayStr, time.Local)
		if err != nil {
			continue
		}
		if d.Before(cutoff) {
			candidates = append(candidates, name)
		}
	}
	sort.Strings(candidates)
	for _, name := range candidates {
		_ = os.Remove(filepath.Join(r.dir, name))
	}
}

// sizeRotatingFile 是"固定文件名 + 按体积滚动"的写入器。
//
// 与 dailyRotator 的区别：文件名固定（便于运维直接找到），滚动由体积触发而非日期。
// 只保留一个历史文件（<path>.1），避免无限增长。
type sizeRotatingFile struct {
	path     string
	maxBytes int64

	mu   sync.Mutex
	file *os.File
	size int64
}

// newSizeRotatingFile 打开（不存在则创建）固定文件；目录不存在则创建。
func newSizeRotatingFile(path string, maxBytes int64) (*sizeRotatingFile, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("logging: 创建日志目录失败: %w", err)
		}
	}
	w := &sizeRotatingFile{path: path, maxBytes: maxBytes}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

// open 以追加方式打开当前文件并记录现有大小。
func (w *sizeRotatingFile) open() error {
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("logging: 打开日志文件失败: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("logging: 获取日志文件大小失败: %w", err)
	}
	w.file = f
	w.size = info.Size()
	return nil
}

func (w *sizeRotatingFile) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return 0, io.ErrClosedPipe
	}
	if w.maxBytes > 0 && w.size+int64(len(p)) > w.maxBytes {
		// 滚动失败不应丢日志：忽略错误，继续往原文件追加。
		_ = w.rotate()
	}
	n, err := w.file.Write(p)
	w.size += int64(n)
	return n, err
}

// rotate 把当前文件改名为 <path>.1（覆盖旧备份）后重新打开。
func (w *sizeRotatingFile) rotate() error {
	if w.file != nil {
		_ = w.file.Close()
		w.file = nil
	}
	backup := w.path + ".1"
	// Windows 下 rename 到已存在的文件会失败，先删除旧备份；改名失败（如被占用）
	// 也一并忽略——后续 open 会继续追加到原文件，不阻断日志。
	_ = os.Remove(backup)
	_ = os.Rename(w.path, backup)
	return w.open()
}

func (w *sizeRotatingFile) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}

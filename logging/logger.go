package logging

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/teambition/gear"
)

var crlfEscaper = strings.NewReplacer("\r", "\\r", "\n", "\\n")

// Messager is implemented by any value that has a Format method and a String method.
// They are using by Logger to format value to string.
type Messager interface {
	fmt.Stringer
	Format() (string, error)
}

// Log records key-value pairs for structured logging.
type Log map[string]interface{}

// Format try to marshal the structured log with json.Marshal.
func (l Log) Format() (string, error) {
	res, err := json.Marshal(l)
	if err == nil {
		return string(res), nil
	}
	return "", err
}

// GoString implemented fmt.GoStringer interface, returns a Go-syntax string.
func (l Log) GoString() string {
	count := len(l)
	buf := bytes.NewBufferString("Log{")
	for key, value := range l {
		if count--; count == 0 {
			fmt.Fprintf(buf, "%s:%#v}", key, value)
		} else {
			fmt.Fprintf(buf, "%s:%#v, ", key, value)
		}
	}
	return buf.String()
}

// String implemented fmt.Stringer interface, returns a Go-syntax string.
func (l Log) String() string {
	return l.GoString()
}

// From copy values from the Log argument, returns self.
//  log := Log{"key": "foo"}
//  logging.Info(log.From(Log{"key2": "foo2"}))
func (l Log) From(log Log) Log {
	for key, val := range log {
		l[key] = val
	}
	return l
}

// Into copy self values into the Log argument, returns the Log argument.
//  redisLog := Log{"kind": "redis"}
//  logging.Err(redisLog.Into(Log{"data": "foo"}))
func (l Log) Into(log Log) Log {
	for key, val := range l {
		log[key] = val
	}
	return log
}

// With copy values from the argument, returns new log.
//  log := Log{"key": "foo"}
//  logging.Info(log.With(Log{"key2": "foo2"}))
func (l Log) With(log map[string]interface{}) Log {
	cp := l.Into(Log{})
	for key, val := range log {
		cp[key] = val
	}
	return cp
}

// Reset delete all key-value on the log. Empty log will not be consumed.
//
//  log := logger.FromCtx(ctx)
//  if ctx.Path == "/" {
//  	log.Reset() // reset log, don't logging for path "/"
//  } else {
//  	log["Data"] = someData
//  }
//
func (l Log) Reset() {
	for key := range l {
		delete(l, key)
	}
}

// Level represents logging level
// https://tools.ietf.org/html/rfc5424
// https://en.wikipedia.org/wiki/Syslog
type Level uint8

const (
	// EmergLevel is 0, "Emergency", system is unusable
	EmergLevel Level = iota
	// AlertLevel is 1, "Alert", action must be taken immediately
	AlertLevel
	// CritiLevel is 2, "Critical", critical conditions
	CritiLevel
	// ErrLevel is 3, "Error", error conditions
	ErrLevel
	// WarningLevel is 4, "Warning", warning conditions
	WarningLevel
	// NoticeLevel is 5, "Notice", normal but significant condition
	NoticeLevel
	// InfoLevel is 6, "Informational", informational messages
	InfoLevel
	// DebugLevel is 7, "Debug", debug-level messages
	DebugLevel
)

func (l Level) String() string {
	switch l {
	case EmergLevel:
		return "EMERG"
	case AlertLevel:
		return "ALERT"
	case CritiLevel:
		return "CRIT"
	case ErrLevel:
		return "ERR"
	case WarningLevel:
		return "WARNING"
	case NoticeLevel:
		return "NOTICE"
	case InfoLevel:
		return "INFO"
	case DebugLevel:
		return "DEBUG"
	default:
		return "LOG"
	}
}

var std = New(os.Stderr)

// Default returns the default logger
// If devMode is true, logger will print a simple version of Common Log Format with terminal color
func Default(devMode ...bool) *Logger {
	if len(devMode) > 0 && devMode[0] {
		std.SetLogConsume(developmentConsume)
	}
	return std
}

// a simple version of Common Log Format with terminal color
// https://en.wikipedia.org/wiki/Common_Log_Format
//
//  127.0.0.1 - - [2017-06-01T12:23:13.161Z] "GET /context.go?query=xxx HTTP/1.1" 200 21559 5.228ms
//
func developmentConsume(log Log, ctx *gear.Context) {
	std.mu.Lock() // don't need Lock usually, logger.Output do it for us.
	defer std.mu.Unlock()

	end := time.Now().UTC()
	FprintWithColor(std.Out, fmt.Sprintf("%s", log["IP"]), ColorGreen)
	fmt.Fprintf(std.Out, ` - - [%s] "%s %s %s" `, end.Format(std.tf), log["Method"], log["URL"], log["Proto"])
	status := log["Status"].(int)
	FprintWithColor(std.Out, strconv.Itoa(status), colorStatus(status))
	resTime := float64(end.Sub(log["Start"].(time.Time))) / 1e6
	fmt.Fprintln(std.Out, fmt.Sprintf(" %d %.3fms", log["Length"], resTime))
}

// New creates a Logger instance with given io.Writer and DebugLevel log level.
// the logger timestamp format is "2006-01-02T15:04:05.999Z"(JavaScript ISO date string), log format is "[%s] %s %s"
func New(w io.Writer) *Logger {
	logger := &Logger{Out: w}
	logger.SetLevel(DebugLevel)
	logger.SetTimeFormat("2006-01-02T15:04:05.999Z")
	logger.SetLogFormat("[%s] %s %s")

	logger.init = func(log Log, ctx *gear.Context) {
		log["IP"] = ctx.IP().String()
		log["Method"] = ctx.Method
		log["URL"] = ctx.Req.URL.String()
		log["Proto"] = ctx.Req.Proto
		log["UserAgent"] = ctx.GetHeader(gear.HeaderUserAgent)
		log["Start"] = time.Now()
	}

	logger.consume = func(log Log, ctx *gear.Context) {
		end := time.Now()
		if t, ok := log["Start"].(time.Time); ok {
			log["Time"] = end.Sub(t) / 1e6 // ms
		}
		if err := logger.Output(InfoLevel.String(), end, log); err != nil {
			ctx.LogErr(err)
		}
	}

	logger.output = func(tag string, t time.Time, v interface{}) error {
		s, err := format(v)
		if err != nil {
			return err
		}
		if l := len(s); l > 0 && s[l-1] == '\n' {
			s = s[0 : l-1]
		}

		logger.mu.Lock()
		defer logger.mu.Unlock()
		_, err = fmt.Fprintf(logger.Out, logger.lf, t.UTC().Format(logger.tf), tag, crlfEscaper.Replace(s))
		if err == nil {
			logger.Out.Write([]byte{'\n'})
		}
		return err
	}
	return logger
}

// A Logger represents an active logging object that generates lines of
// output to an io.Writer. Each logging operation makes a single call to
// the Writer's Write method. A Logger can be used simultaneously from
// multiple goroutines; it guarantees to serialize access to the Writer.
//
// A custom logger example:
//
//  app := gear.New()
//
//  logger := logging.New(os.Stdout)
//  logger.SetLevel(logging.InfoLevel)
//  app.UseHandler(logger)
//  app.Use(func(ctx *gear.Context) error {
//  	logger.SetTo(ctx, "Data", []int{1, 2, 3})
//  	return ctx.HTML(200, "OK")
//  })
//
type Logger struct {
	// Destination for output, It's common to set this to a
	// file, or `os.Stderr`. You can also set this to
	// something more adventorous, such as logging to Kafka.
	Out     io.Writer
	l       Level                    // logging level
	tf, lf  string                   // time format, log format
	mu      sync.Mutex               // ensures atomic writes; protects the following fields
	init    func(Log, *gear.Context) // hook to initialize log with gear.Context
	consume func(Log, *gear.Context) // hook to consume log
	output  func(string, time.Time, interface{}) error
}

// Check log output level statisfy output level or not, used internal, for performance
func (l *Logger) checkLogLevel(level Level) bool {
	// don't satisfy logger level, so skip
	return level <= l.l
}

// Emerg produce a "Emergency" log
func (l *Logger) Emerg(v interface{}) error {
	return l.Output(EmergLevel.String(), time.Now(), gear.ErrorWithStack(v, 2))
}

// Alert produce a "Alert" log
func (l *Logger) Alert(v interface{}) error {
	if l.checkLogLevel(AlertLevel) {
		return l.Output(AlertLevel.String(), time.Now(), gear.ErrorWithStack(v, 2))
	}
	return nil
}

// Crit produce a "Critical" log
func (l *Logger) Crit(v interface{}) error {
	if l.checkLogLevel(CritiLevel) {
		return l.Output(CritiLevel.String(), time.Now(), gear.ErrorWithStack(v, 2))
	}
	return nil
}

// Err produce a "Error" log
func (l *Logger) Err(v interface{}) error {
	if l.checkLogLevel(ErrLevel) {
		return l.Output(ErrLevel.String(), time.Now(), gear.ErrorWithStack(v, 2))
	}
	return nil
}

// Warning produce a "Warning" log
func (l *Logger) Warning(v interface{}) error {
	if l.checkLogLevel(WarningLevel) {
		return l.Output(WarningLevel.String(), time.Now(), v)
	}
	return nil
}

// Notice produce a "Notice" log
func (l *Logger) Notice(v interface{}) error {
	if l.checkLogLevel(NoticeLevel) {
		return l.Output(NoticeLevel.String(), time.Now(), v)
	}
	return nil
}

// Info produce a "Informational" log
func (l *Logger) Info(v interface{}) error {
	if l.checkLogLevel(InfoLevel) {
		return l.Output(InfoLevel.String(), time.Now(), v)
	}
	return nil
}

// Debug produce a "Debug" log
func (l *Logger) Debug(v interface{}) error {
	if l.checkLogLevel(DebugLevel) {
		return l.Output(DebugLevel.String(), time.Now(), v)
	}
	return nil
}

// Debugf produce a "Debug" log in the manner of fmt.Printf
func (l *Logger) Debugf(format string, args ...interface{}) error {
	if l.checkLogLevel(DebugLevel) {
		return l.Output(DebugLevel.String(), time.Now(), fmt.Sprintf(format, args...))
	}
	return nil
}

// Panic produce a "Emergency" log and then calls panic with the message
func (l *Logger) Panic(v interface{}) {
	err := gear.ErrorWithStack(v, 2)
	l.Output(EmergLevel.String(), time.Now(), err)
	panic(err)
}

var exit = func() { os.Exit(1) }

// Fatal produce a "Emergency" log and then calls os.Exit(1)
func (l *Logger) Fatal(v interface{}) {
	l.Output(EmergLevel.String(), time.Now(), gear.ErrorWithStack(v, 2))
	exit()
}

// Print produce a log in the manner of fmt.Print, without timestamp and log level
func (l *Logger) Print(args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprint(l.Out, args...)
}

// Printf produce a log in the manner of fmt.Printf, without timestamp and log level
func (l *Logger) Printf(format string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(l.Out, format, args...)
}

// Println produce a log in the manner of fmt.Println, without timestamp and log level
func (l *Logger) Println(args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintln(l.Out, args...)
}

// Output writes a string log with timestamp and log level to the output.
// If the level is greater than logger level, the log will be omitted.
// The log will be format by timeFormat and logFormat.
func (l *Logger) Output(tag string, t time.Time, v interface{}) (err error) {
	return l.output(tag, t, v)
}

// SetLevel set the logger's log level
// The default logger level is DebugLevel
func (l *Logger) SetLevel(level Level) *Logger {
	l.mu.Lock()
	defer l.mu.Unlock()
	if level > DebugLevel {
		panic(gear.Err.WithMsg("invalid logger level"))
	}
	l.l = level
	return l
}

// SetTimeFormat set the logger timestamp format
// The default logger timestamp format is "2006-01-02T15:04:05.999Z"(JavaScript ISO date string)
func (l *Logger) SetTimeFormat(timeFormat string) *Logger {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.tf = timeFormat
	return l
}

// SetLogFormat set the logger log format
// it should accept 3 string values: timestamp, log level and log message
// The default logger log format is "[%s] %s %s": "[time] logLevel message"
func (l *Logger) SetLogFormat(logFormat string) *Logger {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lf = logFormat
	return l
}

// SetLogInit set a log init handle to the logger.
// It will be called when log created.
func (l *Logger) SetLogInit(fn func(Log, *gear.Context)) *Logger {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.init = fn
	return l
}

// SetLogConsume set a log consumer handle to the logger.
// It will be called on a "end hook" and should write the log to underlayer logging system.
func (l *Logger) SetLogConsume(fn func(Log, *gear.Context)) *Logger {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.consume = fn
	return l
}

// SetOutput set a log output handle to the logger. It only work with logger.Emerg, logger.Alert,
// logger.Crit, logger.Err, logger.Warning, logger.Notice, logger.Info, logger.Debug, logger.Debugf and logger.Output.
// Use fluent client as output handle:
//
//  fc, err := fluent.New(fluent.Config{FluentPort: 24224, FluentHost: "127.0.0.1", MarshalAsJSON: true})
//  if err != nil {
//  	panic(err)
//  }
//  logger.SetOutput(fc.EncodeAndPostData)
//
// Please implements a Log Consume for your production.
func (l *Logger) SetOutput(fn func(string, time.Time, interface{}) error) *Logger {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.output = fn
	return l
}

// New implements gear.Any interface,then we can use ctx.Any to retrieve a Log instance from ctx.
// Here also some initialization work after created.
func (l *Logger) New(ctx *gear.Context) (interface{}, error) {
	log := Log{}
	l.init(log, ctx)
	return log, nil
}

// FromCtx retrieve the Log instance from the ctx with ctx.Any.
// Logger.New and ctx.Any will guarantee it exists.
func (l *Logger) FromCtx(ctx *gear.Context) Log {
	any, _ := ctx.Any(l)
	return any.(Log)
}

// SetTo sets key/value to the Log instance on ctx.
//  app.Use(func(ctx *gear.Context) error {
//  	logging.SetTo(ctx, "Data", []int{1, 2, 3})
//  	return ctx.HTML(200, "OK")
//  })
func (l *Logger) SetTo(ctx *gear.Context, key string, val interface{}) {
	any, _ := ctx.Any(l)
	any.(Log)[key] = val
}

// Serve implements gear.Handler interface, we can use logger as gear middleware.
//
//  app := gear.New()
//  app.UseHandler(logging.Default())
//  app.Use(func(ctx *gear.Context) error {
//  	log := logging.FromCtx(ctx)
//  	log["Data"] = []int{1, 2, 3}
//  	return ctx.HTML(200, "OK")
//  })
//
func (l *Logger) Serve(ctx *gear.Context) error {
	log := l.FromCtx(ctx)
	// Add a "end hook" to flush logs
	ctx.OnEnd(func() {
		// Ignore empty log
		if len(log) == 0 {
			return
		}
		log["Status"] = ctx.Res.Status()
		log["Length"] = len(ctx.Res.Body())
		l.consume(log, ctx)
	})
	return nil
}

// Emerg produce a "Emergency" log with the default logger
func Emerg(v interface{}) error {
	return std.Emerg(v)
}

// Alert produce a "Alert" log with the default logger
func Alert(v interface{}) error {
	return std.Alert(v)
}

// Crit produce a "Critical" log with the default logger
func Crit(v interface{}) error {
	return std.Crit(v)
}

// Err produce a "Error" log with the default logger
func Err(v interface{}) error {
	return std.Err(v)
}

// Warning produce a "Warning" log with the default logger
func Warning(v interface{}) error {
	return std.Warning(v)
}

// Notice produce a "Notice" log with the default logger
func Notice(v interface{}) error {
	return std.Notice(v)
}

// Info produce a "Informational" log with the default logger
func Info(v interface{}) error {
	return std.Info(v)
}

// Debug produce a "Debug" log with the default logger
func Debug(v interface{}) error {
	return std.Debug(v)
}

// Debugf produce a "Debug" log in the manner of fmt.Printf with the default logger
func Debugf(format string, args ...interface{}) error {
	return std.Debugf(format, args...)
}

// Panic produce a "Emergency" log with the default logger and then calls panic with the message
func Panic(v interface{}) {
	std.Panic(v)
}

// Fatal produce a "Emergency" log with the default logger and then calls os.Exit(1)
func Fatal(v interface{}) {
	std.Fatal(v)
}

// Print produce a log in the manner of fmt.Print with the default logger,
// without timestamp and log level
func Print(args ...interface{}) {
	std.Print(args...)
}

// Printf produce a log in the manner of fmt.Printf with the default logger,
// without timestamp and log level
func Printf(format string, args ...interface{}) {
	std.Printf(format, args...)
}

// Println produce a log in the manner of fmt.Println with the default logger,
// without timestamp and log level
func Println(args ...interface{}) {
	std.Println(args...)
}

// FromCtx retrieve the Log instance for the default logger.
func FromCtx(ctx *gear.Context) Log {
	return std.FromCtx(ctx)
}

// SetTo sets key/value to the Log instance on ctx for the default logger.
//  app.UseHandler(logging.Default())
//  app.Use(func(ctx *gear.Context) error {
//  	logging.SetTo(ctx, "Data", []int{1, 2, 3})
//  	return ctx.HTML(200, "OK")
//  })
func SetTo(ctx *gear.Context, key string, val interface{}) {
	std.SetTo(ctx, key, val)
}

func colorStatus(code int) ColorType {
	switch {
	case code < 300:
		return ColorGreen
	case code >= 300 && code < 400:
		return ColorCyan
	case code >= 400 && code < 500:
		return ColorYellow
	default:
		return ColorRed
	}
}

func format(i interface{}) (string, error) {
	switch v := i.(type) {
	case Messager:
		str, err := v.Format()
		if err == nil {
			return str, nil
		}
		return v.String(), err
	default:
		return fmt.Sprint(i), nil
	}
}

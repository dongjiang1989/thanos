// Copyright (c) The Thanos Authors.
// Licensed under the Apache License 2.0.

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"strconv"
	"syscall"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/oklog/run"
	"github.com/opentracing/opentracing-go"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	versioncollector "github.com/prometheus/client_golang/prometheus/collectors/version"
	"github.com/prometheus/common/version"
	"go.uber.org/automaxprocs/maxprocs"
	"gopkg.in/alecthomas/kingpin.v2"

	"github.com/thanos-io/thanos/pkg/extkingpin"
	"github.com/thanos-io/thanos/pkg/logging"
	"github.com/thanos-io/thanos/pkg/tracing/client"

	// use the original golang/protobuf package we can continue serializing
	// messages from our dependencies, particularly from OTEL. Original version
	// from Vitess.
	"google.golang.org/grpc/encoding"
	"google.golang.org/grpc/mem"

	// Guarantee that the built-in proto is called registered before this one
	// so that it can be replaced.
	_ "google.golang.org/grpc/encoding/proto"
)

// Name is the name registered for the proto compressor.
const Name = "proto"

// Use lower GOGC if it isn't set yet.
// It is recommended increasing GOGC if go_memstats_gc_cpu_fraction exceeds 0.05 for extended periods of time.
const DefaultGOGC = 75

// vtprotoCodec is like the vtprotobuf codec
// but also handles non-vtproto messages that are needed
// for stuff like OpenTelemetry. Otherwise, such errors appear:
// error while marshaling: failed to marshal, message is *v1.ExportTraceServiceRequest (missing vtprotobuf helpers).
type vtprotoCodec struct {
	fallback encoding.CodecV2
}

type vtprotoMessage interface {
	MarshalVT() ([]byte, error)
	UnmarshalVT([]byte) error
	MarshalToSizedBufferVT(data []byte) (int, error)
	SizeVT() int
}

func (c *vtprotoCodec) Marshal(v any) (mem.BufferSlice, error) {
	if m, ok := v.(vtprotoMessage); ok {
		size := m.SizeVT()
		if mem.IsBelowBufferPoolingThreshold(size) {
			buf := make([]byte, size)
			if _, err := m.MarshalToSizedBufferVT(buf); err != nil {
				return nil, err
			}
			return mem.BufferSlice{mem.SliceBuffer(buf)}, nil
		}
		pool := mem.DefaultBufferPool()
		buf := pool.Get(size)
		if _, err := m.MarshalToSizedBufferVT((*buf)[:size]); err != nil {
			pool.Put(buf)
			return nil, err
		}
		return mem.BufferSlice{mem.NewBuffer(buf, pool)}, nil
	}

	return c.fallback.Marshal(v)
}

func (c *vtprotoCodec) Unmarshal(data mem.BufferSlice, v any) error {
	if m, ok := v.(vtprotoMessage); ok {
		buf := data.MaterializeToBuffer(mem.DefaultBufferPool())
		defer buf.Free()
		return m.UnmarshalVT(buf.ReadOnlyData())
	}

	return c.fallback.Unmarshal(data, v)
}
func (vtprotoCodec) Name() string {
	return Name
}

func init() {
	encoding.RegisterCodecV2(&vtprotoCodec{
		fallback: encoding.GetCodecV2("proto"),
	})
}

func main() {
	// We use mmaped resources in most of the components so hardcode PanicOnFault to true. This allows us to recover (if we can e.g if queries
	// are temporarily accessing unmapped memory).
	debug.SetPanicOnFault(true)

	if os.Getenv("DEBUG") != "" {
		runtime.SetMutexProfileFraction(10)
		runtime.SetBlockProfileRate(10)
	}

	if v := os.Getenv("GOGC"); v != "" {
		n, err := strconv.ParseFloat(v, 64)
		if err != nil {
			n = 100
		}
		debug.SetGCPercent(int(n))
	} else {
		debug.SetGCPercent(DefaultGOGC)
		os.Setenv("GOGC", strconv.Itoa(DefaultGOGC))
	}

	app := extkingpin.NewApp(kingpin.New(filepath.Base(os.Args[0]), "A block storage based long-term storage for Prometheus.").Version(version.Print("thanos")))
	debugName := app.Flag("debug.name", "Name to add as prefix to log lines.").Hidden().String()
	logLevel := app.Flag("log.level", "Log filtering level.").
		Default("info").Enum("error", "warn", "info", "debug")
	logFormat := app.Flag("log.format", "Log format to use. Possible options: logfmt or json.").
		Default(logging.LogFormatLogfmt).Enum(logging.LogFormatLogfmt, logging.LogFormatJSON)
	tracingConfig := extkingpin.RegisterCommonTracingFlags(app)

	goMemLimitConf := goMemLimitConfig{}

	goMemLimitConf.registerFlag(app)

	registerSidecar(app)
	registerStore(app)
	registerQuery(app)
	registerRule(app)
	registerCompact(app)
	registerTools(app)
	registerReceive(app)
	registerQueryFrontend(app)

	cmd, setup := app.Parse()
	logger := logging.NewLogger(*logLevel, *logFormat, *debugName)

	if err := configureGoAutoMemLimit(goMemLimitConf); err != nil {
		level.Error(logger).Log("msg", "failed to configure Go runtime memory limits", "err", err)
		os.Exit(1)
	}

	// Running in container with limits but with empty/wrong value of GOMAXPROCS env var could lead to throttling by cpu
	// maxprocs will automate adjustment by using cgroups info about cpu limit if it set as value for runtime.GOMAXPROCS.
	undo, err := maxprocs.Set(maxprocs.Logger(func(template string, args ...interface{}) {
		level.Debug(logger).Log("msg", fmt.Sprintf(template, args...))
	}))
	defer undo()
	if err != nil {
		level.Warn(logger).Log("warn", errors.Wrapf(err, "failed to set GOMAXPROCS: %v", err))
	}

	metrics := prometheus.NewRegistry()
	metrics.MustRegister(
		versioncollector.NewCollector("thanos"),
		collectors.NewGoCollector(
			collectors.WithGoCollectorRuntimeMetrics(collectors.GoRuntimeMetricsRule{Matcher: regexp.MustCompile("/.*")}),
		),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	// Some packages still use default Register. Replace to have those metrics.
	prometheus.DefaultRegisterer = metrics

	var g run.Group
	var tracer opentracing.Tracer
	// Setup optional tracing.
	{
		var (
			ctx             = context.Background()
			closer          io.Closer
			confContentYaml []byte
		)

		confContentYaml, err = tracingConfig.Content()
		if err != nil {
			level.Error(logger).Log("msg", "getting tracing config failed", "err", err)
			os.Exit(1)
		}

		if len(confContentYaml) == 0 {
			tracer = client.NoopTracer()
		} else {
			tracer, closer, err = client.NewTracer(ctx, logger, metrics, confContentYaml)
			if err != nil {
				fmt.Fprintln(os.Stderr, errors.Wrapf(err, "tracing failed"))
				os.Exit(1)
			}
		}

		// This is bad, but Prometheus does not support any other tracer injections than just global one.
		// TODO(bplotka): Work with basictracer to handle gracefully tracker mismatches, and also with Prometheus to allow
		// tracer injection.
		opentracing.SetGlobalTracer(tracer)

		ctx, cancel := context.WithCancel(ctx)
		g.Add(func() error {
			<-ctx.Done()
			return ctx.Err()
		}, func(error) {
			if closer != nil {
				if err := closer.Close(); err != nil {
					level.Warn(logger).Log("msg", "closing tracer failed", "err", err)
				}
			}
			cancel()
		})
	}
	// Create a signal channel to dispatch reload events to sub-commands.
	reloadCh := make(chan struct{}, 1)

	if err := setup(&g, logger, metrics, tracer, reloadCh, *logLevel == "debug"); err != nil {
		// Use %+v for github.com/pkg/errors error to print with stack.
		level.Error(logger).Log("err", fmt.Sprintf("%+v", errors.Wrapf(err, "preparing %s command failed", cmd)))
		os.Exit(1)
	}

	// Listen for termination signals.
	{
		cancel := make(chan struct{})
		g.Add(func() error {
			return interrupt(logger, cancel)
		}, func(error) {
			close(cancel)
		})
	}

	// Listen for reload signals.
	{
		cancel := make(chan struct{})
		g.Add(func() error {
			return reload(logger, cancel, reloadCh)
		}, func(error) {
			close(cancel)
		})
	}

	if err := g.Run(); err != nil {
		// Use %+v for github.com/pkg/errors error to print with stack.
		level.Error(logger).Log("err", fmt.Sprintf("%+v", errors.Wrapf(err, "%s command failed", cmd)))
		os.Exit(1)
	}
	level.Info(logger).Log("msg", "exiting")
}

func interrupt(logger log.Logger, cancel <-chan struct{}) error {
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
	select {
	case s := <-c:
		level.Info(logger).Log("msg", "caught signal. Exiting.", "signal", s)
		return nil
	case <-cancel:
		return errors.New("canceled")
	}
}

func reload(logger log.Logger, cancel <-chan struct{}, r chan<- struct{}) error {
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGHUP)
	for {
		select {
		case s := <-c:
			level.Info(logger).Log("msg", "caught signal. Reloading.", "signal", s)
			select {
			case r <- struct{}{}:
				level.Info(logger).Log("msg", "reload dispatched.")
			default:
			}
		case <-cancel:
			return errors.New("canceled")
		}
	}
}

func getFlagsMap(flags []*kingpin.FlagModel) map[string]string {
	flagsMap := map[string]string{}

	// Exclude kingpin default flags to expose only Thanos ones.
	boilerplateFlags := kingpin.New("", "").Version("")

	for _, f := range flags {
		if boilerplateFlags.GetFlag(f.Name) != nil {
			continue
		}
		// Mask inline objstore flag which can have credentials.
		if f.Name == "objstore.config" || f.Name == "objstore.config-file" {
			flagsMap[f.Name] = "<REDACTED>"
			continue
		}
		flagsMap[f.Name] = f.Value.String()
	}

	return flagsMap
}

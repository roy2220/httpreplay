package main

import (
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/alexflint/go-arg"
)

func main() {
	debug := os.Getenv("DEBUG") == "1"
	exitSignal := make(chan os.Signal, 1)
	signal.Notify(exitSignal, syscall.SIGINT, syscall.SIGTERM)

	Main(os.Args[1:], os.Stdout, debug, os.Exit, exitSignal)
}

// Main is the entry point of the program.
func Main(
	rawArgs []string,
	output io.Writer,
	debug bool,
	exit func(int),
	exitSignal <-chan os.Signal,
) {
	logger := log.New(output, "", log.LstdFlags)

	httpRequester, err := newHttpRequester(mustParseHttpRequesterConfig(rawArgs, output, logger, debug, exit))
	if err != nil {
		logger.Printf("[FATAL] failed to create http requester: %v", err)
		exit(1)
	}
	defer httpRequester.Close()

	select {
	case <-httpRequester.Idleness():
	case <-exitSignal:
		logger.Printf("[INFO] http requester is stopping...")
	}
}

const (
	minFailureTapeBufferSize = 4 * 1024
	minSyncToDiskInterval    = 10 * time.Millisecond
)

var version = func() string {
	if buildInfo, ok := debug.ReadBuildInfo(); ok {
		if v := buildInfo.Main.Version; v != "" {
			return v
		}
	}
	return "unknown"
}()

type args struct {
	TapeFileName            string  `arg:"required,positional" placeholder:"TAPE-FILE" help:"path to the tape file containing HTTP requests"`
	MaxNumberOfHttpRequests int     `arg:"-n,--" placeholder:"NUM" help:"stop after NUM requests, no limit if less than 0" default:"-1"`
	QpsLimit                int     `arg:"-q,--" placeholder:"QPS" help:"maximum QPS, no limit if less than 1" default:"1"`
	ConcurrencyLimit        int     `arg:"-c,--" placeholder:"CONCURRENCY" help:"maximum concurrency, no limit if less than 1" default:"1"`
	RequestTimeout          float64 `arg:"-t,--" placeholder:"SECONDS" help:"request timeout in seconds (float), no timeout if less than or equal to 0.0" default:"10.0"`
	FollowRedirects         bool    `arg:"-f,--" help:"follow HTTP redirects" default:"false"`
	DryRun                  bool    `arg:"-d,--" help:"dry-run mode" default:"false"`
	DeliverySemantics       string  `arg:"--,--delivery-semantics" placeholder:"SEMANTICS" help:"delivery semantics for resuming after a crash (one of at-least-once, at-most-once)" default:"at-least-once"`
	FailureTapeBufferSize   int     `arg:"--,--failure-tape-buffer-size" placeholder:"BYTES" help:"buffer size of the failure tape file, in bytes; values too small will be raised" default:"16777216"`
	SyncToDiskInterval      float64 `arg:"--,--sync-to-disk-interval" placeholder:"SECONDS" help:"interval in seconds (float) for syncing the failure tape and position file to disk; values too small will be raised" default:"0.5"`
}

func (args) Version() string { return "httpreplay " + version }

func mustParseHttpRequesterConfig(
	rawArgs []string,
	output io.Writer,
	logger *log.Logger,
	debug bool,
	exit func(int),
) httpRequesterConfig {
	var args args
	var deliverySemantics deliverySemantics
	{
		parser, err := arg.NewParser(arg.Config{Exit: exit, Out: output}, &args)
		if err != nil {
			fmt.Fprintln(output, err)
			exit(1)
		}
		parser.MustParse(rawArgs)

		if args.QpsLimit < 1 && args.ConcurrencyLimit < 1 {
			parser.Fail("should limit at least one of qps or concurrency")
		}
		if math.IsNaN(args.RequestTimeout) {
			parser.Fail("request timeout should not be NaN")
		}
		switch args.DeliverySemantics {
		case "at-least-once":
			deliverySemantics = deliveryAtLeastOnce
		case "at-most-once":
			deliverySemantics = deliveryAtMostOnce
		default:
			parser.Fail("delivery semantics should be one of at-least-once or at-most-once")
		}
		if math.IsNaN(args.SyncToDiskInterval) {
			parser.Fail("sync to disk interval should not be NaN")
		}
	}

	return httpRequesterConfig{
		TapeFileName:            args.TapeFileName,
		MaxNumberOfHttpRequests: args.MaxNumberOfHttpRequests,
		QpsLimit:                args.QpsLimit,
		ConcurrencyLimit:        args.ConcurrencyLimit,
		RequestTimeout:          max(time.Duration(args.RequestTimeout*float64(time.Second)), 0),
		FollowRedirects:         args.FollowRedirects,
		DryRun:                  args.DryRun,
		DeliverySemantics:       deliverySemantics,
		FailureTapeBufferSize:   max(args.FailureTapeBufferSize, minFailureTapeBufferSize),
		SyncToDiskInterval:      max(time.Duration(args.SyncToDiskInterval*float64(time.Second)), minSyncToDiskInterval),
		DefaultUserAgent:        []string{"httpreplay/" + strings.TrimPrefix(version, "v")},
		Logger:                  logger,
		Debug:                   debug,
	}
}

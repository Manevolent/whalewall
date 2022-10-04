package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

var (
	clear          bool
	dataDir        string
	debugLogs      bool
	logPath        string
	displayVersion bool
)

func init() {
	flag.BoolVar(&clear, "clear", false, "remove all firewall rules created by whalewall")
	flag.StringVar(&dataDir, "d", ".", "directory to store state in")
	flag.BoolVar(&debugLogs, "debug", false, "enable debug logging")
	flag.StringVar(&logPath, "l", "stdout", "path to log to")
	flag.BoolVar(&displayVersion, "version", false, "print version and build information and exit")
}

func main() {
	flag.Parse()

	info, ok := debug.ReadBuildInfo()
	if !ok {
		log.Fatal("build information not found")
	}

	if version == "" {
		version = "devel"
	}
	if displayVersion {
		printVersionInfo(info)
		os.Exit(0)
	}

	// build logger
	logCfg := zap.NewProductionConfig()
	logCfg.OutputPaths = []string{logPath}
	if debugLogs {
		logCfg.Level.SetLevel(zap.DebugLevel)
	}
	logCfg.EncoderConfig.TimeKey = "time"
	logCfg.EncoderConfig.EncodeTime = zapcore.RFC3339NanoTimeEncoder
	logCfg.DisableCaller = true

	logger, err := logCfg.Build()
	if err != nil {
		log.Fatalf("error creating logger: %v", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	r := newRuleManager(logger)

	// remove all created firewall rules if the use asked to clear
	if clear {
		logger.Info("clearing rules")
		if err := r.clear(ctx, dataDir); err != nil {
			logger.Fatal("error clearing rules", zap.Error(err))
		}
		os.Exit(0)
	}

	// log current version/commit
	versionFields := []zap.Field{
		zap.String("version", version),
	}
	for _, buildSetting := range info.Settings {
		if buildSetting.Key == "vcs.revision" {
			versionFields = append(versionFields, zap.String("commit", buildSetting.Value))
			break
		}
	}
	logger.Info("starting whalewall", versionFields...)

	// start managing firewall rules
	if err = r.start(ctx, dataDir); err != nil {
		logger.Fatal("error starting", zap.Error(err))
	}

	<-ctx.Done()
	logger.Info("shutting down")
	r.stop()
}

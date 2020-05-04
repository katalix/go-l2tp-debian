package main

import (
	"flag"
	"fmt"
	stdlog "log"
	"os"
	"os/signal"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/katalix/go-l2tp/config"
	"github.com/katalix/go-l2tp/l2tp"
	"golang.org/x/sys/unix"
)

type application struct {
	config   *config.Config
	logger   log.Logger
	l2tpCtx  *l2tp.Context
	sigChan  chan os.Signal
	shutdown bool
}

func newApplication(configPath string, verbose, nullDataplane bool) (*application, error) {

	sigChan := make(chan os.Signal)
	signal.Notify(sigChan, unix.SIGINT, unix.SIGTERM)

	dataplane := l2tp.LinuxNetlinkDataPlane

	config, err := config.LoadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load configuration: %v", err)
	}

	logger := log.NewLogfmtLogger(os.Stderr)
	if verbose {
		logger = level.NewFilter(logger, level.AllowDebug())
	} else {
		logger = level.NewFilter(logger, level.AllowInfo())
	}

	if nullDataplane {
		dataplane = nil
	}

	l2tpCtx, err := l2tp.NewContext(dataplane, logger)
	if err != nil {
		return nil, fmt.Errorf("failed to create L2TP context: %v", err)
	}

	return &application{
		config:  config,
		logger:  logger,
		l2tpCtx: l2tpCtx,
		sigChan: sigChan,
	}, nil
}

func (app *application) HandleEvent(event interface{}) {
	// TODO
}

func (app *application) run() int {

	defer app.l2tpCtx.Close()

	// Listen for L2TP events
	app.l2tpCtx.RegisterEventHandler(app)

	// Instantiate tunnels and sessions from the config file
	for _, tcfg := range app.config.Tunnels {
		tunl, err := app.l2tpCtx.NewDynamicTunnel(tcfg.Name, tcfg.Config)
		if err != nil {
			level.Error(app.logger).Log(
				"message", "failed to create tunnel",
				"tunnel_name", tcfg.Name,
				"error", err)
			return 1
		}

		for _, scfg := range tcfg.Sessions {
			_, err := tunl.NewSession(scfg.Name, scfg.Config)
			if err != nil {
				level.Error(app.logger).Log(
					"message", "failed to create session",
					"session_name", scfg.Name,
					"error", err)
				return 1
			}
		}
	}

	for !app.shutdown {
		select {
		case <-app.sigChan:
			level.Info(app.logger).Log("message", "received signal, shutting down")
			app.shutdown = true
		}
	}

	return 0
}

func main() {
	cfgPathPtr := flag.String("config", "/etc/kl2tpd/kl2tpd.toml", "specify configuration file path")
	verbosePtr := flag.Bool("verbose", false, "toggle verbose log output")
	nullDataPlanePtr := flag.Bool("null", false, "toggle null data plane")
	flag.Parse()

	app, err := newApplication(*cfgPathPtr, *verbosePtr, *nullDataPlanePtr)
	if err != nil {
		stdlog.Fatalf("failed to instantiate application: %v", err)
	}

	os.Exit(app.run())
}

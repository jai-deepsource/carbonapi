package main

import (
	"expvar"
	"flag"
	"log"
	"net/http"
	"net/http/pprof"
	"sync"
	"time"

	"github.com/facebookgo/grace/gracehttp"
	"github.com/gorilla/handlers"
	"github.com/lomik/zapwriter"
	"github.com/rs/dnscache"
	"go.uber.org/zap"

	"github.com/go-graphite/carbonapi/cmd/carbonapi/config"
	carbonapiHttp "github.com/go-graphite/carbonapi/cmd/carbonapi/http"
)

// BuildVersion is provided to be overridden at build time. Eg. go build -ldflags -X 'main.BuildVersion=...'
var BuildVersion = "(development build)"

func main() {
	err := zapwriter.ApplyConfig([]zapwriter.Config{config.DefaultLoggerConfig})
	if err != nil {
		log.Fatal("Failed to initialize logger with default configuration")
	}
	logger := zapwriter.Logger("main")

	configPath := flag.String("config", "", "Path to the `config file`.")
	envPrefix := flag.String("envprefix", "CARBONAPI", "Prefix for environment variables override")
	if *envPrefix == "(empty)" {
		*envPrefix = ""
	}
	if *envPrefix == "" {
		logger.Warn("empty prefix is not recommended due to possible collisions with OS environment variables")
	}
	flag.Parse()
	config.SetUpViper(logger, configPath, *envPrefix)
	config.SetUpConfigUpstreams(logger)
	config.SetUpConfig(logger, BuildVersion)
	carbonapiHttp.SetupMetrics(logger)
	setupGraphiteMetrics(logger)

	if config.Config.UseCachingDNSResolver {
		config.Config.Resolver = &dnscache.Resolver{}

		// Periodically refresh cache
		go func() {
			ticker := time.NewTicker(config.Config.CachingDNSRefreshTime)
			defer ticker.Stop()
			for range ticker.C {
				config.Config.Resolver.Refresh(true)
			}
		}()
	}

	config.Config.ZipperInstance = newZipper(carbonapiHttp.ZipperStats, &config.Config.Upstreams, config.Config.IgnoreClientTimeout, zapwriter.Logger("zipper"))

	wg := sync.WaitGroup{}
	if config.Config.Expvar.Enabled {
		if config.Config.Expvar.Listen != "" || config.Config.Expvar.Listen != config.Config.Listeners[0].Address {
			r := http.NewServeMux()
			r.HandleFunc(config.Config.Prefix+"/debug/vars", expvar.Handler().ServeHTTP)
			if config.Config.Expvar.PProfEnabled {
				r.HandleFunc(config.Config.Prefix+"/debug/pprof/", pprof.Index)
				r.HandleFunc(config.Config.Prefix+"/debug/pprof/cmdline", pprof.Cmdline)
				r.HandleFunc(config.Config.Prefix+"/debug/pprof/profile", pprof.Profile)
				r.HandleFunc(config.Config.Prefix+"/debug/pprof/symbol", pprof.Symbol)
				r.HandleFunc(config.Config.Prefix+"/debug/pprof/trace", pprof.Trace)
			}

			handler := handlers.CompressHandler(r)
			handler = handlers.CORS()(handler)
			handler = handlers.ProxyHeaders(handler)

			logger.Info("expvar handler will listen on a separate address/port",
				zap.String("expvar_listen", config.Config.Expvar.Listen),
				zap.Bool("pprof_enabled", config.Config.Expvar.PProfEnabled),
			)

			listener := config.Listener{
				Address: config.Config.Expvar.Listen,
			}
			wg.Add(1)
			go func(listen config.Listener) {
				err = gracehttp.Serve(&http.Server{
					Addr:    listen.Address,
					Handler: handler,
				})

				if err != nil {
					logger.Fatal("failed to start http server",
						zap.Error(err),
					)
				}

				wg.Done()
			}(listener)
		}
	}

	r := carbonapiHttp.InitHandlers(config.Config.HeadersToPass, config.Config.HeadersToLog)
	handler := handlers.CompressHandler(r)
	handler = handlers.CORS()(handler)
	handler = handlers.ProxyHeaders(handler)

	for _, listener := range config.Config.Listeners {
		wg.Add(1)
		go func(listen config.Listener) {
			err = gracehttp.Serve(&http.Server{
				Addr:    listen.Address,
				Handler: handler,
			})

			if err != nil {
				logger.Fatal("failed to start http server",
					zap.Error(err),
				)
			}

			wg.Done()
		}(listener)
	}

	wg.Wait()
}
